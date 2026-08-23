package relayer

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Pool 讓 N 個 goroutine 共用同一個 Worker、對同一條 queue 領工作。它只管三件事：起幾個、收工怎麼收、誰出了意外。
// 「每一筆 intent 只送一次」不是它的責任：那是 queue 的 lease 加上 Worker 每一步冪等給的，一個 worker 跟八個 worker 一樣成立。
//
// 分工用 pull 不用 push：每個 goroutine 自己 Lease，沒有一個中央 dispatcher 先領一批再塞進 channel 分給大家
// （Go by Example 那種 jobs channel 的 worker pool，https://gobyexample.com/worker-pools）。queue 本身就是那條 channel：
// 在 channel 裡排隊的 job 對 queue 來說是「已領走」，lease 的時鐘在走、別的程序看不到它；dispatcher 一死，
// 一整批 job 要等 lease 過期才回得來。每個 worker 自己領，手上最多一份，慢的 worker 自然少領，快的多領。
//
// 收工分兩段，照 Kubernetes 結束一個 pod 的方式（先 TERM、等 grace period、再 KILL，
// https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination）：
//
//  1. ctx 結束：不再 Lease 新的 job，但在手上的 job 繼續做完。這一段用另一個 workCtx，不跟著 ctx 一起被取消。
//     正在 Send 的交易被取消，回來的是「不知道送出去了沒」，照 settling 那一格的規則（見 Worker.process）那筆 intent 會卡在 settling、最後送審；
//     多等幾秒讓它送完，比製造一筆 needs_review 便宜。
//  2. DrainTimeout 到了還沒做完：取消 workCtx，放棄在手上的 job。被打斷的 Send 會以 retry 收場（Nack），
//     job 留在 queue 裡，intent 停在 settling，交給重來的 worker 照狀態處理。Stats.Abandoned 記下放棄了幾份。
//     放棄是有意識的，因為 grace period 過後程序反正會被 KILL；與其被 KILL 在不知道哪一行，不如自己挑在哪一步停。
//
// 跟 net/http 的 Server.Shutdown 同一個形狀（https://pkg.go.dev/net/http#Server.Shutdown）：
// 先關掉入口、等在跑的做完、ctx 到期就不等了。
type Pool struct {
	w   *Worker
	cfg PoolConfig
}

// PoolConfig 是 Pool 的三個數字。
type PoolConfig struct {
	// Size 是 goroutine 數，也就是同時最多幾份 job 在手上。它不是同時往 RPC 送幾筆：那是 Throttle 的 MaxInFlight。這裡預設 4。
	Size int
	// Idle 是 queue 空的時候睡多久再看一次。這裡預設 1 秒。
	Idle time.Duration
	// DrainTimeout 是收工時等在手上的 job 最多等多久。要比一次 Send 的正常耗時長、比部署系統的 grace period 短
	//（Kubernetes 預設 30 秒）。這裡預設 20 秒。
	DrainTimeout time.Duration
}

// DefaultPoolConfig：4 個 worker、空了睡 1 秒、收工最多等 20 秒。
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{Size: 4, Idle: time.Second, DrainTimeout: 20 * time.Second}
}

// Stats 是一次 Run 的統計。前五個照 Outcome 分；Errors 是 process 回錯的次數（job 已 Nack）；
// Panics 是 worker goroutine 從 panic 裡爬起來的次數；Abandoned 是收工逾時時還在手上、被放棄的 job 數。
//
// Poison 是這一輪停止重試的 job 數。它跟 Retry 要分開看：Retry 高只是這批工作暫時做不動，
// Poison 只要不是零就代表有東西被放棄了，該有人去看。
type Stats struct {
	Sent, Noop, Retry, Review uint64
	Poison                    uint64
	Errors, Panics            uint64
	Abandoned                 int64
}

// NewPool 建立一個 Pool。所有 goroutine 共用同一個 Worker：Worker 本身沒有狀態，只有四個依賴，而那四個依賴各自負責自己的鎖。
func NewPool(w *Worker, cfg PoolConfig) *Pool {
	return &Pool{w: w, cfg: cfg}
}

// Run 起 Size 個 goroutine，跑到 ctx 結束，然後照 package 註解的兩段收工，最後回傳統計。
// 它不回傳 error：queue 暫時壞掉算在 Stats.Errors 裡、睡 Idle 再試，因為 worker pool 的工作就是一直在那裡。
func (p *Pool) Run(ctx context.Context) Stats {
	// workCtx 帶著 ctx 的值、但不跟著 ctx 被取消：ctx 結束只代表「不要再領新的」，不代表「手上的不要做」。
	workCtx, hardStop := context.WithCancel(context.WithoutCancel(ctx))
	defer hardStop()

	var st stats
	var busy atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < p.cfg.Size; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if p.runOne(workCtx, &st, &busy) {
					continue
				}
				select {
				case <-ctx.Done():
				case <-time.After(p.cfg.Idle):
				}
			}
		}()
	}

	<-ctx.Done()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(p.cfg.DrainTimeout):
		st.abandoned.Store(busy.Load())
		hardStop()
		<-done
	}
	return st.snapshot()
}

// runOne 是一個 goroutine 的一圈：RunOnce 一次、記統計。回傳 false 代表 queue 是空的（或壞的），該睡一下。
//
// panic 在這裡接住：Go 的 panic 會帶走整個程序，一份壞掉的 job（store 回了不該回的東西、Sender 的 bug）不該讓另外 N-1 個
// worker 陪葬。接住之後什麼都不做：沒有 Ack、沒有 Nack，job 留在 lease 裡，過期後自然回來，重來的 worker 照 intent
// 現在的狀態處理。這裡絕對不能 Ack：panic 的那一步不知道做到哪，Ack 等於把這份工作當做完。
func (p *Pool) runOne(ctx context.Context, st *stats, busy *atomic.Int64) (ok bool) {
	busy.Add(1)
	defer busy.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			st.panics.Add(1)
			ok = true
		}
	}()
	rep, ok, err := p.w.RunOnce(ctx)
	if err != nil {
		st.errors.Add(1)
	}
	if !ok {
		return false
	}
	switch rep.Outcome {
	case OutcomeSent:
		st.sent.Add(1)
	case OutcomeNoop:
		st.noop.Add(1)
	case OutcomeRetry:
		st.retry.Add(1)
	case OutcomeReview:
		st.review.Add(1)
	case OutcomePoison:
		st.poison.Add(1)
	}
	return true
}

// stats 是 Stats 的可併發版本，Run 結束時 snapshot 成 Stats。
type stats struct {
	sent, noop, retry, review atomic.Uint64
	poison                    atomic.Uint64
	errors, panics            atomic.Uint64
	abandoned                 atomic.Int64
}

func (s *stats) snapshot() Stats {
	return Stats{
		Sent: s.sent.Load(), Noop: s.noop.Load(), Retry: s.retry.Load(), Review: s.review.Load(),
		Poison: s.poison.Load(),
		Errors: s.errors.Load(), Panics: s.panics.Load(), Abandoned: s.abandoned.Load(),
	}
}

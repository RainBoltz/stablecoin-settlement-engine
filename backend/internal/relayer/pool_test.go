package relayer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
)

// refusing 是一個永遠給不出名額的 Limiter。
type refusing struct{}

func (refusing) Acquire(context.Context) error { return errors.New("no slot") }
func (refusing) Release()                      {}

// TestWorker_ThrottledJobGoesBackUntouched：拿不到名額的 job 原封不動回 queue：intent 還在 authorized、帳上沒有 hold、
// Send 沒被叫。限流擋在任何副作用之前，所以被擋住的那一刻放手不用收拾任何東西；名額有了，下一次交付照常走完。
func TestWorker_ThrottledJobGoesBackUntouched(t *testing.T) {
	wd := newWorld(t)
	ctx := context.Background()
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.w.limiter = refusing{}

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeRetry || rep.Detail != "throttled: no slot" {
		t.Fatalf("report: %s", rep)
	}
	if got := wd.state(t, it.ID); got.State != intent.StateAuthorized || got.Version != 2 {
		t.Fatalf("intent should be untouched: %s v%d", got.State, got.Version)
	}
	if _, err := wd.journal.Get(ctx, "pi_0001/hold"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("no hold expected, got %v", err)
	}
	if wd.sends["pi_0001"] != 0 || wd.queueLen(t) != 1 {
		t.Fatalf("sends=%d queue=%d", wd.sends["pi_0001"], wd.queueLen(t))
	}

	wd.w.limiter = Unlimited{}
	wd.tick(DefaultConfig().RetryAfter)
	if rep := wd.runOnce(t); rep.Outcome != OutcomeSent || rep.Attempt != 2 {
		t.Fatalf("second delivery: %s", rep)
	}
}

// TestWorker_ThrottleWaitIsBoundedByTheLease：名額被別人占著，worker 最多等到自己的 lease 結束就放手（retry），
// 不會抱著一份別人已經可以領走的 job 一直等。
func TestWorker_ThrottleWaitIsBoundedByTheLease(t *testing.T) {
	wd := newWorld(t)
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	th := NewThrottle(1, 0, 0)
	if err := th.Acquire(context.Background()); err != nil { // 唯一的名額被別人拿走了
		t.Fatal(err)
	}
	wd.w.limiter = th
	wd.w.cfg = Config{Lease: 30 * time.Millisecond, RetryAfter: 5 * time.Second, StuckAfter: 5 * time.Minute}

	start := time.Now()
	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeRetry || rep.Detail != "throttled: context deadline exceeded" {
		t.Fatalf("report: %s", rep)
	}
	if waited := time.Since(start); waited < 30*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("waited %s, want about the lease", waited)
	}
	if got := wd.state(t, it.ID); got.State != intent.StateAuthorized {
		t.Fatalf("intent should be untouched: %s", got.State)
	}
}

// blockingSender 把每一次 Send 卡在 release 關掉之前（或 ctx 結束），並回報進來的是誰。tx hash 從 intent id 推，讓輸出可預測。
func blockingSender(entered chan<- string, release <-chan struct{}) Sender {
	return SenderFunc(func(ctx context.Context, it *intent.Intent) (string, error) {
		entered <- it.ID
		select {
		case <-release:
			return "0x" + it.ID[3:], nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
}

// TestPool_DrainsInFlightJobsBeforeReturning：ctx 結束時兩個 worker 都卡在 Send。Run 不會馬上回來：它不再領新的 job，
// 等那兩筆送完、Ack 之後才回。queue 裡剩下的四份沒有被任何人領過。
func TestPool_DrainsInFlightJobsBeforeReturning(t *testing.T) {
	wd := newWorld(t)
	for i := 1; i <= 6; i++ {
		wd.enqueue(t, wd.newIntent(t, fmt.Sprintf("pi_%04d", i), intent.StateAuthorized))
	}
	entered := make(chan string, 6)
	release := make(chan struct{})
	wd.w.sender = blockingSender(entered, release)
	p := NewPool(wd.w, PoolConfig{Size: 2, Idle: time.Millisecond, DrainTimeout: 5 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Stats, 1)
	go func() { done <- p.Run(ctx) }()
	<-entered
	<-entered
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned while two sends were still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	st := <-done
	if st.Sent != 2 || st.Abandoned != 0 || st.Retry != 0 {
		t.Fatalf("stats: %+v", st)
	}
	if wd.queueLen(t) != 4 {
		t.Fatalf("queue: want 4 left, got %d", wd.queueLen(t))
	}
	d, ok, _ := wd.q.Lease(context.Background(), wd.now(), time.Second)
	if !ok || d.Attempt != 1 {
		t.Fatalf("the remaining jobs should never have been leased: ok=%v attempt=%d", ok, d.Attempt)
	}
	for _, id := range []string{"pi_0001", "pi_0002"} {
		if got := wd.state(t, id); got.State != intent.StateConfirming {
			t.Fatalf("%s: %s", id, got.State)
		}
	}
}

// TestPool_DrainTimeoutAbandonsInFlightJobs：Send 永遠不回來，DrainTimeout 到了 Pool 取消 workCtx、放棄在手上的兩份。
// 被打斷的 Send 以 retry 收場：job 還在 queue、intent 停在 settling 帶著 hold，交給重來的 worker 照狀態處理。
func TestPool_DrainTimeoutAbandonsInFlightJobs(t *testing.T) {
	wd := newWorld(t)
	for i := 1; i <= 3; i++ {
		wd.enqueue(t, wd.newIntent(t, fmt.Sprintf("pi_%04d", i), intent.StateAuthorized))
	}
	entered := make(chan string, 3)
	wd.w.sender = blockingSender(entered, nil) // nil channel：只有 ctx 結束才會回來
	p := NewPool(wd.w, PoolConfig{Size: 2, Idle: time.Millisecond, DrainTimeout: 30 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Stats, 1)
	go func() { done <- p.Run(ctx) }()
	<-entered
	<-entered
	cancel()
	st := <-done
	if st.Abandoned != 2 || st.Retry != 2 || st.Sent != 0 {
		t.Fatalf("stats: %+v", st)
	}
	if wd.queueLen(t) != 3 {
		t.Fatalf("queue: want 3 left, got %d", wd.queueLen(t))
	}
	settling := 0
	for i := 1; i <= 3; i++ {
		if got := wd.state(t, fmt.Sprintf("pi_%04d", i)); got.State == intent.StateSettling && got.TxHash == "" {
			settling++
		}
	}
	if settling != 2 {
		t.Fatalf("want 2 intents left in settling without tx hash, got %d", settling)
	}
}

// TestPool_PanicIsContainedAndTheJobComesBack：一份 job 的 Send panic 了，其他 job 照常處理、程序不會死。
// panic 的那份既沒 Ack 也沒 Nack，lease 過期後回到 queue；intent 停在 settling，之後照 settling 那一格的規則走。
func TestPool_PanicIsContainedAndTheJobComesBack(t *testing.T) {
	wd := newWorld(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		wd.enqueue(t, wd.newIntent(t, fmt.Sprintf("pi_%04d", i), intent.StateAuthorized))
	}
	wd.w.sender = SenderFunc(func(_ context.Context, it *intent.Intent) (string, error) {
		if it.ID == "pi_0002" {
			panic("sender bug")
		}
		return "0x" + it.ID[3:], nil
	})
	runCtx, cancel := context.WithCancel(ctx)
	var sent atomic.Int64
	wd.w.observe = func(r Report) {
		if r.Outcome == OutcomeSent && sent.Add(1) == 2 {
			cancel()
		}
	}
	st := NewPool(wd.w, PoolConfig{Size: 1, Idle: time.Millisecond, DrainTimeout: time.Second}).Run(runCtx)
	if st.Sent != 2 || st.Panics != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if wd.queueLen(t) != 1 {
		t.Fatalf("queue: want the panicked job to remain, got %d", wd.queueLen(t))
	}
	if _, ok, _ := wd.q.Lease(ctx, wd.now(), time.Second); ok {
		t.Fatal("panicked job should still be leased, not acked or nacked")
	}
	wd.tick(DefaultConfig().Lease)
	d, ok, _ := wd.q.Lease(ctx, wd.now(), time.Second)
	if !ok || d.Job.IntentID != "pi_0002" || d.Attempt != 2 {
		t.Fatalf("after lease expiry: ok=%v job=%+v", ok, d)
	}
	if got := wd.state(t, "pi_0002"); got.State != intent.StateSettling {
		t.Fatalf("pi_0002: %s", got.State)
	}
}

// TestPool_ManyWorkersThrottledSendEachIntentOnce：八個 worker、名額三個、五十筆 intent。每一筆恰好 Send 一次、
// 同一時刻在 Send 裡的從來不超過三個、最後全部 confirming。worker 數與 RPC 連線數是兩個數字。
func TestPool_ManyWorkersThrottledSendEachIntentOnce(t *testing.T) {
	wd := newWorld(t)
	ctx := context.Background()
	const n = 50
	for i := 1; i <= n; i++ {
		wd.enqueue(t, wd.newIntent(t, fmt.Sprintf("pi_%04d", i), intent.StateAuthorized))
	}
	var inFlight, peak, sends atomic.Int64
	wd.w.sender = SenderFunc(func(_ context.Context, it *intent.Intent) (string, error) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		sends.Add(1)
		return "0x" + it.ID[3:], nil
	})
	wd.w.limiter = NewThrottle(3, 0, 0)
	runCtx, cancel := context.WithCancel(ctx)
	var done atomic.Int64
	wd.w.observe = func(r Report) {
		if r.Outcome == OutcomeSent && done.Add(1) == n {
			cancel()
		}
	}
	st := NewPool(wd.w, PoolConfig{Size: 8, Idle: time.Millisecond, DrainTimeout: 5 * time.Second}).Run(runCtx)
	if st.Sent != n || st.Retry != 0 || st.Panics != 0 || st.Abandoned != 0 {
		t.Fatalf("stats: %+v", st)
	}
	if sends.Load() != n || peak.Load() > 3 {
		t.Fatalf("sends=%d peak in flight=%d", sends.Load(), peak.Load())
	}
	for i := 1; i <= n; i++ {
		if got := wd.state(t, fmt.Sprintf("pi_%04d", i)); got.State != intent.StateConfirming {
			t.Fatalf("%s: %s", got.ID, got.State)
		}
	}
	holds := 0
	_ = wd.journal.Scan(ctx, func(ledger.Entry) error { holds++; return nil })
	if holds != n || wd.queueLen(t) != 0 {
		t.Fatalf("holds=%d queue=%d", holds, wd.queueLen(t))
	}
}

// TestPool_IdleWorkersStopPromptly：queue 是空的、worker 都在睡 Idle，ctx 一結束 Run 就回來，不會睡滿一整個 Idle。
func TestPool_IdleWorkersStopPromptly(t *testing.T) {
	wd := newWorld(t)
	p := NewPool(wd.w, PoolConfig{Size: 4, Idle: time.Hour, DrainTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Stats, 1)
	go func() { done <- p.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case st := <-done:
		if st != (Stats{}) {
			t.Fatalf("stats: %+v", st)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

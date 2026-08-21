// Package relayer 是把 authorized 的 intent 送上鏈的那個元件：從 queue 領 job、讀 intent、推到 settling、
// 在 ledger 記 hold、廣播、推到 confirming、Ack。它是狀態機裡 settling 這一格的主人。
//
// 業界對 relayer 這個詞的用法（例如 OpenZeppelin Defender 的 Relayers，
// https://docs.openzeppelin.com/defender/module/relayers）大致是「替你保管送交易的錢包、排隊、簽名、送出、盯到上鏈」的服務。
// 這裡的 relayer 也是那個意思，只是它從第一天就長在 Payment Intent 狀態機與 ledger 旁邊：它不自己記「送到哪了」，
// intent 的 State 與 ledger 的 hold 就是它的紀錄。
//
// 整個 package 圍著一件事設計：queue 是 at-least-once 的，同一份 job 會被交付不只一次，
// 而且 worker 可能在任何一步之間死掉。所以每一步都是冪等的（狀態機重放 no-op、ledger 同 ID no-op、存檔 CAS），
// 而且順序固定：先寫 settling（CAS 成功才算領到這筆 intent）、再記 hold（帳上先卡位）、才廣播、最後才 Ack。
// Ack 永遠是最後一步：中間任何一步死掉，lease 過期後 job 會再回來，重來的那個 worker 照 intent 現在的狀態決定要做什麼
// （見 Worker.process）。
//
// 今天最保守的一條規則在 settling 那一格：重來的 worker 看到 intent 已經在 settling、卻沒有 tx hash，
// 它不知道上一次的交易送出去了沒（可能死在廣播前，也可能死在廣播後、寫回 confirming 前）。再送一次可能付兩次，
// 所以它只做兩件事：還年輕就 Nack 等一等（上一個 worker 可能只是慢），超過 StuckAfter 就推到 needs_review 讓人看。
// 代價是 RPC 抖一下、或 worker 慢一點，就會有 intent 送審。要讓 relayer 敢在 settling 重送，得先能證明上一次的交易到底
// 有沒有出門，那需要 nonce 與每一次嘗試的紀錄，之後會討論。
//
// 沒有真的鏈：Sender 是介面，Example 與測試用 fake。多個 worker 怎麼共用一條 queue、怎麼收工、怎麼不把 RPC 打爆，
// 見 Pool 與 Throttle；nonce 怎麼排、送出失敗怎麼分類、放棄的 job 去哪，之後會討論。
package relayer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
)

// Sender 把一筆 intent 的付款送上鏈、回傳 tx hash。今天只有測試用的 fake；接真的鏈時，chain adapter 實作這個介面。
//
// 回傳 error 的意思是「這次沒送成」，但 relayer 不敢假設「沒送成等於沒送出去」：RPC 逾時的那筆可能已經被節點收下了。
// 這就是為什麼 Send 失敗之後 relayer 只 Nack，不自己再送一次。
type Sender interface {
	Send(ctx context.Context, it *intent.Intent) (txHash string, err error)
}

// SenderFunc 讓一個函式當 Sender 用。
type SenderFunc func(ctx context.Context, it *intent.Intent) (string, error)

// Send 實作 Sender。
func (f SenderFunc) Send(ctx context.Context, it *intent.Intent) (string, error) { return f(ctx, it) }

// Outcome 是 worker 處理一份 job 的四種結果。哪一種對應 Ack、哪一種對應 Nack，見 RunOnce。
type Outcome string

const (
	// OutcomeSent：走完整條路，intent 現在是 confirming、帳上有 hold、tx 已送出。Ack。
	OutcomeSent Outcome = "sent"
	// OutcomeNoop：這份 job 沒有事可做（intent 已經過了 settling、或已經 terminal），多半是重送。Ack。
	OutcomeNoop Outcome = "no-op"
	// OutcomeRetry：現在做不了（還沒 authorized、輸了 CAS、送出失敗、上一個 worker 可能還在送）。Nack，等一下再來。
	OutcomeRetry Outcome = "retry"
	// OutcomeReview：intent 卡在 settling 太久又沒有 tx hash，交易下落不明，推到 needs_review 讓人看。Ack。
	OutcomeReview Outcome = "needs_review"
)

// Report 是處理一份 job 的結果，Run 會交給 observer、Example 會印出來。
type Report struct {
	Job     queue.Job
	Attempt uint64
	Outcome Outcome
	// TxHash 只有 OutcomeSent 有。
	TxHash string
	// Detail 是給人看的一句話：為什麼 retry、為什麼 no-op。
	Detail string
}

// String 用固定格式印一行：job、第幾次交付、結果、細節。
func (r Report) String() string {
	s := fmt.Sprintf("%-16s #%-2d %-12s", r.Job.ID, r.Attempt, r.Outcome)
	if r.TxHash != "" {
		s += " tx " + r.TxHash
	}
	if r.Detail != "" {
		s += " (" + r.Detail + ")"
	}
	return s
}

// Config 是三個時間常數。
type Config struct {
	// Lease 是一份 job 被領走後最多可以隱形多久。要比「讀 intent、寫兩次、送一次交易」的正常耗時長很多，
	// 不然慢一點的 worker 會被當成死了。這裡設 30 秒。
	Lease time.Duration
	// RetryAfter 是 Nack 之後多久再交付。這裡設 5 秒。
	RetryAfter time.Duration
	// StuckAfter 是 intent 在 settling 沒有 tx hash 多久之後算「下落不明」、送去 needs_review。
	// 要比 Lease 長：lease 過期只代表上一個 worker 沒回報，不代表它沒在送。這裡設 5 分鐘。
	StuckAfter time.Duration
}

// DefaultConfig：30 秒 lease、5 秒後重試、5 分鐘算卡住。
func DefaultConfig() Config {
	return Config{Lease: 30 * time.Second, RetryAfter: 5 * time.Second, StuckAfter: 5 * time.Minute}
}

// Worker 是一個處理迴圈：領一份、做一份。它自己沒有狀態，所以多個 goroutine 可以共用同一個 Worker 對同一條 queue 領工作，
// Pool 就是這樣做的。
type Worker struct {
	queue   queue.Queue
	intents intent.Store
	journal ledger.Journal
	sender  Sender
	limiter Limiter
	cfg     Config
	now     func() time.Time
	observe func(Report)
}

// Option 調整 Worker 的預設值。
type Option func(*Worker)

// WithClock 換掉時鐘，測試用。
func WithClock(now func() time.Time) Option { return func(w *Worker) { w.now = now } }

// WithConfig 換掉三個時間常數。
func WithConfig(c Config) Option { return func(w *Worker) { w.cfg = c } }

// WithObserver 讓 Run 每處理完一份 job 就回報一次（印 log、算指標都從這裡接）。
func WithObserver(f func(Report)) Option { return func(w *Worker) { w.observe = f } }

// WithLimiter 換掉限流器（預設 Unlimited）。多個 worker 共用同一個 Worker 時，它們也就共用同一個 Limiter，
// 這正是要的：名額限的是整個 pool 對外的總量，不是每個 worker 各自的。
func WithLimiter(l Limiter) Option { return func(w *Worker) { w.limiter = l } }

// New 建立一個 Worker。四個依賴都是介面：queue、intent store、journal、sender，今天全是記憶體版或 fake。
func New(q queue.Queue, intents intent.Store, journal ledger.Journal, sender Sender, opts ...Option) *Worker {
	w := &Worker{queue: q, intents: intents, journal: journal, sender: sender, limiter: Unlimited{}, cfg: DefaultConfig(), now: time.Now}
	for _, o := range opts {
		o(w)
	}
	return w
}

// RunOnce 領一份 job、處理、然後 Ack 或 Nack。queue 空的回傳 ok=false。
//
// 這裡是 Outcome 對應到 queue 動作的唯一地方：sent、no-op、needs_review 三種都 Ack（這份工作結束了）；
// retry 才 Nack。process 回錯（store 或 journal 出問題）也 Nack：錯誤本身回給呼叫端，job 留在 queue 裡等下一次。
// Ack 或 Nack 撞到 ErrStaleReceipt 代表我們做太慢、lease 已經被別人接走：那就什麼都不做，
// 我們寫進 store 與 journal 的東西都是冪等的，接手的 worker 會看到、也會安靜地重放。
func (w *Worker) RunOnce(ctx context.Context) (Report, bool, error) {
	d, ok, err := w.queue.Lease(ctx, w.now(), w.cfg.Lease)
	if err != nil || !ok {
		return Report{}, false, err
	}
	rep, perr := w.process(ctx, d)
	rep.Job, rep.Attempt = d.Job, d.Attempt
	if perr != nil {
		rep.Outcome, rep.Detail = OutcomeRetry, "error: "+perr.Error()
	}
	var qerr error
	if rep.Outcome == OutcomeRetry {
		qerr = w.queue.Nack(ctx, d, w.cfg.RetryAfter, w.now())
	} else {
		qerr = w.queue.Ack(ctx, d)
	}
	if qerr != nil && !errors.Is(qerr, queue.ErrStaleReceipt) {
		return rep, true, qerr
	}
	if w.observe != nil {
		w.observe(rep)
	}
	return rep, true, perr
}

// Run 一直 RunOnce 到 ctx 結束；queue 空的時候睡 idle 再看。process 的錯誤交給 observer 之後繼續跑，
// 只有 queue 本身壞掉才回傳。
func (w *Worker) Run(ctx context.Context, idle time.Duration) error {
	for {
		_, ok, err := w.RunOnce(ctx)
		if err != nil && !ok {
			return err
		}
		if ok {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(idle):
		}
	}
}

// process 照 intent 現在的狀態決定這份 job 要做什麼。job 是 at-least-once 交付的，所以這裡不能假設「job 來了就代表
// intent 在 authorized」；每一次都重讀 intent、照它現在的狀態走：
//
//   - created：還沒 authorized（簽名迴圈還沒走完，或 job 比 intent 早到）。retry。
//   - authorized：正常路。CAS 推到 settling（輸了就是別人先動了它：另一個 worker、或 API 取消；retry，下次重讀再說）、
//     記 hold、Send、推到 confirming。Send 失敗只 retry（見 Sender），下一次交付會落到 settling 那一格。
//   - settling：上一次的 worker 死在半路，或還在送。沒有 tx hash 可看（進 confirming 才會有）。年輕就 retry，
//     超過 StuckAfter 就 needs_review。
//   - 其他（confirming、needs_review、settled、failed、canceled）：relayer 沒有事可做。no-op。
func (w *Worker) process(ctx context.Context, d queue.Delivery) (Report, error) {
	it, err := w.intents.Get(ctx, d.Job.IntentID)
	if err != nil {
		if errors.Is(err, intent.ErrNotFound) {
			// job 指著一筆不存在的 intent：這是 bug（誰丟的 job？），但留在 queue 裡也不會變好。丟掉、留一句話。
			return Report{Outcome: OutcomeNoop, Detail: "intent not found, dropping job"}, nil
		}
		return Report{}, err
	}

	switch it.State {
	case intent.StateCreated:
		return Report{Outcome: OutcomeRetry, Detail: "not authorized yet"}, nil

	case intent.StateAuthorized:
		// 先跟 limiter 要一個送出的名額，拿到之前一個 byte 都不寫：這時放手最便宜，intent 還在 authorized、帳上沒有 hold，
		// job Nack 回 queue 就好（為什麼不把名額擋在 Send 裡面，見 Limiter）。等待的上限是這份 lease 剩下的時間：
		// lease 過期後 job 會被別人領走，再等下去也輪不到我們做。
		actx, cancel := context.WithTimeout(ctx, d.LeaseUntil.Sub(w.now()))
		err := w.limiter.Acquire(actx)
		cancel()
		if err != nil {
			return Report{Outcome: OutcomeRetry, Detail: "throttled: " + err.Error()}, nil
		}
		defer w.limiter.Release()
		if err := w.advance(ctx, it, intent.Request{To: intent.StateSettling, By: intent.ActorRelayer, At: w.now()}); err != nil {
			if errors.Is(err, intent.ErrVersionConflict) {
				return Report{Outcome: OutcomeRetry, Detail: "lost the race to settling, will re-read"}, nil
			}
			return Report{}, err
		}
		// 帳上先卡位，交易才出門。hold 記的是請款金額；實收多少要等鏈上確認、由 listener 記 post。
		if _, _, err := w.journal.Append(ctx, holdEntry(it)); err != nil {
			return Report{}, err
		}
		txHash, err := w.sender.Send(ctx, it)
		if err != nil {
			return Report{Outcome: OutcomeRetry, Detail: "send: " + err.Error()}, nil
		}
		// 這裡若寫不回去（store 掛了），交易已經在路上、intent 卻停在 settling：下一次交付會走 settling 那一格，
		// 最後由人（或之後的 listener）拿著鏈上的 ref 對回來。今天先接受這個洞。
		if err := w.advance(ctx, it, intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: txHash, At: w.now()}); err != nil {
			return Report{}, err
		}
		return Report{Outcome: OutcomeSent, TxHash: txHash}, nil

	case intent.StateSettling:
		age := w.now().Sub(it.UpdatedAt)
		if age < w.cfg.StuckAfter {
			return Report{Outcome: OutcomeRetry, Detail: fmt.Sprintf("settling for %s without tx hash, waiting", age)}, nil
		}
		reason := fmt.Sprintf("settling for %s without tx hash; broadcast outcome unknown", age)
		if err := w.advance(ctx, it, intent.Request{To: intent.StateNeedsReview, By: intent.ActorRelayer, Reason: reason, At: w.now()}); err != nil {
			if errors.Is(err, intent.ErrVersionConflict) {
				return Report{Outcome: OutcomeRetry, Detail: "lost the race to needs_review, will re-read"}, nil
			}
			return Report{}, err
		}
		return Report{Outcome: OutcomeReview, Detail: reason}, nil

	default:
		return Report{Outcome: OutcomeNoop, Detail: "already " + string(it.State)}, nil
	}
}

// advance 是「Apply、CAS 寫回」：拿的是 process 開頭讀到的那份 intent，不重讀。
// 這樣從讀到寫之間任何人動過這筆 intent（另一個 worker、API 取消），都只會以 ErrVersionConflict 的形式出現，
// 呼叫端只要處理一種競爭；用 intent.Advance 會多讀一次，競爭就可能變成 ErrTerminal 之類看起來像 bug 的錯誤。
// 成功時 it 就是寫回後的那份。
func (w *Worker) advance(ctx context.Context, it *intent.Intent, req intent.Request) error {
	expected := it.Version
	applied, err := intent.Apply(it, req)
	if err != nil || !applied {
		return err
	}
	return w.intents.Save(ctx, it, expected)
}

// holdEntry 是 relayer 在廣播之前記的那筆 hold：payer 出請款金額、merchant 進請款金額，ID 是 <intent id>/hold。
// At 用 intent 進 settling 的時間（UpdatedAt），不用 worker 自己的時鐘：hold 在邏輯上就發生在 settling 那一刻，
// 而且同一份 intent 不管誰來算，算出來的 hold 都一樣，journal 對它才是冪等的（重放 no-op，而不是 ErrConflict）。
func holdEntry(it *intent.Intent) ledger.Entry {
	return ledger.Entry{
		ID:    it.ID + "/hold",
		Ref:   it.Ref,
		Kind:  ledger.KindHold,
		Asset: ledger.Asset{Chain: it.Chain, Token: it.Token},
		Legs: []ledger.Leg{
			{Account: ledger.PayerAccount(it.Payer), Amount: new(big.Int).Neg(it.Amount)},
			{Account: ledger.MerchantAccount(it.Merchant), Amount: new(big.Int).Set(it.Amount)},
		},
		By: string(intent.ActorRelayer),
		At: it.UpdatedAt,
	}
}

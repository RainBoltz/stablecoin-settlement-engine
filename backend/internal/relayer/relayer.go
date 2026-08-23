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
// settling 那一格的規則本來很保守：重來的 worker 看到 intent 已經在 settling、卻沒有 tx hash，它不知道上一次的交易
// 送出去了沒（可能死在廣播前，也可能死在廣播後、寫回 confirming 前），再送一次可能付兩次，所以它只能等，
// 超過 StuckAfter 就推到 needs_review。代價是 RPC 抖一下、或 worker 慢一點，就會有 intent 送審。
// 現在它多了一招：Broadcasts 記著每一次嘗試站的是哪一個號、出了多少價，所以重來的 worker 有辦法在同一個號上
// 送一筆更貴的交易，把可能還在 mempool 裡的那筆換掉（見 Worker.rescue 與 internal/txfee）。同號最多一筆會進區塊，
// 所以再送一次不會讓錢多動一次。
//
// 沒有真的鏈：Sender 是介面，Example 與測試用 fake。多個 worker 怎麼共用一條 queue、怎麼收工、怎麼不把 RPC 打爆，
// 見 Pool 與 Throttle；同一個帳戶送出的交易在鏈上怎麼排成一列，見 OrderedSender 與 internal/txseq；
// 一次失敗值不值得再交付一次，見 internal/txfail 與 Worker.poison；停止重試的那份 job 停到哪裡去、
// 誰有資格把它放回來，見 internal/dlq。
package relayer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/dlq"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfail"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfee"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// Sender 把一筆 intent 的付款送上鏈、回傳 tx hash。今天只有測試用的 fake；接真的鏈時，chain adapter 實作這個介面。
//
// 回傳 error 的意思是「這次沒送成」，但 relayer 不敢假設「沒送成等於沒送出去」：RPC 逾時的那筆可能已經被節點收下了。
// 這就是為什麼 Send 失敗之後 relayer 只 Nack，不自己再送一次。確定沒出門的那幾種錯誤要包 ErrNotSent，見下面。
type Sender interface {
	Send(ctx context.Context, it *intent.Intent) (txHash string, err error)
}

// SenderFunc 讓一個函式當 Sender 用。
type SenderFunc func(ctx context.Context, it *intent.Intent) (string, error)

// Send 實作 Sender。
func (f SenderFunc) Send(ctx context.Context, it *intent.Intent) (string, error) { return f(ctx, it) }

// ErrNotSent 是 Sender 說「這筆確定沒出門」的方式：簽名失敗、參數組不出來、連線根本沒建立。包在錯誤裡
// （fmt.Errorf("%w: ...", relayer.ErrNotSent)）relayer 才敢把序號收回來給下一筆用。沒包的錯誤一律當成「不知道」，
// 序號就此留一個洞（見 txseq.Sent）。這個介面刻意做成「要主動宣告」而不是「預設沒送出去」：
// 猜錯的代價不對稱，把出門了的當成沒出門會撞到自己那筆還躺在 mempool 的交易。
var ErrNotSent = errors.New("relayer: transaction was not sent")

// OrderedSender 是「序號要發送方自己算」的那類鏈的 Sender：EVM 的 nonce、TON 的 seqno 都要在簽名之前就決定
// 這筆交易站哪一格。worker 看到 sender 實作了這個介面，就會先跟 Sequencer 取號再呼叫 SendAt。
//
// 沒實作的走原本的 Send，relayer 完全不介入：Solana 的 recent blockhash 與 SUI 的 object version 都是送出當下
// 從鏈上讀的，同一個帳戶要同時送幾筆都行（見 txseq 的 package 註解）。
//
// 它包含 Sender，是為了讓一個 OrderedSender 可以直接塞進 New；實作者可以讓 Send 回一句「這條鏈的交易沒有序號組不出來」，
// worker 不會呼叫它。
type OrderedSender interface {
	Sender
	// Account 回報這筆 intent 會從哪個帳戶送出去。序號綁在帳戶上，不是綁在 intent 上：
	// 兩筆付款只要從同一個錢包出去，就得排隊。
	Account(it *intent.Intent) string
	// SendAt 帶著序號送。res.Value 就是要填進交易的 nonce 或 seqno。
	SendAt(ctx context.Context, it *intent.Intent, res txseq.Reservation) (txHash string, err error)
}

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
	// OutcomeReplaced：在原本那個號上用更高的出價再送了一次同一筆付款，intent 走到 confirming。Ack。
	OutcomeReplaced Outcome = "replaced"
	// OutcomeCleared：放棄這筆付款，在原本那個號上送了一筆不動錢的交易把那一格清出來，intent 推到 needs_review。Ack。
	OutcomeCleared Outcome = "cleared"
	// OutcomePoison：這份 job 再交付幾次結果都一樣（錯誤宣告了，或 max attempts 用完），停止重試。Ack。
	// 那筆 intent 要不要跟著收尾、怎麼收尾，看它停在哪一格；那份 job 本身停進 dlq 等人，都見 Worker.poison。
	OutcomePoison Outcome = "poison"
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
	// Err 是這次失敗的原因本身，給 txfail.Judge 用；Detail 是同一件事給人看的版本。
	// 兩份都留是因為包過的錯誤要用 errors.Is 才拆得開，字串拆不動。String 不印它。
	Err error
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
	// RetryAfter 是 backoff 階梯的第一階：第一次 Nack 之後多久再交付，之後每一次加倍（見 internal/txfail）。
	// 所以它只決定起點，不決定一份 job 最久隔多久回來。這裡設 5 秒。
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
	queue      queue.Queue
	intents    intent.Store
	journal    ledger.Journal
	sender     Sender
	limiter    Limiter
	seq        txseq.Sequencer
	cfg        Config
	fee        txfee.Policy
	faults     txfail.Policy
	broadcasts Broadcasts
	dead       dlq.Store
	now        func() time.Time
	observe    func(Report)
}

// Option 調整 Worker 的預設值。
type Option func(*Worker)

// WithClock 換掉時鐘，測試用。
func WithClock(now func() time.Time) Option { return func(w *Worker) { w.now = now } }

// WithConfig 換掉三個時間常數。
func WithConfig(c Config) Option { return func(w *Worker) { w.cfg = c } }

// WithObserver 讓 Run 每處理完一份 job 就回報一次（印 log、算指標都從這裡接）。
func WithObserver(f func(Report)) Option { return func(w *Worker) { w.observe = f } }

// WithSequencer 換掉發號器（預設一個空的 txseq.Counter）。只有 sender 是 OrderedSender 時才用得到；
// 接真的鏈時要先對每個發送帳戶 Sync 一次，不然第一筆交易會拿到一個早就用過的號。
func WithSequencer(s txseq.Sequencer) Option { return func(w *Worker) { w.seq = s } }

// WithLimiter 換掉限流器（預設 Unlimited）。多個 worker 共用同一個 Worker 時，它們也就共用同一個 Limiter，
// 這正是要的：名額限的是整個 pool 對外的總量，不是每個 worker 各自的。
func WithLimiter(l Limiter) Option { return func(w *Worker) { w.limiter = l } }

// WithFeePolicy 換掉替換規則（預設 txfee.DefaultPolicy）。起價、加價幅度、天花板、最多廣播幾次都在裡面。
func WithFeePolicy(p txfee.Policy) Option { return func(w *Worker) { w.fee = p } }

// WithFaultPolicy 換掉重試規則（預設 txfail.DefaultPolicy）。一份 job 最多交付幾次、backoff 的上限、加不加 jitter 都在裡面。
func WithFaultPolicy(p txfail.Policy) Option { return func(w *Worker) { w.faults = p } }

// WithBroadcasts 換掉廣播紀錄本（預設一本空的 MemoryBroadcasts）。多個 worker 要共用同一本，
// 不然重來的那個 worker 讀不到前一個 worker 送出去的東西，救援就退回成送審。
func WithBroadcasts(b Broadcasts) Option { return func(w *Worker) { w.broadcasts = b } }

// WithDeadLetters 換掉停放被放棄的 job 的地方（預設一個空的 dlq.MemoryStore）。人要看得到那些 job，
// 所以整個 pool 與那支給人用的介面要共用同一個。
func WithDeadLetters(s dlq.Store) Option { return func(w *Worker) { w.dead = s } }

// New 建立一個 Worker。四個依賴都是介面：queue、intent store、journal、sender，今天全是記憶體版或 fake。
func New(q queue.Queue, intents intent.Store, journal ledger.Journal, sender Sender, opts ...Option) *Worker {
	w := &Worker{queue: q, intents: intents, journal: journal, sender: sender, limiter: Unlimited{},
		seq: txseq.NewCounter(), cfg: DefaultConfig(), fee: txfee.DefaultPolicy(), faults: txfail.DefaultPolicy(),
		broadcasts: NewMemoryBroadcasts(), dead: dlq.NewMemoryStore(), now: time.Now}
	for _, o := range opts {
		o(w)
	}
	return w
}

// RunOnce 領一份 job、處理、然後 Ack 或 Nack。queue 空的回傳 ok=false。
//
// 這裡是 Outcome 對應到 queue 動作的唯一地方：retry 以外的都 Ack（這份工作結束了）；retry 才 Nack，
// 而且隔多久再交付、還要不要再交付，由 txfail.Judge 說了算。process 回錯（store 或 journal 出問題）
// 也算一次失敗：錯誤本身回給呼叫端，job 照判決回 queue。
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
		rep.Outcome, rep.Detail, rep.Err = OutcomeRetry, "error: "+perr.Error(), perr
	}
	// 失敗的 job 不再一律以同一個延遲回 queue：先判這一次值不值得再交付一次。判決是 poison 的話
	// Nack 這條路就此結束，改由 poison 決定那份 job 與那筆 intent 各自怎麼收尾。
	retryIn := w.cfg.RetryAfter
	if rep.Outcome == OutcomeRetry {
		v := w.faults.Judge(txfail.Fault{Err: rep.Err, Attempt: d.Attempt, Base: w.cfg.RetryAfter})
		retryIn = v.Backoff
		if v.Class == txfail.ClassPoison {
			r, err := w.poison(ctx, d, rep, v)
			if err != nil {
				return rep, true, err
			}
			r.Job, r.Attempt = d.Job, d.Attempt
			rep, retryIn = r, w.cfg.RetryAfter
		}
	}
	var qerr error
	if rep.Outcome == OutcomeRetry {
		qerr = w.queue.Nack(ctx, d, retryIn, w.now())
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
//   - authorized：正常路。取號（見 OrderedSender）、CAS 推到 settling（輸了就是別人先動了它：另一個 worker、
//     或 API 取消；retry，下次重讀再說）、記 hold、Send、推到 confirming。Send 失敗只 retry（見 Sender），
//     下一次交付會落到 settling 那一格。
//   - settling：上一次的 worker 死在半路，或還在送。沒有 tx hash 可看（進 confirming 才會有）。年輕就 retry，
//     久了就照 Broadcasts 的紀錄決定要不要在同一個號上替換掉它（見 Worker.rescue）。
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

		// 取號跟拿名額一樣擋在副作用之前，理由也一樣：等待要在什麼都還沒寫的時候等，拿不到就原封不動回 queue
		// （intent 還在 authorized、帳上沒有 hold）。但歸還的規則不一樣：名額 defer 就還，序號要看交易到底有沒有出門，
		// 所以 sent 一路帶到函式結束才收尾。預設 SentNo：只要沒走到 Send，這個號就沒被用掉，退回去給下一筆用。
		ordered, isOrdered := w.sender.(OrderedSender)
		res, sent := txseq.Reservation{}, txseq.SentNo
		if isOrdered {
			rctx, cancel := context.WithTimeout(ctx, d.LeaseUntil.Sub(w.now()))
			r, rerr := w.seq.Reserve(rctx, ordered.Account(it))
			cancel()
			if rerr != nil {
				return Report{Outcome: OutcomeRetry, Detail: "no slot: " + rerr.Error()}, nil
			}
			res = r
			// 收尾用 WithoutCancel：ctx 被取消的時候序號更需要交代，不交代就是留一個洞。
			defer func() { _ = w.seq.Resolve(context.WithoutCancel(ctx), res, sent) }()
		}

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
		var txHash string
		var serr error
		if isOrdered {
			txHash, serr = ordered.SendAt(ctx, it, res)
		} else {
			txHash, serr = w.sender.Send(ctx, it)
		}
		if serr != nil {
			// 沒宣告 ErrNotSent 的失敗一律當成「不知道」：序號當成用掉了，這個帳戶到對帳為止不再發號。
			// 退回去重用會撞到自己那筆可能還躺在 mempool 的交易，那比停下來貴得多。
			if !errors.Is(serr, ErrNotSent) {
				sent = txseq.SentUnknown
			}
			w.record(ctx, it, res, w.fee.Base, "", sent)
			return Report{Outcome: OutcomeRetry, Detail: "send: " + serr.Error(), Err: serr}, nil
		}
		sent = txseq.SentYes
		w.record(ctx, it, res, w.fee.Base, txHash, sent)
		// 這裡若寫不回去（store 掛了），交易已經在路上、intent 卻停在 settling：下一次交付會走 settling 那一格，
		// 最後由人（或之後的 listener）拿著鏈上的 ref 對回來。今天先接受這個洞。
		if err := w.advance(ctx, it, intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: txHash, At: w.now()}); err != nil {
			return Report{}, err
		}
		return Report{Outcome: OutcomeSent, TxHash: txHash}, nil

	case intent.StateSettling:
		return w.rescue(ctx, d, it)

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

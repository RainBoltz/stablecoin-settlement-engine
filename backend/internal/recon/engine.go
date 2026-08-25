package recon

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/dlq"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/listener"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
)

// Deps 是對帳引擎要認識的每一個東西。它是整個系統裡唯一什麼都認識的元件，因為對帳本來就是把每一層對在一起。
type Deps struct {
	Intents  intent.Store
	Journal  ledger.Journal
	Jobs     queue.Queue
	Dead     dlq.Store
	Listener *listener.Listener
	Source   Source
}

// ErrOutsideWindow：Source 回了一筆高度在 window 之外的轉帳。這是 adapter 的 bug，而且是會把還沒 finalized 的錢
// 算進來的那種，所以不忍、不跳過，整段 window 下次重來。
var ErrOutsideWindow = errors.New("recon: transfer outside the window")

// Engine 對一條鏈做對帳。一個 Engine 管一條鏈，因為 cursor 是那條鏈的高度；多鏈就是多個 Engine。
//
// 兩個 Engine 對同一條鏈同時跑也不會出事（排程重疊、兩個副本），因為每一步都是冪等的；
// 只是同一段 window 會被對兩次、同一份 Finding 會被列兩次。
type Engine struct {
	chain string
	d     Deps
	mu    sync.Mutex
	// cursor 是上一次對到的最高高度，下一次從 cursor+1 開始。今天只在記憶體裡。
	cursor uint64
	now    func() time.Time
}

// Option 調整 Engine 的預設值。
type Option func(*Engine)

// WithClock 換掉時鐘，測試用。時鐘只用在 Enqueue 上，對帳本身不看時間，看高度。
func WithClock(now func() time.Time) Option { return func(e *Engine) { e.now = now } }

// WithCursor 指定從哪個高度之後開始對。預設 0，也就是從創世區塊；正式環境要從上一次存下來的 cursor 接著。
func WithCursor(height uint64) Option { return func(e *Engine) { e.cursor = height } }

// New 建立一個 Engine。chain 是 intent.Chain 的寫法（evm:31337），不是協定名：cursor 是網路的高度，不是協定的。
func New(chain string, d Deps, opts ...Option) *Engine {
	e := &Engine{chain: chain, d: d, now: time.Now}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Cursor 回報上一次對到的最高高度。
func (e *Engine) Cursor() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cursor
}

// Run 對一次：先掃鏈下、再對鏈上、最後才推 cursor。
//
// 順序是刻意的。鏈下掃描擺在前面，是因為它會把 confirming 的 intent 交給 listener 收尾，等一下對鏈上的時候
// 那些 intent 多半已經 settled、post 已經在帳上，對起來就是「對得上」而不是「再 Check 一次」。
// cursor 擺在最後，是因為中間任何一步回錯都代表這段 window 沒對完：cursor 不動，下一次整段重來，
// 而重來是安全的（見 package 註解的第三條紀律）。
func (e *Engine) Run(ctx context.Context) (Report, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rep := Report{Chain: e.chain}
	if err := e.sweep(ctx, &rep); err != nil {
		return rep, err
	}
	final, err := e.d.Source.Finalized(ctx)
	if err != nil {
		return rep, err
	}
	rep.From, rep.To = e.cursor+1, final
	if final <= e.cursor {
		return rep, nil
	}
	transfers, err := e.d.Source.Transfers(ctx, rep.From, rep.To)
	if err != nil {
		return rep, err
	}
	// 照鏈上的先後對：同一個 ref 出現兩次時，先進區塊的那筆算數，後面那筆是 paid_twice。誰才是真的由人判，
	// 對帳引擎只保證每次跑出來的答案一樣。
	sort.Slice(transfers, func(i, j int) bool {
		if transfers[i].Height != transfers[j].Height {
			return transfers[i].Height < transfers[j].Height
		}
		return transfers[i].TxHash < transfers[j].TxHash
	})
	for _, t := range transfers {
		if t.Height < rep.From || t.Height > rep.To {
			return rep, fmt.Errorf("%w: tx %s at %d, window %d..%d", ErrOutsideWindow, t.TxHash, t.Height, rep.From, rep.To)
		}
		if err := e.reconcile(ctx, t, &rep); err != nil {
			return rep, err
		}
	}
	e.cursor = final
	return rep, nil
}

// sweep 是鏈下那一半：找出這條鏈上還沒有人推它的 intent，各推一把。
//
//   - authorized、settling：丟一份 settle job 進 queue。Enqueue 對同 ID 冪等，queue 還記得那份 job 的話就是 no-op；
//     真的掉了的（寫完 intent、丟 job 之前掛掉）從這裡補回來。已經停在 dlq 裡等人的不碰：
//     那份 job 被放棄是有理由的，再丟一份進去只是把 poison 的迴圈拉長，能放它回去的只有人工介入。
//   - confirming：交給 listener.Check。這就是「誰把 confirming 的 intent 交給 listener」的答案：對帳引擎每一次 Run 都交一次。
//
// 轉移表上 relayer 與 listener 各自的地盤都沒有動：對帳引擎丟的是便條、交的是 intent id，判斷還是它們自己的。
func (e *Engine) sweep(ctx context.Context, rep *Report) error {
	open, err := e.d.Intents.ByState(ctx, intent.StateAuthorized, intent.StateSettling, intent.StateConfirming)
	if err != nil {
		return err
	}
	for _, it := range open {
		if it.Chain != e.chain {
			continue
		}
		s := Sweep{IntentID: it.ID, State: it.State}
		switch it.State {
		case intent.StateConfirming:
			r, err := e.d.Listener.Check(ctx, it.ID)
			switch {
			case errors.Is(err, intent.ErrVersionConflict):
				s.Action = "lost the race to another listener; next run re-reads"
			case err != nil:
				return err
			default:
				s.Action = string(r.Outcome) + " (" + r.Detail + ")"
			}
		default:
			job := settleJob(it)
			rec, err := e.d.Dead.Get(ctx, job.ID)
			switch {
			case err == nil && rec.Status == dlq.StatusParked:
				s.Action = "parked in the dlq, waiting for a person"
			case err != nil && !errors.Is(err, dlq.ErrNotFound):
				return err
			default:
				applied, err := e.d.Jobs.Enqueue(ctx, job, e.now())
				if err != nil {
					return err
				}
				s.Action = "already queued"
				if applied {
					s.Action = "enqueued " + job.ID
				}
			}
		}
		rep.Sweeps = append(rep.Sweeps, s)
	}
	return nil
}

// settleJob 是 API 那一側本來就該丟的那份 job，形狀照 queue 的慣例：<intent id>/settle，只帶 id 與 ref。
func settleJob(it *intent.Intent) queue.Job {
	return queue.Job{ID: it.ID + "/settle", Kind: queue.KindSettle, IntentID: it.ID, Ref: it.Ref}
}

// reconcile 是鏈上那一半：拿一筆轉帳的 ref 去找主人，照主人現在停在哪一格決定它是對得上、要補證據、還是一筆 Finding。
//
// 每一筆都重讀 intent，不把前一筆的結果帶到下一筆：同一個 ref 的兩筆轉帳，第一筆把 intent 推到 settled 之後，
// 第二筆重讀到的就是 settled，走的就是 paid_twice 那條路。這跟 worker 每一次交付都重讀 intent 是同一個習慣。
func (e *Engine) reconcile(ctx context.Context, t Transfer, rep *Report) error {
	if t.Ref.IsZero() {
		rep.Findings = append(rep.Findings, Finding{Kind: KindUnreferenced, Transfer: t,
			Detail: fmt.Sprintf("%s to %s without a ref", t.Amount, shortAddr(t.To))})
		return nil
	}
	it, err := e.d.Intents.GetByRef(ctx, t.Ref)
	if errors.Is(err, intent.ErrNotFound) {
		rep.Findings = append(rep.Findings, Finding{Kind: KindUnknownRef, Transfer: t,
			Detail: fmt.Sprintf("ref %s matches no intent", shortHex(t.Ref.String()))})
		return nil
	}
	if err != nil {
		return err
	}
	who := fmt.Sprintf("ref %s (%s)", shortHex(t.Ref.String()), it.ID)
	// 補證據的條件比叫人嚴：只有「這筆轉帳長得跟 intent 上寫的付款一模一樣」才拿來當證據。
	// ref 是對付款條件的 commitment，但寫進交易的是一個 32 bytes，誰都可以抄；token、付款人、收款人有一個不對，
	// 就不是這筆付款，錢卻動了，那是人要看的。
	if it.Chain != e.chain || t.Token != it.Token || t.From != it.Payer || t.To != it.Merchant {
		rep.Findings = append(rep.Findings, Finding{Kind: KindUnexpected, Transfer: t, IntentID: it.ID,
			Detail: who + " carries the ref, but token, payer or merchant differ from the intent"})
		return nil
	}

	switch it.State {
	case intent.StateSettled:
		// 鏈下已經結案。對的是帳本上那筆 post，不是 intent 上的請款金額：post 記的才是「我們認定實際發生的事」。
		post, err := e.d.Journal.Get(ctx, it.ID+"/post")
		if errors.Is(err, ledger.ErrNotFound) {
			rep.Findings = append(rep.Findings, Finding{Kind: KindMismatch, Transfer: t, IntentID: it.ID,
				Detail: who + " is settled but has no post on the books"})
			return nil
		}
		if err != nil {
			return err
		}
		if post.TxHash != t.TxHash {
			rep.Findings = append(rep.Findings, Finding{Kind: KindPaidTwice, Transfer: t, IntentID: it.ID,
				Detail: fmt.Sprintf("%s already settled on tx %s", who, post.TxHash)})
			return nil
		}
		if got := merchantLeg(post, it.Merchant); got == nil || got.Cmp(t.Amount) != 0 {
			rep.Findings = append(rep.Findings, Finding{Kind: KindMismatch, Transfer: t, IntentID: it.ID,
				Detail: fmt.Sprintf("%s posted %s, chain says %s", who, got, t.Amount)})
			return nil
		}
		rep.Matches = append(rep.Matches, Match{Transfer: t, IntentID: it.ID, Action: "settled, post matches the chain"})
		return nil

	case intent.StateConfirming:
		prefix := ""
		if it.TxHash != t.TxHash {
			// intent 身上的 hash 是最後一次送出去的那筆，鏈上帶著 ref 的卻是另一筆（替換之後舊那筆贏了、
			// 或原封不動重送的兩筆進了不同的區塊）。listener 不能拿另一筆 hash 宣告 settled（ErrEvidenceMismatch），
			// 所以走轉移表上唯一的回頭路：先退回 settling 把舊 hash 清掉，再帶著鏈上這筆進 confirming。
			// 兩步都是 listener 的權限，兩步中間死掉也沒關係：下一次這筆轉帳會走 settling 那條路，補回第二步。
			reason := fmt.Sprintf("tx %s on record, but the chain shows tx %s carrying the ref", it.TxHash, t.TxHash)
			ok, err := e.push(ctx, it.ID, intent.Request{To: intent.StateSettling, By: intent.ActorListener, Reason: reason, At: e.now()})
			if err != nil || !ok {
				return e.raced(t, it.ID, rep, err)
			}
			ok, err = e.push(ctx, it.ID, intent.Request{To: intent.StateConfirming, By: intent.ActorListener, TxHash: t.TxHash, At: e.now()})
			if err != nil || !ok {
				return e.raced(t, it.ID, rep, err)
			}
			prefix = fmt.Sprintf("on record %s -> settling -> confirming -> ", it.TxHash)
		}
		return e.check(ctx, t, it.ID, prefix, rep)

	case intent.StateSettling:
		// relayer 送出去了、寫回 confirming 之前死掉：鏈上有交易、intent 沒有 hash。這是對帳引擎補證據的正宗用途，
		// 轉移表上 settling -> confirming 本來就寫著 listener。補完交給 listener 判，不自己宣告 settled。
		ok, err := e.push(ctx, it.ID, intent.Request{To: intent.StateConfirming, By: intent.ActorListener, TxHash: t.TxHash, At: e.now()})
		if err != nil || !ok {
			return e.raced(t, it.ID, rep, err)
		}
		return e.check(ctx, t, it.ID, "settling -> confirming -> ", rep)

	default:
		// created、authorized、needs_review、failed、canceled：鏈下沒有人在等這筆錢，錢卻動了。
		rep.Findings = append(rep.Findings, Finding{Kind: KindUnexpected, Transfer: t, IntentID: it.ID,
			Detail: fmt.Sprintf("%s is %s, yet the money moved", who, it.State)})
		return nil
	}
}

// push 是「讀、Apply、CAS 寫回」。回 ok=false 代表輸給了別人：不是錯，是有人先動了它。
//
// 輸的樣子有三種，因為 reconcile 讀到的那份 intent 跟這裡寫回之間，任何人都可能先把它走完：
// 寫回撞到別人是 ErrVersionConflict；Advance 重讀時它已經到 terminal 是 ErrTerminal；已經被推去 needs_review
// 的話「從那裡到 confirming」不在轉移表上，是 ErrIllegalTransition。三種都代表「這筆已經不歸這一次對帳管」，
// 放手就好，下一段 window 重讀再對。其他錯誤（ErrForbiddenActor、ErrMissingEvidence）是 recon 自己的 bug，照樣往上丟。
func (e *Engine) push(ctx context.Context, id string, req intent.Request) (bool, error) {
	_, _, err := intent.Advance(ctx, e.d.Intents, id, req)
	if errors.Is(err, intent.ErrVersionConflict) || errors.Is(err, intent.ErrTerminal) || errors.Is(err, intent.ErrIllegalTransition) {
		return false, nil
	}
	return err == nil, err
}

// raced 把「輸給別人」記成一筆 Match 而不是錯：這段 window 下一次還會再對一遍，到時候重讀就對了。
func (e *Engine) raced(t Transfer, id string, rep *Report, err error) error {
	if err != nil {
		return err
	}
	rep.Matches = append(rep.Matches, Match{Transfer: t, IntentID: id, Action: "lost the race to another actor; next run re-reads"})
	return nil
}

// check 把補好證據的 intent 交給 listener，然後把它的結果接在 prefix 後面記成一筆 Match。
func (e *Engine) check(ctx context.Context, t Transfer, id, prefix string, rep *Report) error {
	r, err := e.d.Listener.Check(ctx, id)
	if errors.Is(err, intent.ErrVersionConflict) {
		return e.raced(t, id, rep, nil)
	}
	if err != nil {
		return err
	}
	rep.Matches = append(rep.Matches, Match{Transfer: t, IntentID: id, Action: prefix + string(r.Outcome) + " (" + r.Detail + ")"})
	return nil
}

// merchantLeg 找出 post 裡 merchant 那條腿的金額；沒有就回 nil。
func merchantLeg(post ledger.Entry, merchant string) *big.Int {
	for _, l := range post.Legs {
		if l.Account == ledger.MerchantAccount(merchant) {
			return l.Amount
		}
	}
	return nil
}

// Package listener 是狀態機裡 confirming 這一格的主人：relayer 把交易送進區塊之後，盯著鏈看的那個元件。
//
// 從轉移表定案那天起，confirming 的三個出口（settled、退回 settling、needs_review）就都寫著 listener，
// 因為只有它從鏈上讀事實：relayer 看到的是「我送出去了」，不是「錢動了」。這個 package 把那三個出口做出來：
//
//   - settled：鏈說這筆交易不可逆（見 internal/finality）、而且交易裡真的有一筆帶著我們 ref 的轉帳、金額跟請款一樣。
//     帳上先把 hold 收成 post，再推 settled——帳先動、狀態後走，跟 relayer 記 hold 的順序同一個方向。
//   - settling：交易太久不在任何區塊裡（被 reorg 吐回來、或被丟掉）。這是轉移表上唯一的回頭路，
//     退回去之後 relayer 照 Broadcasts 的紀錄決定要等、要替換、還是要送審，listener 不替它做這個決定。
//   - needs_review：不可逆了，但錢沒有照請款的樣子動——交易 revert、交易裡找不到帶我們 ref 的轉帳（幽靈支付）、
//     實收跟請款對不上。這三種 listener 都不敢自己判，因為判錯的兩邊都是錢：宣告 settled 是替商家記一筆沒收到的錢，
//     宣告 failed 是把一筆可能已經到帳的付款當成沒付。
//
// 「不可逆」跟「錢動了」是兩個問題，這裡刻意分成兩步問：finality.Judge 只回答第一個，第二個看 Watcher 回來的 Received。
// 一筆 finalized 的交易可以一毛錢都沒動（Day 2 那種回傳 false 的 token），finality 過了只代表這個結果不會再變。
//
// 跟鏈的接口是 Watcher，今天只有測試用的 fake；接真的鏈時 chain adapter 讀取的那一半實作它。
// 把 confirming 的 intent 交給 Check 的是對帳引擎的鏈下掃描，「鏈上動過的錢是不是都對得回 intent」也是它對的，
// 見 internal/recon。
package listener

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
)

// Sighting 是 Watcher 對一筆 intent 的回答：那筆交易在鏈上怎麼樣，以及錢有沒有照我們的 ref 動。
type Sighting struct {
	finality.Observation
	// Received 是這筆交易裡、帶著 intent 的 ref 的那筆轉帳讓 merchant 實際多了多少。nil 代表交易裡沒有這樣一筆：
	// 交易成功、gas 燒了、餘額沒變，就是幽靈支付。它跟請款金額的差就是轉帳稅，帳上會落在 fee 那條腿，
	// 但今天 listener 遇到對不上的一律送審，容差與 fee 腿之後再討論。
	Received *big.Int
}

// Watcher 是 chain adapter 讀鏈的那一半：拿 intent 身上的 tx hash 與 ref 去問鏈。
//
// 它回答的是「這一刻節點看到什麼」，同一筆 intent 問兩次可以得到不同的答案（head 往前走了、marker 到了、交易消失了），
// 所以 listener 每次 Check 都重新問，不記上一次的答案。
type Watcher interface {
	Lookup(ctx context.Context, it *intent.Intent) (Sighting, error)
}

// WatcherFunc 讓一個函式當 Watcher 用。
type WatcherFunc func(ctx context.Context, it *intent.Intent) (Sighting, error)

// Lookup 實作 Watcher。
func (f WatcherFunc) Lookup(ctx context.Context, it *intent.Intent) (Sighting, error) {
	return f(ctx, it)
}

// ErrNoPolicy：這筆 intent 的鏈沒有設定不可逆規則。這是設定錯誤，不是可以猜的事，所以不給預設。
var ErrNoPolicy = errors.New("listener: no finality policy for chain")

// Outcome 是 Check 一次的五種結果。名字對著 intent 被推到的那一格取，wait 與 no-op 沒有動 intent。
type Outcome string

const (
	// OutcomeWait：還不能算數，下次再看。intent 一個欄位都沒動。
	OutcomeWait Outcome = "wait"
	// OutcomeSettled：帳上記了 post、intent 推到 settled。
	OutcomeSettled Outcome = "settled"
	// OutcomeHandedBack：交易不在鏈上了，intent 退回 settling 交給 relayer。
	OutcomeHandedBack Outcome = "settling"
	// OutcomeReview：不可逆了但錢不對，intent 推到 needs_review。
	OutcomeReview Outcome = "needs_review"
	// OutcomeNoop：intent 不在 confirming，listener 沒有事可做（多半是同一筆被看了兩次）。
	OutcomeNoop Outcome = "no-op"
)

// Report 是 Check 一次的結果，Example 會印出來。
type Report struct {
	IntentID string
	TxHash   string
	Outcome  Outcome
	// Detail 是給人看的一句話：為什麼等、憑什麼算 final、為什麼送審。
	Detail string
}

// String 用固定格式印一行：intent、結果、tx、細節。
func (r Report) String() string {
	s := fmt.Sprintf("%-8s %-12s", r.IntentID, r.Outcome)
	if r.TxHash != "" {
		s += " tx " + r.TxHash
	}
	if r.Detail != "" {
		s += " (" + r.Detail + ")"
	}
	return strings.TrimRight(s, " ")
}

// Listener 對一筆 confirming 的 intent 做一次判斷。它自己沒有狀態，多個 goroutine 共用一個也沒關係：
// 兩個 listener 同時看同一筆 intent，post 對同 ID 是 no-op、Save 是 CAS，晚的那個會拿到 ErrVersionConflict。
type Listener struct {
	intents  intent.Store
	journal  ledger.Journal
	watcher  Watcher
	policies map[string]finality.Policy
	now      func() time.Time
}

// Option 調整 Listener 的預設值。
type Option func(*Listener)

// WithClock 換掉時鐘，測試用。
func WithClock(now func() time.Time) Option { return func(l *Listener) { l.now = now } }

// WithPolicy 換掉一條鏈的不可逆規則。key 是協定名（evm、solana、ton、sui），不是 intent.Chain 整串：
// 同一個協定的每個網路用同一套判斷標準。
func WithPolicy(protocol string, p finality.Policy) Option {
	return func(l *Listener) { l.policies[protocol] = p }
}

// New 建立一個 Listener。預設用 finality.Defaults 那四套判斷標準。
func New(intents intent.Store, journal ledger.Journal, watcher Watcher, opts ...Option) *Listener {
	l := &Listener{intents: intents, journal: journal, watcher: watcher, policies: finality.Defaults(), now: time.Now}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Protocol 取 intent.Chain 冒號前面那一段：evm:31337 的協定是 evm。
func Protocol(chain string) string {
	p, _, _ := strings.Cut(chain, ":")
	return p
}

// Check 對一筆 intent 問一次鏈、判一次，然後照判決推 intent。
//
// 順序是固定的：讀 intent、問鏈、判不可逆、再判錢。每一步都以 intent「現在」的樣子為準，不假設它還在 confirming：
// 同一筆 intent 被兩個 listener 看、或被 operator 先收尾，都會在這裡變成 no-op 或 ErrVersionConflict，不會變成第二筆 post。
func (l *Listener) Check(ctx context.Context, id string) (Report, error) {
	it, err := l.intents.Get(ctx, id)
	if err != nil {
		return Report{}, err
	}
	rep := Report{IntentID: it.ID, TxHash: it.TxHash}
	if it.State != intent.StateConfirming {
		rep.Outcome, rep.Detail = OutcomeNoop, "already "+string(it.State)
		return rep, nil
	}
	policy, ok := l.policies[Protocol(it.Chain)]
	if !ok {
		return rep, fmt.Errorf("%w: %s", ErrNoPolicy, it.Chain)
	}
	seen, err := l.watcher.Lookup(ctx, it)
	if err != nil {
		return rep, err
	}

	// age 是進 confirming 多久了，跟 relayer 的 StuckAfter 一樣從 intent 自己的 UpdatedAt 算，不另外記時間。
	v := policy.Judge(seen.Observation, l.now().Sub(it.UpdatedAt))
	switch v.Kind {
	case finality.KindPending:
		rep.Outcome, rep.Detail = OutcomeWait, v.Reason
		return rep, nil
	case finality.KindLost:
		// 唯一的回頭路。退回去要清掉 tx hash（Apply 會做），因為那筆交易已經不算在鏈上了；
		// 要不要在同一個 nonce 上再送、還是等，relayer 讀 Broadcasts 自己決定。
		rep.Detail = v.Reason
		reason := fmt.Sprintf("tx %s %s", it.TxHash, v.Reason)
		return l.push(ctx, it, rep, OutcomeHandedBack, intent.Request{To: intent.StateSettling, By: intent.ActorListener, Reason: reason, At: l.now()})
	case finality.KindFailed:
		return l.push(ctx, it, rep, OutcomeReview, intent.Request{To: intent.StateNeedsReview, By: intent.ActorListener, Reason: v.Reason, At: l.now()})
	}

	// 到這裡交易已經不可逆而且執行成功。第二個問題：錢有沒有照請款的樣子動。
	switch {
	case seen.Received == nil:
		reason := v.Reason + "; no transfer carrying our ref, nothing moved"
		return l.push(ctx, it, rep, OutcomeReview, intent.Request{To: intent.StateNeedsReview, By: intent.ActorListener, Reason: reason, At: l.now()})
	case seen.Received.Cmp(it.Amount) != 0:
		reason := fmt.Sprintf("%s; received %s, expected %s", v.Reason, seen.Received, it.Amount)
		return l.push(ctx, it, rep, OutcomeReview, intent.Request{To: intent.StateNeedsReview, By: intent.ActorListener, Reason: reason, At: l.now()})
	}

	// 帳先動、狀態後走：post 對同 ID 是 no-op，settled 重放也是 no-op，所以死在兩步中間重來一次沒關係；
	// 反過來先推 settled 的話，settled 是 terminal，死在中間那筆 hold 會永遠掛在 pending 上，沒有人回得來收尾它。
	if _, _, err := l.journal.Append(ctx, postEntry(it, seen.Received)); err != nil {
		return rep, err
	}
	rep.Detail = v.Reason
	return l.push(ctx, it, rep, OutcomeSettled, intent.Request{To: intent.StateSettled, By: intent.ActorListener, TxHash: it.TxHash, At: l.now()})
}

// push 是「Apply、CAS 寫回」：拿的是 Check 開頭讀到的那份 intent，不重讀，理由跟 relayer 的 advance 一樣——
// 從讀到寫之間任何人動過這筆 intent，都只會以 ErrVersionConflict 的形式出現，呼叫端只要處理一種競爭。
func (l *Listener) push(ctx context.Context, it *intent.Intent, rep Report, out Outcome, req intent.Request) (Report, error) {
	expected := it.Version
	applied, err := intent.Apply(it, req)
	if err != nil {
		return rep, err
	}
	if applied {
		if err := l.intents.Save(ctx, it, expected); err != nil {
			return rep, err
		}
	}
	rep.Outcome = out
	if rep.Detail == "" {
		rep.Detail = req.Reason
	}
	return rep, nil
}

// postEntry 是 listener 在鏈上確認之後記的那筆 post：收掉 <intent id>/hold，腿記的是實收金額。
//
// At 用 intent 進 confirming 的時間（UpdatedAt），不用 listener 自己的時鐘，理由跟 relayer 的 holdEntry 一樣：
// 同一份 intent 不管哪個 listener、第幾次來算，算出來的 post 都一樣，journal 對它才是冪等的。
// 死在 post 與 settled 之間的那一次重來，靠的就是這一點。
func postEntry(it *intent.Intent, received *big.Int) ledger.Entry {
	return ledger.Entry{
		ID:    it.ID + "/post",
		Ref:   it.Ref,
		Kind:  ledger.KindPost,
		Holds: it.ID + "/hold",
		Asset: ledger.Asset{Chain: it.Chain, Token: it.Token},
		Legs: []ledger.Leg{
			{Account: ledger.PayerAccount(it.Payer), Amount: new(big.Int).Neg(received)},
			{Account: ledger.MerchantAccount(it.Merchant), Amount: new(big.Int).Set(received)},
		},
		By:     string(intent.ActorListener),
		At:     it.UpdatedAt,
		TxHash: it.TxHash,
	}
}

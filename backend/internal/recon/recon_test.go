package recon_test

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/dlq"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/listener"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/recon"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// world 是一整套鏈下加一條 fake 鏈：測試先把 intent 推到要測的那一格、把轉帳放上鏈，再 Run 一次看結果。
type world struct {
	intents *intent.MemoryStore
	journal *ledger.MemoryJournal
	jobs    *queue.MemoryQueue
	dead    *dlq.MemoryStore
	chain   *fakeChain
	now     time.Time
	amount  *big.Int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return &world{
		intents: intent.NewMemoryStore(), journal: ledger.NewMemoryJournal(), jobs: queue.NewMemoryQueue(),
		dead: dlq.NewMemoryStore(), chain: &fakeChain{final: 164}, now: t0.Add(20 * time.Minute), amount: big.NewInt(100_000_000),
	}
}

// intent 建一筆付款、推到 state 停下來。settling 起帳上有 hold；confirming 帶 tx；settled 帶 post；failed 帶 void。
func (w *world) intent(t *testing.T, id, chain string, state intent.State, tx string) *intent.Intent {
	t.Helper()
	ctx := context.Background()
	it, err := intent.New(intent.Spec{ID: id, Chain: chain, Token: usdc, Payer: payer, Merchant: merchant, Amount: w.amount}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.intents.Save(ctx, it, 0); err != nil {
		t.Fatal(err)
	}
	step := func(req intent.Request) {
		req.At = t0
		if _, _, err := intent.Advance(ctx, w.intents, id, req); err != nil {
			t.Fatalf("%s -> %s: %v", id, req.To, err)
		}
	}
	entry := func(kind ledger.Kind, holds, tx string, legs []ledger.Leg) {
		if _, _, err := w.journal.Append(ctx, ledger.Entry{ID: id + "/" + string(kind), Ref: it.Ref, Kind: kind, Holds: holds,
			Asset: ledger.Asset{Chain: chain, Token: usdc}, Legs: legs, By: "test", At: t0, TxHash: tx}); err != nil {
			t.Fatal(err)
		}
	}
	legs := []ledger.Leg{{Account: ledger.PayerAccount(payer), Amount: new(big.Int).Neg(w.amount)},
		{Account: ledger.MerchantAccount(merchant), Amount: w.amount}}
	if state == intent.StateCreated {
		return it
	}
	step(intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI})
	if state == intent.StateAuthorized {
		return w.get(t, id)
	}
	step(intent.Request{To: intent.StateSettling, By: intent.ActorRelayer})
	entry(ledger.KindHold, "", "", legs)
	switch state {
	case intent.StateSettling:
	case intent.StateFailed:
		entry(ledger.KindVoid, id+"/hold", "", nil)
		step(intent.Request{To: intent.StateFailed, By: intent.ActorRelayer, Reason: "not sent"})
	case intent.StateNeedsReview:
		step(intent.Request{To: intent.StateNeedsReview, By: intent.ActorRelayer, Reason: "unknown"})
	case intent.StateConfirming:
		step(intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: tx})
	case intent.StateSettled:
		step(intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: tx})
		entry(ledger.KindPost, id+"/hold", tx, legs)
		step(intent.Request{To: intent.StateSettled, By: intent.ActorListener, TxHash: tx})
	default:
		t.Fatalf("no fixture for %s", state)
	}
	return w.get(t, id)
}

func (w *world) get(t *testing.T, id string) *intent.Intent {
	t.Helper()
	it, err := w.intents.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// transfer 把一筆帶著 ref 的轉帳放上鏈，形狀跟 intent 上的付款一模一樣。
func (w *world) transfer(tx string, height uint64, ref paymentref.Ref) recon.Transfer {
	tr := recon.Transfer{TxHash: tx, Height: height, Ref: ref, Token: usdc, From: payer, To: merchant, Amount: new(big.Int).Set(w.amount)}
	w.chain.transfers = append(w.chain.transfers, tr)
	return tr
}

func (w *world) engine(opts ...recon.Option) *recon.Engine {
	l := listener.New(w.intents, w.journal, w.chain, listener.WithClock(func() time.Time { return w.now }))
	opts = append([]recon.Option{recon.WithClock(func() time.Time { return w.now })}, opts...)
	return recon.New("evm:31337", recon.Deps{Intents: w.intents, Journal: w.journal, Jobs: w.jobs, Dead: w.dead, Listener: l, Source: w.chain}, opts...)
}

func (w *world) run(t *testing.T, e *recon.Engine) recon.Report {
	t.Helper()
	rep, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (w *world) posts(t *testing.T) []ledger.Entry {
	t.Helper()
	var out []ledger.Entry
	_ = w.journal.Scan(context.Background(), func(e ledger.Entry) error {
		if e.Kind == ledger.KindPost {
			out = append(out, e)
		}
		return nil
	})
	return out
}

// rawSource 是一個不管 window、把整條鏈都倒出來的 Source，模擬一個會把還沒 finalized 的錢算進來的壞 adapter。
type rawSource struct{ c *fakeChain }

func (r rawSource) Finalized(ctx context.Context) (uint64, error) { return r.c.Finalized(ctx) }
func (r rawSource) Transfers(_ context.Context, _, _ uint64) ([]recon.Transfer, error) {
	return append([]recon.Transfer(nil), r.c.transfers...), nil
}

func kinds(rep recon.Report) []recon.Kind {
	var out []recon.Kind
	for _, f := range rep.Findings {
		out = append(out, f.Kind)
	}
	return out
}

// TestRun_SweepEnqueuesTheIntentsNobodyIsDriving：authorized 與 settling 的 intent 若 queue 裡沒有它的 job，鏈下掃描替它丟一份；
// 第二次 Run 那份還在排隊，Enqueue 是 no-op。這條釘的是 recon 與 queue 之間「Enqueue 對同 ID 冪等」的約定。
func TestRun_SweepEnqueuesTheIntentsNobodyIsDriving(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.intent(t, "pi_0001", "evm:31337", intent.StateAuthorized, "")
	w.intent(t, "pi_0002", "evm:31337", intent.StateSettling, "")
	w.intent(t, "pi_0003", "evm:31337", intent.StateCreated, "")
	e := w.engine()

	rep := w.run(t, e)
	if len(rep.Sweeps) != 2 || rep.Sweeps[0].Action != "enqueued pi_0001/settle" || rep.Sweeps[1].Action != "enqueued pi_0002/settle" {
		t.Fatalf("sweeps: %v", rep.Sweeps)
	}
	if n, _ := w.jobs.Len(ctx); n != 2 {
		t.Fatalf("queue has %d jobs, want 2", n)
	}
	rep = w.run(t, e)
	if len(rep.Sweeps) != 2 || rep.Sweeps[0].Action != "already queued" || rep.Sweeps[1].Action != "already queued" {
		t.Fatalf("second run: %v", rep.Sweeps)
	}
	if n, _ := w.jobs.Len(ctx); n != 2 {
		t.Fatalf("queue has %d jobs after the second run, want 2", n)
	}
}

// TestRun_SweepLeavesAParkedJobToAPerson：停在 dlq 裡的 job 不再丟回 queue。放棄是有理由的，再丟一份只是把 poison 的迴圈拉長；
// 能放它回去的只有人工介入。這條釘的是 recon 與 dlq 之間的分工。
func TestRun_SweepLeavesAParkedJobToAPerson(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	it := w.intent(t, "pi_0001", "evm:31337", intent.StateAuthorized, "")
	if _, err := w.dead.Park(ctx, dlq.Record{Job: queue.Job{ID: "pi_0001/settle", Kind: queue.KindSettle, IntentID: "pi_0001", Ref: it.Ref},
		Attempts: 3, Reason: "no signing key for this chain", IntentState: "authorized"}, t0); err != nil {
		t.Fatal(err)
	}
	rep := w.run(t, w.engine())
	if len(rep.Sweeps) != 1 || !strings.Contains(rep.Sweeps[0].Action, "dlq") {
		t.Fatalf("sweeps: %v", rep.Sweeps)
	}
	if n, _ := w.jobs.Len(ctx); n != 0 {
		t.Fatalf("queue has %d jobs, want none", n)
	}
	// 人把它 Drop 掉之後就不再是 parked，下一次掃描照常丟。
	if _, err := w.dead.Resolve(ctx, "pi_0001/settle", dlq.StatusDropped, "ops", t0); err != nil {
		t.Fatal(err)
	}
	rep = w.run(t, w.engine())
	if rep.Sweeps[0].Action != "enqueued pi_0001/settle" {
		t.Fatalf("after drop: %v", rep.Sweeps)
	}
}

// TestRun_SweepHandsConfirmingToTheListener：confirming 的 intent 每一次 Run 都交給 listener.Check 一次；
// 還沒 finalized 就是 wait，finalized 了就 settled。這就是「誰把 confirming 交給 listener」的答案。
func TestRun_SweepHandsConfirmingToTheListener(t *testing.T) {
	w := newWorld(t)
	it := w.intent(t, "pi_0001", "evm:31337", intent.StateConfirming, "0x0001")
	w.chain.final = 99
	w.transfer("0x0001", 100, it.Ref)
	e := w.engine()

	rep := w.run(t, e)
	if len(rep.Sweeps) != 1 || !strings.HasPrefix(rep.Sweeps[0].Action, "wait") {
		t.Fatalf("not final: %v", rep.Sweeps)
	}
	w.chain.final = 164
	rep = w.run(t, e)
	if !strings.HasPrefix(rep.Sweeps[0].Action, "settled") || w.get(t, "pi_0001").State != intent.StateSettled {
		t.Fatalf("final: %v, %s", rep.Sweeps, w.get(t, "pi_0001").State)
	}
	if len(rep.Matches) != 1 || rep.Matches[0].Action != "settled, post matches the chain" {
		t.Fatalf("the window should then find it settled: %v", rep.Matches)
	}
}

// TestRun_IgnoresOtherChains：一個 Engine 管一條鏈。別條鏈的 intent 不掃、不丟 job；別條鏈的 ref 出現在這條鏈上是 unexpected。
func TestRun_IgnoresOtherChains(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.intent(t, "pi_0001", "solana:mainnet", intent.StateAuthorized, "")
	it := w.intent(t, "pi_0002", "solana:mainnet", intent.StateSettling, "")
	w.transfer("0x0002", 100, it.Ref)
	rep := w.run(t, w.engine())
	if len(rep.Sweeps) != 0 {
		t.Fatalf("sweeps: %v", rep.Sweeps)
	}
	if n, _ := w.jobs.Len(ctx); n != 0 {
		t.Fatalf("queue has %d jobs, want none", n)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != recon.KindUnexpected || w.get(t, "pi_0002").State != intent.StateSettling {
		t.Fatalf("findings: %v", rep.Findings)
	}
}

// TestRun_MatchesASettledPostByRef：鏈上那筆轉帳的 ref 找到一筆 settled 的 intent、帳上的 post 指著同一筆交易、
// merchant 那條腿的金額跟鏈上一樣：對得上，沒有 Finding，intent 與帳本都不動。
func TestRun_MatchesASettledPostByRef(t *testing.T) {
	w := newWorld(t)
	it := w.intent(t, "pi_0001", "evm:31337", intent.StateSettled, "0x0001")
	w.transfer("0x0001", 100, it.Ref)
	rep := w.run(t, w.engine())
	if len(rep.Matches) != 1 || rep.Matches[0].IntentID != "pi_0001" || len(rep.Findings) != 0 {
		t.Fatalf("matches %v findings %v", rep.Matches, rep.Findings)
	}
	if w.get(t, "pi_0001").Version != it.Version || len(w.posts(t)) != 1 {
		t.Fatal("a match must not touch the intent or the books")
	}
}

// TestRun_FillsInTheHashForASettlingIntent：relayer 送出去之後、寫回 confirming 之前死掉，intent 停在 settling 沒有 hash。
// 鏈上帶著它的 ref 的那筆交易就是證據：推到 confirming（listener 的權限）、交給 listener、settled，post 只有一筆。
func TestRun_FillsInTheHashForASettlingIntent(t *testing.T) {
	w := newWorld(t)
	it := w.intent(t, "pi_0001", "evm:31337", intent.StateSettling, "")
	w.transfer("0x0001", 100, it.Ref)
	rep := w.run(t, w.engine())
	if len(rep.Matches) != 1 || rep.Matches[0].Action != "settling -> confirming -> settled (finalized at 100, 65 deep)" {
		t.Fatalf("matches: %v", rep.Matches)
	}
	got := w.get(t, "pi_0001")
	if got.State != intent.StateSettled || got.TxHash != "0x0001" {
		t.Fatalf("intent: %s tx=%s", got.State, got.TxHash)
	}
	step := got.History[len(got.History)-2]
	if step.From != intent.StateSettling || step.To != intent.StateConfirming || step.By != intent.ActorListener || step.TxHash != "0x0001" {
		t.Fatalf("the evidence step: %s", step)
	}
	if posts := w.posts(t); len(posts) != 1 || posts[0].TxHash != "0x0001" {
		t.Fatalf("posts: %v", posts)
	}
}

// TestRun_SwapsTheHashWhenTheChainShowsAnotherTransaction：intent 記著最後一次送出去的 0x000a，鏈上帶著 ref 的卻是 0x000b
// （替換之後舊那筆贏了）。listener 不能拿 0x000b 宣告 settled，所以先走回頭路退回 settling、再帶著 0x000b 進 confirming，
// 兩步都留在 History 上，最後 settled 在 0x000b。
func TestRun_SwapsTheHashWhenTheChainShowsAnotherTransaction(t *testing.T) {
	w := newWorld(t)
	// 0x000a 還沒老到被判 lost（超過 LostAfter 的話，鏈下掃描會先走回頭路把 hash 清掉，match 就走 settling 那條，結果一樣）。
	w.now = t0.Add(2 * time.Minute)
	it := w.intent(t, "pi_0001", "evm:31337", intent.StateConfirming, "0x000a")
	w.transfer("0x000b", 100, it.Ref)
	rep := w.run(t, w.engine())
	// 鏈下掃描先問過一次 0x000a：它不在任何區塊裡，還年輕，wait。
	if len(rep.Sweeps) != 1 || !strings.HasPrefix(rep.Sweeps[0].Action, "wait") {
		t.Fatalf("sweeps: %v", rep.Sweeps)
	}
	if len(rep.Matches) != 1 || rep.Matches[0].Action != "on record 0x000a -> settling -> confirming -> settled (finalized at 100, 65 deep)" {
		t.Fatalf("matches: %v", rep.Matches)
	}
	got := w.get(t, "pi_0001")
	if got.State != intent.StateSettled || got.TxHash != "0x000b" {
		t.Fatalf("intent: %s tx=%s", got.State, got.TxHash)
	}
	h := got.History
	back, forth := h[len(h)-3], h[len(h)-2]
	if back.To != intent.StateSettling || !strings.Contains(back.Reason, "0x000a") || !strings.Contains(back.Reason, "0x000b") {
		t.Fatalf("the way back: %s", back)
	}
	if forth.To != intent.StateConfirming || forth.TxHash != "0x000b" || forth.By != intent.ActorListener {
		t.Fatalf("the way forward: %s", forth)
	}
	if posts := w.posts(t); len(posts) != 1 || posts[0].TxHash != "0x000b" {
		t.Fatalf("posts: %v", posts)
	}
}

// TestRun_FlagsAnUnknownRef：帶著一個 intent store 裡沒有的 ref。不猜、不記帳，列成 unknown_ref。
func TestRun_FlagsAnUnknownRef(t *testing.T) {
	w := newWorld(t)
	stranger := paymentref.Derive(paymentref.Terms{IntentID: "pi_9999", Chain: "evm:31337", Token: usdc, Payer: payer, Merchant: merchant, Amount: "1"})
	w.transfer("0x0009", 100, stranger)
	rep := w.run(t, w.engine())
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != recon.KindUnknownRef || rep.Findings[0].IntentID != "" {
		t.Fatalf("findings: %v", rep.Findings)
	}
	if len(w.posts(t)) != 0 {
		t.Fatal("an unknown ref must not be posted")
	}
}

// TestRun_FlagsAnUnreferencedTransfer：打到 merchant、沒帶 ref。跟交易所對「沒填 memo 的入金」一樣：不自動入帳，列出來給人。
func TestRun_FlagsAnUnreferencedTransfer(t *testing.T) {
	w := newWorld(t)
	w.transfer("0x0007", 100, paymentref.Ref{})
	rep := w.run(t, w.engine())
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != recon.KindUnreferenced || !strings.Contains(rep.Findings[0].Detail, "without a ref") {
		t.Fatalf("findings: %v", rep.Findings)
	}
}

// TestRun_FlagsTheSecondTransferWithTheSameRef：同一個 ref 在兩筆交易裡都動了錢。先進區塊的那筆算數、把 intent 走完，
// 第二筆重讀到的是 settled，列成 paid_twice；帳上只有一筆 post，因為一筆 hold 只能收尾一次。
func TestRun_FlagsTheSecondTransferWithTheSameRef(t *testing.T) {
	w := newWorld(t)
	it := w.intent(t, "pi_0001", "evm:31337", intent.StateSettling, "")
	w.transfer("0x0002", 101, it.Ref) // 放上鏈的順序跟高度相反，確認對帳照高度排
	w.transfer("0x0001", 100, it.Ref)
	rep := w.run(t, w.engine())
	if len(rep.Matches) != 1 || rep.Matches[0].Transfer.TxHash != "0x0001" {
		t.Fatalf("matches: %v", rep.Matches)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != recon.KindPaidTwice || rep.Findings[0].Transfer.TxHash != "0x0002" ||
		!strings.Contains(rep.Findings[0].Detail, "already settled on tx 0x0001") {
		t.Fatalf("findings: %v", rep.Findings)
	}
	if len(w.posts(t)) != 1 {
		t.Fatalf("got %d posts, want exactly one", len(w.posts(t)))
	}
}

// TestRun_FlagsMoneyMovingForAClosedIntent：failed、needs_review、authorized 的 intent 都不在等錢，錢卻動了。
// 對帳引擎不替它們收尾（failed 是 terminal，needs_review 只有 operator 走得動），列成 unexpected，intent 一個欄位都不動。
func TestRun_FlagsMoneyMovingForAClosedIntent(t *testing.T) {
	w := newWorld(t)
	for i, st := range []intent.State{intent.StateFailed, intent.StateNeedsReview, intent.StateAuthorized} {
		id := "pi_000" + string(rune('1'+i))
		it := w.intent(t, id, "evm:31337", st, "")
		w.transfer("0x000"+string(rune('1'+i)), 100+uint64(i), it.Ref)
	}
	before := map[string]uint64{}
	for _, id := range []string{"pi_0001", "pi_0002", "pi_0003"} {
		before[id] = w.get(t, id).Version
	}
	rep := w.run(t, w.engine())
	if got := kinds(rep); len(got) != 3 || got[0] != recon.KindUnexpected || got[1] != recon.KindUnexpected || got[2] != recon.KindUnexpected {
		t.Fatalf("findings: %v", rep.Findings)
	}
	for _, f := range rep.Findings {
		if !strings.Contains(f.Detail, "yet the money moved") || w.get(t, f.IntentID).Version != before[f.IntentID] {
			t.Fatalf("%s: %s, version moved", f.IntentID, f.Detail)
		}
	}
	if len(w.posts(t)) != 0 {
		t.Fatal("nothing may be posted")
	}
}

// TestRun_FlagsATransferThatDoesNotLookLikeThePayment：ref 對得上，但收款人不是 intent 上的 merchant。
// ref 誰都抄得走，補證據的條件比叫人嚴：不推 intent、不交給 listener，列成 unexpected。
func TestRun_FlagsATransferThatDoesNotLookLikeThePayment(t *testing.T) {
	w := newWorld(t)
	it := w.intent(t, "pi_0001", "evm:31337", intent.StateSettling, "")
	tr := w.transfer("0x0001", 100, it.Ref)
	w.chain.transfers[0].To = "0xa0Ee7A142d267C1f36714E4a8F75612F20a79720"
	rep := w.run(t, w.engine())
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != recon.KindUnexpected || !strings.Contains(rep.Findings[0].Detail, "differ from the intent") {
		t.Fatalf("findings: %v", rep.Findings)
	}
	if got := w.get(t, "pi_0001"); got.State != intent.StateSettling || got.TxHash != "" {
		t.Fatalf("intent moved: %s tx=%s (transfer %s)", got.State, got.TxHash, tr.TxHash)
	}
}

// TestRun_FlagsAPostThatDisagreesWithTheChain：settled、post 指著同一筆交易，但 merchant 那條腿記的跟鏈上動的不一樣。
// listener 記 post 之前比過金額，所以走到這裡是 adapter 或帳本出了問題，列成 mismatch。
func TestRun_FlagsAPostThatDisagreesWithTheChain(t *testing.T) {
	w := newWorld(t)
	it := w.intent(t, "pi_0001", "evm:31337", intent.StateSettled, "0x0001")
	w.transfer("0x0001", 100, it.Ref)
	w.chain.transfers[0].Amount = big.NewInt(99_900_000)
	rep := w.run(t, w.engine())
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != recon.KindMismatch || !strings.Contains(rep.Findings[0].Detail, "posted 100000000, chain says 99900000") {
		t.Fatalf("findings: %v", rep.Findings)
	}
}

// TestRun_OnlyReconcilesFinalizedBlocks：window 的上界是 finalized 的高度，cursor 只走到那裡。一筆還沒 finalized 的轉帳這一次不看；
// finalized 追上之後的下一次 Run 從 cursor+1 接著對，它才出現，而且前一段不會再對一次。
func TestRun_OnlyReconcilesFinalizedBlocks(t *testing.T) {
	w := newWorld(t)
	it1 := w.intent(t, "pi_0001", "evm:31337", intent.StateSettled, "0x0001")
	it2 := w.intent(t, "pi_0002", "evm:31337", intent.StateSettled, "0x0002")
	w.transfer("0x0001", 100, it1.Ref)
	w.transfer("0x0002", 200, it2.Ref)
	e := w.engine()

	rep := w.run(t, e)
	if rep.From != 1 || rep.To != 164 || len(rep.Matches) != 1 || rep.Matches[0].Transfer.TxHash != "0x0001" || e.Cursor() != 164 {
		t.Fatalf("first run: %d..%d matches %v cursor %d", rep.From, rep.To, rep.Matches, e.Cursor())
	}
	rep = w.run(t, e)
	if rep.To >= rep.From || len(rep.Matches) != 0 || e.Cursor() != 164 {
		t.Fatalf("nothing new: %d..%d matches %v cursor %d", rep.From, rep.To, rep.Matches, e.Cursor())
	}
	w.chain.final = 200
	rep = w.run(t, e)
	if rep.From != 165 || rep.To != 200 || len(rep.Matches) != 1 || rep.Matches[0].Transfer.TxHash != "0x0002" || e.Cursor() != 200 {
		t.Fatalf("caught up: %d..%d matches %v cursor %d", rep.From, rep.To, rep.Matches, e.Cursor())
	}
}

// TestRun_ReplayingAWindowIsANoop：同一段 window 對兩次（另一個 Engine 從 cursor 0 再對一次）要得到同一份 Finding，
// intent 與帳本一個欄位都不多。這條釘的是「重跑必須是 no-op」那條紀律。
func TestRun_ReplayingAWindowIsANoop(t *testing.T) {
	w := newWorld(t)
	it1 := w.intent(t, "pi_0001", "evm:31337", intent.StateSettling, "")
	it2 := w.intent(t, "pi_0002", "evm:31337", intent.StateFailed, "")
	w.transfer("0x0001", 100, it1.Ref)
	w.transfer("0x0002", 101, it2.Ref)
	w.transfer("0x0003", 102, paymentref.Ref{})

	first := w.run(t, w.engine())
	v1 := w.get(t, "pi_0001").Version
	second := w.run(t, w.engine())
	if len(first.Findings) != 2 || len(second.Findings) != 2 {
		t.Fatalf("findings: first %v second %v", first.Findings, second.Findings)
	}
	for i := range first.Findings {
		if first.Findings[i].String() != second.Findings[i].String() {
			t.Fatalf("finding %d differs: %s / %s", i, first.Findings[i], second.Findings[i])
		}
	}
	if second.Matches[0].Action != "settled, post matches the chain" || w.get(t, "pi_0001").Version != v1 || len(w.posts(t)) != 1 {
		t.Fatalf("replay touched something: %v v%d posts %d", second.Matches, w.get(t, "pi_0001").Version, len(w.posts(t)))
	}
}

// TestRun_TwoEnginesRaceOverTheSameWindow：兩個 Engine（兩個副本、或排程重疊）同時對同一段 window。
// 補證據的那幾步靠 CAS，輸的那個記成 lost the race；最後 intent settled 一次、post 一筆。
func TestRun_TwoEnginesRaceOverTheSameWindow(t *testing.T) {
	w := newWorld(t)
	it := w.intent(t, "pi_0001", "evm:31337", intent.StateSettling, "")
	w.transfer("0x0001", 100, it.Ref)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.engine().Run(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := w.get(t, "pi_0001"); got.State != intent.StateSettled || got.TxHash != "0x0001" {
		t.Fatalf("intent: %s tx=%s", got.State, got.TxHash)
	}
	if len(w.posts(t)) != 1 {
		t.Fatalf("got %d posts, want exactly one", len(w.posts(t)))
	}
}

// TestRun_DoesNotAdvanceTheCursorOnError：Source 回了一筆在 window 之外的轉帳，是會把還沒 finalized 的錢算進來的那種 bug：
// 整次 Run 回錯、cursor 不動，下一次整段重來。
func TestRun_DoesNotAdvanceTheCursorOnError(t *testing.T) {
	w := newWorld(t)
	it := w.intent(t, "pi_0001", "evm:31337", intent.StateSettled, "0x0001")
	w.transfer("0x0001", 100, it.Ref)
	w.transfer("0x0002", 50, it.Ref) // cursor 已經走過 90，這一筆不該再出現
	l := listener.New(w.intents, w.journal, w.chain, listener.WithClock(func() time.Time { return w.now }))
	e := recon.New("evm:31337", recon.Deps{Intents: w.intents, Journal: w.journal, Jobs: w.jobs, Dead: w.dead,
		Listener: l, Source: rawSource{w.chain}}, recon.WithCursor(90), recon.WithClock(func() time.Time { return w.now }))
	_, err := e.Run(context.Background())
	if !errors.Is(err, recon.ErrOutsideWindow) || e.Cursor() != 90 {
		t.Fatalf("got %v, cursor %d; want ErrOutsideWindow and cursor 90", err, e.Cursor())
	}
}

// TestStrings：Example 與 log 印的那三種行，欄位對齊、內容照填。
func TestStrings(t *testing.T) {
	tr := recon.Transfer{TxHash: "0x0008", Amount: big.NewInt(1)}
	cases := []struct{ got, want string }{
		{recon.Finding{Kind: recon.KindPaidTwice, Transfer: tr, Detail: "ref 0xb02f8d29… (pi_0001) already settled on tx 0x0001"}.String(),
			"paid_twice   tx 0x0008 ref 0xb02f8d29… (pi_0001) already settled on tx 0x0001"},
		{recon.Match{Transfer: tr, IntentID: "pi_0001", Action: "settled, post matches the chain"}.String(),
			"0x0008 pi_0001  settled, post matches the chain"},
		{recon.Sweep{IntentID: "pi_0003", State: intent.StateAuthorized, Action: "enqueued pi_0003/settle"}.String(),
			"pi_0003  authorized   enqueued pi_0003/settle"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Fatalf("got %q, want %q", c.got, c.want)
		}
	}
}

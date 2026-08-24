package listener

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	usdc     = "0x5FbDB2315678afecb367f032d93F642f64180aa3"
	payer    = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	merchant = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
)

// fixture 是一個已經走到 confirming 的世界：intent 在 confirming、帳上有一筆 pending 的 hold、tx hash 是 0x<id 後四碼>。
type fixture struct {
	intents  *intent.MemoryStore
	journal  *ledger.MemoryJournal
	sighting Sighting
	now      time.Time
	looked   int
}

func newFixture(t *testing.T, id, chain string) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{intents: intent.NewMemoryStore(), journal: ledger.NewMemoryJournal(), now: t0}
	it, err := intent.New(intent.Spec{ID: id, Chain: chain, Token: usdc, Payer: payer, Merchant: merchant, Amount: big.NewInt(100_000_000)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.intents.Save(ctx, it, 0); err != nil {
		t.Fatal(err)
	}
	f.advance(t, id, intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: t0})
	it = f.advance(t, id, intent.Request{To: intent.StateSettling, By: intent.ActorRelayer, At: t0})
	if _, _, err := f.journal.Append(ctx, ledger.Entry{ID: id + "/hold", Ref: it.Ref, Kind: ledger.KindHold,
		Asset: ledger.Asset{Chain: chain, Token: usdc},
		Legs: []ledger.Leg{{Account: ledger.PayerAccount(payer), Amount: big.NewInt(-100_000_000)},
			{Account: ledger.MerchantAccount(merchant), Amount: big.NewInt(100_000_000)}},
		By: "relayer", At: it.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	f.advance(t, id, intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: "0x" + id[3:], At: t0})
	f.sighting = Sighting{Received: big.NewInt(100_000_000)}
	f.sighting.Included, f.sighting.Height, f.sighting.Head, f.sighting.Succeeded = true, 100, 100, true
	return f
}

func (f *fixture) advance(t *testing.T, id string, req intent.Request) *intent.Intent {
	t.Helper()
	it, _, err := intent.Advance(context.Background(), f.intents, id, req)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (f *fixture) listener(opts ...Option) *Listener {
	watcher := WatcherFunc(func(_ context.Context, _ *intent.Intent) (Sighting, error) {
		f.looked++
		return f.sighting, nil
	})
	opts = append([]Option{WithClock(func() time.Time { return f.now })}, opts...)
	return New(f.intents, f.journal, watcher, opts...)
}

func (f *fixture) state(t *testing.T, id string) *intent.Intent {
	t.Helper()
	it, err := f.intents.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (f *fixture) posts(t *testing.T) []ledger.Entry {
	t.Helper()
	var out []ledger.Entry
	_ = f.journal.Scan(context.Background(), func(e ledger.Entry) error {
		if e.Kind == ledger.KindPost {
			out = append(out, e)
		}
		return nil
	})
	return out
}

// TestCheck_WaitsThenSettlesAndPostsWhatArrived：進區塊只是等，finalized 才記 post、推 settled；post 的腿記實收金額、
// 收掉那筆 hold，merchant 的餘額從 pending 搬到 posted。
func TestCheck_WaitsThenSettlesAndPostsWhatArrived(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "pi_0001", "evm:31337")
	l := f.listener()

	rep, err := l.Check(ctx, "pi_0001")
	if err != nil || rep.Outcome != OutcomeWait {
		t.Fatalf("not final yet: got %s, %v; want wait", rep, err)
	}
	if got := f.state(t, "pi_0001"); got.State != intent.StateConfirming || got.Version != 4 {
		t.Fatalf("wait must not touch the intent: %s v%d", got.State, got.Version)
	}

	f.sighting.Head, f.sighting.Final = 164, true
	rep, err = l.Check(ctx, "pi_0001")
	if err != nil || rep.Outcome != OutcomeSettled || rep.TxHash != "0x0001" {
		t.Fatalf("final: got %s, %v; want settled", rep, err)
	}
	it := f.state(t, "pi_0001")
	if it.State != intent.StateSettled || it.TxHash != "0x0001" {
		t.Fatalf("intent: %s tx=%s", it.State, it.TxHash)
	}
	posts := f.posts(t)
	if len(posts) != 1 || posts[0].Holds != "pi_0001/hold" || posts[0].TxHash != "0x0001" || posts[0].By != "listener" {
		t.Fatalf("posts: %+v", posts)
	}
	b, _ := f.journal.Balance(ctx, ledger.MerchantAccount(merchant), ledger.Asset{Chain: "evm:31337", Token: usdc})
	if b.Pending.Sign() != 0 || b.Posted.Int64() != 100_000_000 {
		t.Fatalf("balance: %s", b)
	}
}

// TestCheck_ReplaysAfterDyingBetweenPostAndSettled：post 記完、settled 還沒寫回就死掉，下一次 Check 要能安靜地走完：
// post 對同 ID 是 no-op（At 用 intent 進 confirming 的時間，所以兩次算出來一模一樣），不會撞到 ErrConflict，
// journal 裡也只會有一筆 post。這條釘的是 listener 與 ledger 之間「post 必須可重放」的約定。
func TestCheck_ReplaysAfterDyingBetweenPostAndSettled(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "pi_0001", "evm:31337")
	f.sighting.Head, f.sighting.Final = 164, true
	// 上一次死在 post 之後：帳上已經有 post，intent 還在 confirming。
	if _, _, err := f.journal.Append(ctx, postEntry(f.state(t, "pi_0001"), big.NewInt(100_000_000))); err != nil {
		t.Fatal(err)
	}
	// 而且時鐘已經走了：如果 post 的 At 用 listener 的時鐘，這一次算出來的 post 就會跟上一次不同。
	f.now = t0.Add(time.Hour)

	rep, err := f.listener().Check(ctx, "pi_0001")
	if err != nil || rep.Outcome != OutcomeSettled {
		t.Fatalf("got %s, %v; want settled", rep, err)
	}
	if posts := f.posts(t); len(posts) != 1 {
		t.Fatalf("got %d posts, want exactly one", len(posts))
	}
}

// TestCheck_HandsTheIntentBackWhenTheTransactionIsLost：交易太久不在任何區塊裡就退回 settling：tx hash 清掉、
// hold 留在 pending、理由帶著那個 hash。LostAfter 之前只是等。
func TestCheck_HandsTheIntentBackWhenTheTransactionIsLost(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "pi_0002", "evm:31337")
	f.sighting.Included = false
	l := f.listener()

	f.now = t0.Add(4 * time.Minute)
	if rep, _ := l.Check(ctx, "pi_0002"); rep.Outcome != OutcomeWait {
		t.Fatalf("young: got %s, want wait", rep)
	}
	f.now = t0.Add(5 * time.Minute)
	rep, err := l.Check(ctx, "pi_0002")
	if err != nil || rep.Outcome != OutcomeHandedBack {
		t.Fatalf("old: got %s, %v; want settling", rep, err)
	}
	it := f.state(t, "pi_0002")
	last := it.History[len(it.History)-1]
	if it.State != intent.StateSettling || it.TxHash != "" || !strings.Contains(last.Reason, "tx 0x0002") {
		t.Fatalf("intent: %s tx=%q reason=%q", it.State, it.TxHash, last.Reason)
	}
	if len(f.posts(t)) != 0 {
		t.Fatal("a lost transaction must not be posted")
	}
}

// TestCheck_ReviewsAGhostPayment：交易 finalized、執行成功，但裡面沒有帶我們 ref 的轉帳。錢沒動就湊不出第二條腿，
// listener 不記 post、也不敢宣告 settled，只能送審；hold 留在 pending 等人。
func TestCheck_ReviewsAGhostPayment(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "pi_0003", "evm:31337")
	f.sighting.Head, f.sighting.Final, f.sighting.Received = 164, true, nil

	rep, err := f.listener().Check(ctx, "pi_0003")
	if err != nil || rep.Outcome != OutcomeReview || !strings.Contains(rep.Detail, "nothing moved") {
		t.Fatalf("got %s, %v; want needs_review", rep, err)
	}
	if it := f.state(t, "pi_0003"); it.State != intent.StateNeedsReview || it.TxHash != "0x0003" {
		t.Fatalf("intent: %s tx=%s", it.State, it.TxHash)
	}
	if len(f.posts(t)) != 0 {
		t.Fatal("a ghost payment must not be posted")
	}
}

// TestCheck_ReviewsAnAmountMismatch：實收比請款少（轉帳稅）。差額該落在 fee 那條腿，但那是 operator 判定之後的事，
// 今天 listener 一律送審，理由裡把兩個數字都寫出來。
func TestCheck_ReviewsAnAmountMismatch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "pi_0004", "evm:31337")
	f.sighting.Head, f.sighting.Final, f.sighting.Received = 164, true, big.NewInt(99_900_000)

	rep, err := f.listener().Check(ctx, "pi_0004")
	if err != nil || rep.Outcome != OutcomeReview || !strings.Contains(rep.Detail, "received 99900000, expected 100000000") {
		t.Fatalf("got %s, %v; want needs_review with both amounts", rep, err)
	}
	if len(f.posts(t)) != 0 {
		t.Fatal("a mismatch must not be posted")
	}
}

// TestCheck_ReviewsARevertOnlyOnceFinal：revert 的交易在 finalized 之前跟成功的交易一樣可能被換掉，所以先等；
// finalized 了才送審，而且不記 post。
func TestCheck_ReviewsARevertOnlyOnceFinal(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "pi_0005", "evm:31337")
	f.sighting.Succeeded = false
	l := f.listener()

	if rep, _ := l.Check(ctx, "pi_0005"); rep.Outcome != OutcomeWait {
		t.Fatalf("reverted but not final: got %s, want wait", rep)
	}
	f.sighting.Head, f.sighting.Final = 164, true
	rep, err := l.Check(ctx, "pi_0005")
	if err != nil || rep.Outcome != OutcomeReview || !strings.Contains(rep.Detail, "execution failed") {
		t.Fatalf("reverted and final: got %s, %v; want needs_review", rep, err)
	}
	if len(f.posts(t)) != 0 {
		t.Fatal("a reverted transaction must not be posted")
	}
}

// TestCheck_IsANoopOutsideConfirming：不在 confirming 的 intent 連鏈都不問。同一筆被看兩次，第二次就是這條。
func TestCheck_IsANoopOutsideConfirming(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "pi_0006", "evm:31337")
	f.sighting.Head, f.sighting.Final = 164, true
	l := f.listener()
	if _, err := l.Check(ctx, "pi_0006"); err != nil {
		t.Fatal(err)
	}
	looked := f.looked
	rep, err := l.Check(ctx, "pi_0006")
	if err != nil || rep.Outcome != OutcomeNoop || rep.Detail != "already settled" {
		t.Fatalf("got %s, %v; want no-op", rep, err)
	}
	if f.looked != looked {
		t.Fatal("a no-op must not ask the chain")
	}
}

// TestCheck_RefusesAChainWithoutAPolicy：沒設定規則的鏈不猜，回 ErrNoPolicy，intent 不動。
func TestCheck_RefusesAChainWithoutAPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "pi_0007", "tron:1")
	f.sighting.Head, f.sighting.Final = 164, true
	_, err := f.listener().Check(ctx, "pi_0007")
	if !errors.Is(err, ErrNoPolicy) {
		t.Fatalf("got %v, want ErrNoPolicy", err)
	}
	if it := f.state(t, "pi_0007"); it.State != intent.StateConfirming {
		t.Fatalf("intent moved to %s", it.State)
	}
}

// TestCheck_UsesThePolicyOfTheIntentsProtocol：規則以協定名為 key。把 evm 換成只數 2 個區塊的判斷標準之後，
// 同一個 Observation 在 evm:31337 上是 settled，在 solana:mainnet 上照預設還在等 finalized。
func TestCheck_UsesThePolicyOfTheIntentsProtocol(t *testing.T) {
	ctx := context.Background()
	fast := finality.Policy{Confirmations: 2, LostAfter: 5 * time.Minute}
	for _, c := range []struct {
		chain string
		want  Outcome
	}{{"evm:31337", OutcomeSettled}, {"solana:mainnet", OutcomeWait}} {
		f := newFixture(t, "pi_0008", c.chain)
		f.sighting.Head = 101
		rep, err := f.listener(WithPolicy("evm", fast)).Check(ctx, "pi_0008")
		if err != nil || rep.Outcome != c.want {
			t.Fatalf("%s: got %s, %v; want %s", c.chain, rep, err, c.want)
		}
	}
	if Protocol("evm:31337") != "evm" || Protocol("evm") != "evm" {
		t.Fatal("Protocol should return the part before the colon")
	}
}

// TestCheck_LosesTheRaceToAnotherListener：兩個 listener 看同一筆 intent，晚寫回的那個拿到 ErrVersionConflict，
// 而 journal 裡仍然只有一筆 post（同 ID 同內容是重放）。這條釘的是 listener 與 intent store 之間「Save 是 CAS」的約定。
func TestCheck_LosesTheRaceToAnotherListener(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "pi_0009", "evm:31337")
	f.sighting.Head, f.sighting.Final = 164, true
	other := f.listener()
	raced := false
	watcher := WatcherFunc(func(ctx context.Context, it *intent.Intent) (Sighting, error) {
		if !raced {
			raced = true
			if _, err := other.Check(ctx, it.ID); err != nil { // 另一個 listener 在我們讀完 intent 之後先寫回了
				t.Fatal(err)
			}
		}
		return f.sighting, nil
	})
	l := New(f.intents, f.journal, watcher, WithClock(func() time.Time { return f.now }))
	_, err := l.Check(ctx, "pi_0009")
	if !errors.Is(err, intent.ErrVersionConflict) {
		t.Fatalf("got %v, want ErrVersionConflict", err)
	}
	if posts := f.posts(t); len(posts) != 1 {
		t.Fatalf("got %d posts, want exactly one", len(posts))
	}
	if it := f.state(t, "pi_0009"); it.State != intent.StateSettled {
		t.Fatalf("intent: %s", it.State)
	}
}

// TestReport_String：Example 與 log 印的那一行，欄位對齊、沒有 tx 就不印 tx。
func TestReport_String(t *testing.T) {
	cases := []struct {
		r    Report
		want string
	}{
		{Report{IntentID: "pi_0001", Outcome: OutcomeSettled, TxHash: "0x0001", Detail: "finalized at 100, 65 deep"},
			"pi_0001  settled      tx 0x0001 (finalized at 100, 65 deep)"},
		{Report{IntentID: "pi_0002", Outcome: OutcomeNoop, Detail: "already settling"},
			"pi_0002  no-op        (already settling)"},
		{Report{IntentID: "pi_0003", Outcome: OutcomeWait}, "pi_0003  wait"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Fatalf("got %q, want %q", got, c.want)
		}
	}
}

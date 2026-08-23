package relayer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfail"
)

// shortBudget 把預算縮到三次、關掉抖動，測試才數得清是第幾次交付被判死的。
func shortBudget() txfail.Policy {
	p := txfail.DefaultPolicy()
	p.MaxAttempts, p.Jitter = 3, nil
	return p
}

// drain 一直 RunOnce 到 queue 空掉為止；queue 裡還有東西但現在看不到的話，把時鐘撥過退避的上限再看一次。
func (wd *world) drain(t *testing.T, max int) []Report {
	t.Helper()
	var reps []Report
	for i := 0; i < max; i++ {
		rep, ok, err := wd.w.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if !ok {
			if wd.queueLen(t) == 0 {
				return reps
			}
			wd.tick(wd.w.faults.MaxBackoff)
			continue
		}
		reps = append(reps, rep)
	}
	t.Fatalf("queue never drained, %d job(s) left", wd.queueLen(t))
	return nil
}

// TestPoison_DeclaredErrorFailsTheIntentAndVoidsTheHold：Sender 宣告「重試不會好」而且「確定沒發送出去」，
// 所以錢確定沒動：第一次交付就停，hold 放掉、intent 走 settling -> failed。
func TestPoison_DeclaredErrorFailsTheIntentAndVoidsTheHold(t *testing.T) {
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.sendErr = fmt.Errorf("%w: %w: evm:31337 has no signer configured", ErrNotSent, txfail.ErrPoison)

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomePoison || rep.Attempt != 1 {
		t.Fatalf("report: %s", rep)
	}
	got := wd.state(t, "pi_0001")
	if got.State != intent.StateFailed {
		t.Fatalf("intent: %s", got.State)
	}
	last := got.History[len(got.History)-1]
	if last.By != intent.ActorRelayer || !strings.Contains(last.Reason, "no signer configured") {
		t.Fatalf("history: %s", last)
	}
	v, err := wd.journal.Get(context.Background(), "pi_0001/void")
	if err != nil || v.Kind != ledger.KindVoid || v.Holds != "pi_0001/hold" {
		t.Fatalf("void entry: %+v err=%v", v, err)
	}
	bal, err := wd.journal.Balance(context.Background(), ledger.MerchantAccount(merchant), ledger.Asset{Chain: "evm:31337", Token: usdc})
	if err != nil || bal.Pending.Sign() != 0 || bal.Posted.Sign() != 0 {
		t.Fatalf("balance: %+v err=%v", bal, err)
	}
	if wd.queueLen(t) != 0 {
		t.Fatalf("poisoned job should be gone, %d left", wd.queueLen(t))
	}
}

// TestPoison_UnknownBroadcastGoesToReview：一樣是宣告過的錯誤，但沒有宣告 ErrNotSent，
// 所以那筆交易可能已經在鏈上。錢動了沒說不準的時候不能宣告 failed，推 needs_review，hold 留著。
func TestPoison_UnknownBroadcastGoesToReview(t *testing.T) {
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.sendErr = fmt.Errorf("%w: node rejected the payload", txfail.ErrPoison)

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomePoison || !strings.Contains(rep.Detail, "last broadcast unknown") {
		t.Fatalf("report: %s", rep)
	}
	got := wd.state(t, "pi_0001")
	if got.State != intent.StateNeedsReview {
		t.Fatalf("intent: %s", got.State)
	}
	if _, err := wd.journal.Get(context.Background(), "pi_0001/void"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("hold should still be pending, got %v", err)
	}
}

// TestPoison_BudgetRunsOutOnATransientError：沒有人宣告任何事，就是一直失敗。預算用完的那一次停下來，
// 而且理由要留得住最後一次失敗的細節。
func TestPoison_BudgetRunsOutOnATransientError(t *testing.T) {
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.sendErr = errors.New("rpc: connection refused")

	reps := wd.drain(t, 12)
	if len(reps) != 3 {
		t.Fatalf("want three deliveries, got %d", len(reps))
	}
	if reps[0].Outcome != OutcomeRetry || reps[1].Outcome != OutcomeRetry {
		t.Fatalf("first two: %s / %s", reps[0], reps[1])
	}
	last := reps[2]
	if last.Outcome != OutcomePoison || last.Attempt != 3 ||
		!strings.HasPrefix(last.Detail, "no luck after 3 deliveries") {
		t.Fatalf("third: %s", last)
	}
	got := wd.state(t, "pi_0001")
	if got.State != intent.StateNeedsReview {
		t.Fatalf("intent: %s", got.State)
	}
	if r := got.History[len(got.History)-1].Reason; !strings.Contains(r, "no luck after 3 deliveries") ||
		!strings.Contains(r, "without tx hash") {
		t.Fatalf("reason should carry both the verdict and the last failure: %q", r)
	}
}

// TestPoison_UntouchedIntentKeepsItsState：intent 還沒 authorized，relayer 一個 byte 都沒寫過。
// 這一格不是它的地盤（轉移表上它只有往前推到 settling 這一條出口），所以只丟便條，intent 原封不動。
func TestPoison_UntouchedIntentKeepsItsState(t *testing.T) {
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	it := wd.newIntent(t, "pi_0001", intent.StateCreated)
	wd.enqueue(t, it)

	reps := wd.drain(t, 12)
	last := reps[len(reps)-1]
	if last.Outcome != OutcomePoison || last.Detail != "no luck after 3 deliveries; job dropped, intent still created" {
		t.Fatalf("last: %s", last)
	}
	got := wd.state(t, "pi_0001")
	if got.State != intent.StateCreated || got.Version != 1 {
		t.Fatalf("intent should be untouched: %s v%d", got.State, got.Version)
	}
	if wd.queueLen(t) != 0 {
		t.Fatalf("job should be gone, %d left", wd.queueLen(t))
	}
	// 便條丟掉不代表工作消失：intent 是唯一的真相，authorized 之後再丟一份新的 job 進來照樣被處理。
	if _, _, err := intent.Advance(context.Background(), wd.intents, "pi_0001",
		intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: wd.now()}); err != nil {
		t.Fatal(err)
	}
	wd.enqueue(t, wd.state(t, "pi_0001"))
	if rep := wd.runOnce(t); rep.Outcome != OutcomeSent || rep.Attempt != 1 {
		t.Fatalf("re-enqueued job: %s", rep)
	}
}

// TestPoison_BackoffGrowsBetweenDeliveries：判決算出來的退避要真的傳到 queue。
// 這條在防「判了但沒有人用」：Nack 還是照固定的 RetryAfter 走的話，這裡會看到四個一樣的數字。
func TestPoison_BackoffGrowsBetweenDeliveries(t *testing.T) {
	wd := newWorld(t)
	p := shortBudget()
	p.MaxAttempts = 6
	wd.w.faults = p
	spy := &spyQueue{Queue: wd.q}
	wd.w.queue = spy
	wd.enqueue(t, wd.newIntent(t, "pi_0001", intent.StateCreated))

	for i := 0; i < 4; i++ {
		wd.runOnce(t)
		wd.tick(p.MaxBackoff)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second}
	if !slices.Equal(spy.nacks, want) {
		t.Fatalf("nack delays: got %v, want %v", spy.nacks, want)
	}
}

// spyQueue 記下每一次 Nack 用的延遲。
type spyQueue struct {
	queue.Queue
	nacks []time.Duration
}

func (q *spyQueue) Nack(ctx context.Context, d queue.Delivery, retryAfter time.Duration, now time.Time) error {
	q.nacks = append(q.nacks, retryAfter)
	return q.Queue.Nack(ctx, d, retryAfter, now)
}

// TestPoison_LostRaceIsRetried：判決是 poison，但寫回去之前別人先動了那筆 intent。
// 這一次交付不算數，重讀再說；工作不會因為輸了一次 CAS 就消失。
func TestPoison_LostRaceIsRetried(t *testing.T) {
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.sendErr = fmt.Errorf("%w: node rejected the payload", txfail.ErrPoison)
	ctx := context.Background()
	wd.w.intents = &racingOnPoison{Store: wd.intents, after: 2, once: func() {
		_, _, _ = intent.Advance(ctx, wd.intents, "pi_0001",
			intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: "0xdead", At: wd.now()})
	}}

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeRetry || rep.Detail != "lost the race to needs_review, will re-read" {
		t.Fatalf("report: %s", rep)
	}
	if wd.queueLen(t) != 1 {
		t.Fatal("job should still be in the queue")
	}
}

// racingOnPoison 在第 after 次 Get 之後插一手。判決之後 poison 會重讀一次 intent，
// 所以 after=2 剛好落在「判決已經下了、還沒寫回去」的那個空隙。
type racingOnPoison struct {
	intent.Store
	after int
	n     int
	once  func()
}

func (r *racingOnPoison) Get(ctx context.Context, id string) (*intent.Intent, error) {
	it, err := r.Store.Get(ctx, id)
	if r.n++; err == nil && r.n == r.after {
		r.once()
	}
	return it, err
}

// TestPool_CountsPoisonSeparately：Retry 高只代表這批工作暫時做不動，Poison 不是零就代表有東西被放棄了。
// 兩個數字要分開，不然「一直重試」與「已經放棄」在儀表板上長得一樣。
func TestPool_CountsPoisonSeparately(t *testing.T) {
	wd := newWorld(t)
	p := shortBudget()
	p.MaxAttempts = 1 // 第一次交付就用完
	wd.w.faults = p
	wd.sendErr = errors.New("rpc: connection refused")
	for _, id := range []string{"pi_0001", "pi_0002"} {
		wd.enqueue(t, wd.newIntent(t, id, intent.StateAuthorized))
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := NewPool(wd.w, PoolConfig{Size: 2, Idle: time.Millisecond, DrainTimeout: time.Second})
	go func() {
		for {
			if n, _ := wd.q.Len(context.Background()); n == 0 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	st := pool.Run(ctx)
	if st.Poison != 2 || st.Sent != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

package relayer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
)

const (
	usdc     = "0x5FbDB2315678afecb367f032d93F642f64180aa3"
	payer    = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	merchant = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// world 是一組測試用的依賴：queue、intent store、journal、可控的時鐘、記錄每次 Send 的 fake sender。
type world struct {
	q       *queue.MemoryQueue
	intents *intent.MemoryStore
	journal *ledger.MemoryJournal
	clock   time.Time
	mu      sync.Mutex
	sends   map[string]int // intent id → Send 被叫了幾次
	sendErr error          // 非 nil 時 Send 一律失敗
	w       *Worker
}

func newWorld(t *testing.T) *world {
	t.Helper()
	wd := &world{q: queue.NewMemoryQueue(), intents: intent.NewMemoryStore(), journal: ledger.NewMemoryJournal(), clock: t0, sends: map[string]int{}}
	sender := SenderFunc(func(_ context.Context, it *intent.Intent) (string, error) {
		wd.mu.Lock()
		defer wd.mu.Unlock()
		if wd.sendErr != nil {
			return "", wd.sendErr
		}
		wd.sends[it.ID]++
		return fmt.Sprintf("0x%s%02d", it.ID[3:], wd.sends[it.ID]), nil
	})
	wd.w = New(wd.q, wd.intents, wd.journal, sender, WithClock(wd.now))
	return wd
}

func (wd *world) now() time.Time {
	wd.mu.Lock()
	defer wd.mu.Unlock()
	return wd.clock
}

func (wd *world) tick(d time.Duration) {
	wd.mu.Lock()
	defer wd.mu.Unlock()
	wd.clock = wd.clock.Add(d)
}

// newIntent 建一筆 intent 並推到 state（created 或 authorized），回傳它。
func (wd *world) newIntent(t *testing.T, id string, state intent.State) *intent.Intent {
	t.Helper()
	ctx := context.Background()
	it, err := intent.New(intent.Spec{ID: id, Chain: "evm:31337", Token: usdc, Payer: payer, Merchant: merchant, Amount: big.NewInt(100_000_000)}, wd.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := wd.intents.Save(ctx, it, 0); err != nil {
		t.Fatal(err)
	}
	if state == intent.StateAuthorized {
		if it, _, err = intent.Advance(ctx, wd.intents, id, intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: wd.now()}); err != nil {
			t.Fatal(err)
		}
	}
	return it
}

func (wd *world) enqueue(t *testing.T, it *intent.Intent) {
	t.Helper()
	if _, err := wd.q.Enqueue(context.Background(), queue.Job{ID: it.ID + "/settle", Kind: queue.KindSettle, IntentID: it.ID, Ref: it.Ref}, wd.now()); err != nil {
		t.Fatal(err)
	}
}

func (wd *world) state(t *testing.T, id string) *intent.Intent {
	t.Helper()
	it, err := wd.intents.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (wd *world) runOnce(t *testing.T) Report {
	t.Helper()
	rep, ok, err := wd.w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !ok {
		t.Fatal("RunOnce: queue was empty")
	}
	return rep
}

func (wd *world) queueLen(t *testing.T) int {
	t.Helper()
	n, _ := wd.q.Len(context.Background())
	return n
}

// TestWorker_AuthorizedGoesSettlingHoldSendConfirming：正常路。一份 job 走完之後 intent 在 confirming、帶 tx hash，
// journal 裡有一筆 hold（請款金額進 pending），job 已 Ack。
func TestWorker_AuthorizedGoesSettlingHoldSendConfirming(t *testing.T) {
	wd := newWorld(t)
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeSent || rep.TxHash != "0x000101" {
		t.Fatalf("report: %s", rep)
	}
	got := wd.state(t, "pi_0001")
	if got.State != intent.StateConfirming || got.TxHash != "0x000101" || got.Version != 4 {
		t.Fatalf("intent: state=%s tx=%s v%d", got.State, got.TxHash, got.Version)
	}
	h, err := wd.journal.Get(context.Background(), "pi_0001/hold")
	if err != nil || h.Kind != ledger.KindHold || h.Ref != it.Ref || h.By != "relayer" {
		t.Fatalf("hold: %+v err=%v", h, err)
	}
	b, _ := wd.journal.Balance(context.Background(), ledger.MerchantAccount(merchant), ledger.Asset{Chain: "evm:31337", Token: usdc})
	if b.Pending.Int64() != 100_000_000 || b.Posted.Sign() != 0 {
		t.Fatalf("merchant balance: %s", b)
	}
	if n := wd.queueLen(t); n != 0 {
		t.Fatalf("queue should be empty after ack, got %d", n)
	}
}

// TestWorker_HoldIsRecordedBeforeSend：Send 被叫到的那一刻，intent 已經是 settling、帳上已經有 hold。
// 「先記 settling、再 hold、再廣播」不是文件裡的一句話，是被釘住的順序。
func TestWorker_HoldIsRecordedBeforeSend(t *testing.T) {
	wd := newWorld(t)
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	var seenState intent.State
	var seenHold bool
	wd.w.sender = SenderFunc(func(ctx context.Context, cur *intent.Intent) (string, error) {
		stored, _ := wd.intents.Get(ctx, cur.ID)
		seenState = stored.State
		_, err := wd.journal.Get(ctx, cur.ID+"/hold")
		seenHold = err == nil
		return "0xaa", nil
	})
	wd.runOnce(t)
	if seenState != intent.StateSettling || !seenHold {
		t.Fatalf("at send time: state=%s hold=%v", seenState, seenHold)
	}
}

// TestWorker_RedeliveredJobIsNoop：同一筆 intent 的 job 再來一次（sweeper 重掃、上游重送），worker 看到 confirming 就 no-op、Ack。
// 沒有第二筆 hold、Send 沒有被叫第二次。
func TestWorker_RedeliveredJobIsNoop(t *testing.T) {
	wd := newWorld(t)
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.runOnce(t)

	wd.enqueue(t, it)
	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeNoop || rep.Detail != "already confirming" {
		t.Fatalf("report: %s", rep)
	}
	if wd.sends["pi_0001"] != 1 {
		t.Fatalf("send count: want 1, got %d", wd.sends["pi_0001"])
	}
	n := 0
	_ = wd.journal.Scan(context.Background(), func(ledger.Entry) error { n++; return nil })
	if n != 1 || wd.queueLen(t) != 0 {
		t.Fatalf("journal entries=%d queue=%d", n, wd.queueLen(t))
	}
}

// TestWorker_LeaseExpiryHandsTheJobToAnotherWorker：worker A 領走 job 之後死了（什麼都沒寫），lease 過期後 worker B 領到同一份、
// 正常走完；A 醒來想 Ack 已經沒它的份。
func TestWorker_LeaseExpiryHandsTheJobToAnotherWorker(t *testing.T) {
	wd := newWorld(t)
	ctx := context.Background()
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)

	dead, ok, _ := wd.q.Lease(ctx, wd.now(), DefaultConfig().Lease)
	if !ok {
		t.Fatal("first lease")
	}
	wd.tick(DefaultConfig().Lease)
	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeSent || rep.Attempt != 2 {
		t.Fatalf("report: %s", rep)
	}
	if err := wd.q.Ack(ctx, dead); !errors.Is(err, queue.ErrNotFound) {
		// B 已經 Ack 掉了，A 的憑證連 job 都找不到；還在 queue 裡的話會是 ErrStaleReceipt。
		t.Fatalf("dead worker's ack: %v", err)
	}
}

// TestWorker_CreatedIsRetriedUntilAuthorized：job 比 authorized 早到（或簽名迴圈還沒走完）：retry，job 留在 queue；
// authorized 之後的下一次交付才送。
func TestWorker_CreatedIsRetriedUntilAuthorized(t *testing.T) {
	wd := newWorld(t)
	ctx := context.Background()
	it := wd.newIntent(t, "pi_0001", intent.StateCreated)
	wd.enqueue(t, it)

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeRetry || rep.Detail != "not authorized yet" || wd.queueLen(t) != 1 {
		t.Fatalf("report: %s queue=%d", rep, wd.queueLen(t))
	}
	if _, ok, _ := wd.q.Lease(ctx, wd.now(), time.Second); ok {
		t.Fatal("nacked job should be hidden until RetryAfter")
	}
	if _, _, err := intent.Advance(ctx, wd.intents, it.ID, intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: wd.now()}); err != nil {
		t.Fatal(err)
	}
	wd.tick(DefaultConfig().RetryAfter)
	if rep := wd.runOnce(t); rep.Outcome != OutcomeSent || rep.Attempt != 2 {
		t.Fatalf("second delivery: %s", rep)
	}
}

// TestWorker_CanceledIsNoop：job 排進去之後 API 把 intent 取消了。worker 看到 canceled 就 no-op，不記帳、不送。
func TestWorker_CanceledIsNoop(t *testing.T) {
	wd := newWorld(t)
	ctx := context.Background()
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	if _, _, err := intent.Advance(ctx, wd.intents, it.ID, intent.Request{To: intent.StateCanceled, By: intent.ActorAPI, Reason: "merchant canceled", At: wd.now()}); err != nil {
		t.Fatal(err)
	}
	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeNoop || rep.Detail != "already canceled" || wd.sends["pi_0001"] != 0 {
		t.Fatalf("report: %s sends=%d", rep, wd.sends["pi_0001"])
	}
	if _, err := wd.journal.Get(ctx, "pi_0001/hold"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("no hold expected, got %v", err)
	}
}

// TestWorker_SendFailureIsRetriedThenReviewed：Send 失敗只 Nack；重來的 worker 看到 settling 沒有 tx hash，
// 年輕就等、超過 StuckAfter 就 needs_review。整段過程只有一筆 hold、Send 只被叫過一次：relayer 不敢在不知道上一筆
// 有沒有出門的情況下再送。
func TestWorker_SendFailureIsRetriedThenReviewed(t *testing.T) {
	wd := newWorld(t)
	ctx := context.Background()
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.sendErr = errors.New("rpc: connection refused")
	sendCalls := 0
	inner := wd.w.sender
	wd.w.sender = SenderFunc(func(ctx context.Context, cur *intent.Intent) (string, error) {
		sendCalls++
		return inner.Send(ctx, cur)
	})

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeRetry || rep.Detail != "send: rpc: connection refused" {
		t.Fatalf("first: %s", rep)
	}
	if got := wd.state(t, it.ID); got.State != intent.StateSettling {
		t.Fatalf("after failed send: %s", got.State)
	}
	// RPC 好了也一樣：這一格不重送。
	wd.sendErr = nil
	wd.tick(DefaultConfig().RetryAfter)
	if rep := wd.runOnce(t); rep.Outcome != OutcomeRetry || rep.Attempt != 2 || rep.Detail != "settling for 5s without tx hash, waiting" {
		t.Fatalf("second: %s", rep)
	}
	wd.tick(DefaultConfig().StuckAfter)
	rep = wd.runOnce(t)
	if rep.Outcome != OutcomeReview || rep.Attempt != 3 {
		t.Fatalf("third: %s", rep)
	}
	got := wd.state(t, it.ID)
	last := got.History[len(got.History)-1]
	if got.State != intent.StateNeedsReview || last.By != intent.ActorRelayer || last.Reason == "" {
		t.Fatalf("intent: %s, last=%s", got.State, last)
	}
	if sendCalls != 1 || wd.queueLen(t) != 0 {
		t.Fatalf("sendCalls=%d queue=%d", sendCalls, wd.queueLen(t))
	}
	n := 0
	_ = wd.journal.Scan(ctx, func(ledger.Entry) error { n++; return nil })
	if n != 1 {
		t.Fatalf("journal should hold exactly one hold entry, got %d", n)
	}
}

// TestWorker_LostCASIsRetried：worker 讀到 authorized，寫回 settling 之前 API 把它取消了：CAS 輸掉是 retry，
// 下一次交付重讀就看到 canceled、no-op。錢從頭到尾沒動、帳上沒有 hold。
func TestWorker_LostCASIsRetried(t *testing.T) {
	wd := newWorld(t)
	ctx := context.Background()
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.w.intents = &racingStore{Store: wd.intents, once: func() {
		_, _, _ = intent.Advance(ctx, wd.intents, it.ID, intent.Request{To: intent.StateCanceled, By: intent.ActorAPI, Reason: "merchant canceled", At: wd.now()})
	}}
	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeRetry || rep.Detail != "lost the race to settling, will re-read" {
		t.Fatalf("first: %s", rep)
	}
	wd.tick(DefaultConfig().RetryAfter)
	if rep := wd.runOnce(t); rep.Outcome != OutcomeNoop || rep.Detail != "already canceled" {
		t.Fatalf("second: %s", rep)
	}
	if _, err := wd.journal.Get(ctx, "pi_0001/hold"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("no hold expected, got %v", err)
	}
}

// racingStore 在第一次 Get 之後、Save 之前插一手，模擬別人搶先改了 intent。
type racingStore struct {
	intent.Store
	once func()
	done bool
}

func (r *racingStore) Get(ctx context.Context, id string) (*intent.Intent, error) {
	it, err := r.Store.Get(ctx, id)
	if err == nil && !r.done {
		r.done = true
		r.once()
	}
	return it, err
}

// TestWorker_ManyWorkersSendEachIntentOnce：八個 worker 同時對同一條 queue 跑，五十筆 authorized 的 intent，
// 每一筆恰好被 Send 一次、恰好一筆 hold、最後全部 confirming。這是「錢只動一次」在 relayer 這一層的樣子，
// 靠的是 queue 的 lease 加上每一步冪等，不是靠只跑一個 worker。
func TestWorker_ManyWorkersSendEachIntentOnce(t *testing.T) {
	wd := newWorld(t)
	ctx := context.Background()
	const n = 50
	for i := 1; i <= n; i++ {
		wd.enqueue(t, wd.newIntent(t, fmt.Sprintf("pi_%04d", i), intent.StateAuthorized))
	}
	var sends atomic.Int64
	base := wd.w.sender
	wd.w.sender = SenderFunc(func(ctx context.Context, cur *intent.Intent) (string, error) {
		sends.Add(1)
		return base.Send(ctx, cur)
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if _, ok, err := wd.w.RunOnce(ctx); err != nil {
					t.Error(err)
					return
				} else if !ok {
					return
				}
			}
		}()
	}
	wg.Wait()
	if sends.Load() != n {
		t.Fatalf("sends: want %d, got %d", n, sends.Load())
	}
	holds := 0
	_ = wd.journal.Scan(ctx, func(ledger.Entry) error { holds++; return nil })
	if holds != n {
		t.Fatalf("holds: want %d, got %d", n, holds)
	}
	for i := 1; i <= n; i++ {
		if got := wd.state(t, fmt.Sprintf("pi_%04d", i)); got.State != intent.StateConfirming {
			t.Fatalf("%s: %s", got.ID, got.State)
		}
	}
	if wd.queueLen(t) != 0 {
		t.Fatalf("queue not drained: %d", wd.queueLen(t))
	}
}

// TestWorker_RunStopsOnContext：Run 在 queue 空的時候睡 idle，ctx 取消就回來。observer 收到每一份 job 的 Report。
func TestWorker_RunStopsOnContext(t *testing.T) {
	wd := newWorld(t)
	var reports []Report
	var mu sync.Mutex
	wd.w.observe = func(r Report) { mu.Lock(); reports = append(reports, r); mu.Unlock() }
	wd.enqueue(t, wd.newIntent(t, "pi_0001", intent.StateAuthorized))
	wd.enqueue(t, wd.newIntent(t, "pi_0002", intent.StateAuthorized))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := wd.w.Run(ctx, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 2 || reports[0].Outcome != OutcomeSent || reports[1].Outcome != OutcomeSent {
		t.Fatalf("reports: %v", reports)
	}
}

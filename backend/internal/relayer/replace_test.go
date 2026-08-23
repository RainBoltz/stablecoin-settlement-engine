package relayer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfee"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// evmFake 是一個假的 EVM chain adapter：需要 nonce（OrderedSender），而且換得掉已經送出去的交易（ReplacingSender）。
type evmFake struct {
	mu      sync.Mutex
	sendErr error // 非 nil 時 SendAt 失敗
	replErr error // 非 nil 時 Replace / Cancel 失敗
	calls   map[string]int
	lastFee txfee.Fee
	lastRes txseq.Reservation
}

func newEVMFake() *evmFake { return &evmFake{calls: map[string]int{}} }

func (f *evmFake) Account(*intent.Intent) string { return relayerWallet }

func (f *evmFake) Send(context.Context, *intent.Intent) (string, error) {
	return "", fmt.Errorf("%w: evm transactions need a nonce", ErrNotSent)
}

func (f *evmFake) SendAt(_ context.Context, it *intent.Intent, res txseq.Reservation) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["send"]++
	f.lastRes = res
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return "0x" + it.ID[3:], nil
}

func (f *evmFake) Replace(_ context.Context, it *intent.Intent, res txseq.Reservation, fee txfee.Fee) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["replace"]++
	f.lastRes, f.lastFee = res, fee
	if f.replErr != nil {
		return "", f.replErr
	}
	return "0x" + it.ID[3:] + "r", nil
}

func (f *evmFake) Cancel(_ context.Context, _ string, res txseq.Reservation, fee txfee.Fee) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["cancel"]++
	f.lastRes, f.lastFee = res, fee
	if f.replErr != nil {
		return "", f.replErr
	}
	return fmt.Sprintf("0xc%d", res.Value), nil
}

func (f *evmFake) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

// evmWorld 是 world 加上一個換得掉交易的 sender、一個發號器、一本廣播紀錄本。
type evmWorld struct {
	*world
	fake *evmFake
	seq  *txseq.Counter
	logs *MemoryBroadcasts
}

func newEVMWorld(t *testing.T, opts ...Option) *evmWorld {
	t.Helper()
	wd := newWorld(t)
	f, seq, logs := newEVMFake(), txseq.NewCounter(), NewMemoryBroadcasts()
	opts = append([]Option{WithClock(wd.now), WithSequencer(seq), WithBroadcasts(logs)}, opts...)
	wd.w = New(wd.q, wd.intents, wd.journal, f, opts...)
	return &evmWorld{world: wd, fake: f, seq: seq, logs: logs}
}

// stick 讓一筆 intent 卡在 settling：第一次廣播的結果是「不知道」，所以序列上留下一個洞。
func (ew *evmWorld) stick(t *testing.T, id string) *intent.Intent {
	t.Helper()
	it := ew.newIntent(t, id, intent.StateAuthorized)
	ew.enqueue(t, it)
	ew.fake.sendErr = errors.New("rpc: timeout") // 沒包 ErrNotSent，所以是「不知道」
	if rep := ew.runOnce(t); rep.Outcome != OutcomeRetry {
		t.Fatalf("expected the first broadcast to fail: %s", rep)
	}
	ew.fake.sendErr = nil
	if st := ew.seq.Status(relayerWallet); !st.HasGap {
		t.Fatalf("expected a gap: %s", st)
	}
	return it
}

// TestRescue_WaitsWhileYoung：卡得還不夠久就什麼都不做。這一條對三種發送結果一視同仁：
// lease 可能過期了而上一個 worker 還在送，這時候動手會跟還在飛的那一筆撞在一起。
func TestRescue_WaitsWhileYoung(t *testing.T) {
	ew := newEVMWorld(t)
	ew.stick(t, "pi_0001")
	ew.tick(DefaultConfig().RetryAfter)
	rep := ew.runOnce(t)
	if rep.Outcome != OutcomeRetry || rep.Detail != "settling for 5s without tx hash, waiting" {
		t.Fatalf("got %s", rep)
	}
	if ew.fake.count("replace") != 0 {
		t.Fatal("nothing should have been replaced yet")
	}
}

// TestRescue_SpeedUpTakesTheGapBack：卡夠久了就把那個號搶回來，用更高的出價再送一次同一筆付款。
// 洞補起來、帳戶恢復發號、intent 走到 confirming，而帳上還是只有一筆 hold：替換不是第二筆付款。
func TestRescue_SpeedUpTakesTheGapBack(t *testing.T) {
	ctx := context.Background()
	ew := newEVMWorld(t)
	it := ew.stick(t, "pi_0001")
	ew.tick(DefaultConfig().StuckAfter)

	rep := ew.runOnce(t)
	if rep.Outcome != OutcomeReplaced || rep.TxHash != "0x0001r" {
		t.Fatalf("got %s", rep)
	}
	if !ew.fake.lastRes.Fill || ew.fake.lastRes.Value != 0 {
		t.Fatalf("the replacement must reuse the gap: %+v", ew.fake.lastRes)
	}
	if got := ew.fake.lastFee.String(); got != "cap 33.000 gwei tip 2.200 gwei" {
		t.Fatalf("fee = %q, want one bump above the base", got)
	}
	if st := ew.seq.Status(relayerWallet); st.HasGap {
		t.Fatalf("the gap should be gone: %s", st)
	}
	got := ew.state(t, it.ID)
	if got.State != intent.StateConfirming || got.TxHash != "0x0001r" {
		t.Fatalf("intent: %s tx=%s", got.State, got.TxHash)
	}
	n := 0
	_ = ew.journal.Scan(ctx, func(ledger.Entry) error { n++; return nil })
	if n != 1 {
		t.Fatalf("a replacement is not a second payment: %d ledger entries", n)
	}
	if _, tries, _, _ := ew.logs.Last(ctx, it.ID); tries != 2 {
		t.Fatalf("both broadcasts should be on record, got %d", tries)
	}
}

// TestRescue_FailedReplaceKeepsTheGap：替換自己也可能送不出去。送不出去就什麼都不變：洞留著、intent 留在 settling，
// 下一次交付再來一輪。
func TestRescue_FailedReplaceKeepsTheGap(t *testing.T) {
	ew := newEVMWorld(t)
	it := ew.stick(t, "pi_0001")
	ew.fake.replErr = errors.New("rpc: timeout")
	ew.tick(DefaultConfig().StuckAfter)

	rep := ew.runOnce(t)
	if rep.Outcome != OutcomeRetry || rep.Detail != "speed-up: rpc: timeout" {
		t.Fatalf("got %s", rep)
	}
	if st := ew.seq.Status(relayerWallet); !st.HasGap || st.InFlight {
		t.Fatalf("the gap should still be there and nothing in flight: %s", st)
	}
	if got := ew.state(t, it.ID); got.State != intent.StateSettling {
		t.Fatalf("intent: %s", got.State)
	}
}

// TestRescue_ClearsTheSlotWhenGivingUp：廣播次數用完就不再救這筆付款，改成在同一個號上送一筆不動錢的交易。
// 救的是帳戶不是付款：號清出來，後面排隊的付款走得動；這筆 intent 交給人。
func TestRescue_ClearsTheSlotWhenGivingUp(t *testing.T) {
	p := txfee.DefaultPolicy()
	p.MaxTries = 1
	ew := newEVMWorld(t, WithFeePolicy(p))
	it := ew.stick(t, "pi_0001")
	ew.tick(DefaultConfig().StuckAfter)

	rep := ew.runOnce(t)
	if rep.Outcome != OutcomeCleared || rep.TxHash != "0xc0" {
		t.Fatalf("got %s", rep)
	}
	if ew.fake.count("replace") != 0 || ew.fake.count("cancel") != 1 {
		t.Fatal("it should have sent a no-op transaction, not the payment again")
	}
	if st := ew.seq.Status(relayerWallet); st.HasGap {
		t.Fatalf("the slot should be cleared: %s", st)
	}
	got := ew.state(t, it.ID)
	last := got.History[len(got.History)-1]
	if got.State != intent.StateNeedsReview || last.By != intent.ActorRelayer {
		t.Fatalf("intent: %s by=%s", got.State, last.By)
	}
	// 誰贏了那一格要對鏈才知道，所以理由裡要帶著取消交易的 hash。
	if last.Reason == "" || !strings.Contains(last.Reason, "0xc0") {
		t.Fatalf("reason = %q", last.Reason)
	}
}

// TestRescue_ReviewsAtTheFeeCeiling：出價到頂就不再送任何東西。加速與取消都要贏過舊交易，出不起就是出不起。
func TestRescue_ReviewsAtTheFeeCeiling(t *testing.T) {
	p := txfee.DefaultPolicy()
	p.Base = txfee.NewFee(44, 3)
	ew := newEVMWorld(t, WithFeePolicy(p))
	it := ew.stick(t, "pi_0001")
	ew.tick(DefaultConfig().StuckAfter)

	rep := ew.runOnce(t)
	if rep.Outcome != OutcomeReview {
		t.Fatalf("got %s", rep)
	}
	if ew.fake.count("replace")+ew.fake.count("cancel") != 0 {
		t.Fatal("nothing should have been broadcast")
	}
	if got := ew.state(t, it.ID); got.State != intent.StateNeedsReview {
		t.Fatalf("intent: %s", got.State)
	}
	// 號還在洞裡：出不起價就補不了洞，這個帳戶要等人處理。
	if st := ew.seq.Status(relayerWallet); !st.HasGap {
		t.Fatalf("the gap should still be there: %s", st)
	}
}

// TestRescue_ResendsWithAFreshNumber：上一次確定沒發送出去的話，號早就退回去了，鏈上也沒有東西要贏。
// 這時的「替換」其實是重來一次：拿一個新的號、照基準價送。
func TestRescue_ResendsWithAFreshNumber(t *testing.T) {
	ew := newEVMWorld(t)
	it := ew.newIntent(t, "pi_0001", intent.StateAuthorized)
	ew.enqueue(t, it)
	ew.fake.sendErr = fmt.Errorf("%w: signing failed", ErrNotSent)
	if rep := ew.runOnce(t); rep.Outcome != OutcomeRetry {
		t.Fatalf("first: %s", rep)
	}
	if st := ew.seq.Status(relayerWallet); st.HasGap || st.Next != 0 {
		t.Fatalf("a not-sent reservation goes back: %s", st)
	}
	ew.fake.sendErr = nil
	ew.tick(DefaultConfig().StuckAfter)

	rep := ew.runOnce(t)
	if rep.Outcome != OutcomeReplaced || rep.TxHash != "0x0001r" {
		t.Fatalf("second: %s", rep)
	}
	if ew.fake.lastRes.Fill || ew.fake.lastRes.Value != 0 {
		t.Fatalf("expected a fresh number, got %+v", ew.fake.lastRes)
	}
	if got := ew.fake.lastFee.String(); got != "cap 30.000 gwei tip 2.000 gwei" {
		t.Fatalf("fee = %q, want the base fee", got)
	}
}

// TestRescue_NothingToClear：要送取消交易、卻沒有洞可以補，代表那個號早就好好地用掉了，沒有東西可以搶回來。
// 這時再送一筆不動錢的交易只是白燒 gas，所以直接送審。
func TestRescue_NothingToClear(t *testing.T) {
	p := txfee.DefaultPolicy()
	p.MaxTries = 1
	ew := newEVMWorld(t, WithFeePolicy(p))
	it := ew.newIntent(t, "pi_0001", intent.StateAuthorized)
	ew.enqueue(t, it)
	ew.fake.sendErr = fmt.Errorf("%w: signing failed", ErrNotSent)
	ew.runOnce(t)
	ew.fake.sendErr = nil
	ew.tick(DefaultConfig().StuckAfter)

	rep := ew.runOnce(t)
	if rep.Outcome != OutcomeReview || ew.fake.count("cancel") != 0 {
		t.Fatalf("got %s, cancels=%d", rep, ew.fake.count("cancel"))
	}
	if got := ew.state(t, it.ID); got.State != intent.StateNeedsReview {
		t.Fatalf("intent: %s", got.State)
	}
}

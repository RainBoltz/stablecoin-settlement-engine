package relayer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

const relayerWallet = "0x90F79bf6EB2c4f870365E785982E1f101E93b906"

// orderedFake 是一個需要序號的 fake sender（EVM 那一類）：記下每次拿到的號，可以被指定要回什麼錯誤。
type orderedFake struct {
	mu   sync.Mutex
	seen []uint64
	err  error
}

func (f *orderedFake) Account(*intent.Intent) string { return relayerWallet }

func (f *orderedFake) Send(context.Context, *intent.Intent) (string, error) {
	return "", fmt.Errorf("%w: evm transactions need a nonce", ErrNotSent)
}

func (f *orderedFake) SendAt(_ context.Context, it *intent.Intent, res txseq.Reservation) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, res.Value)
	if f.err != nil {
		return "", f.err
	}
	return "0x" + it.ID[3:], nil
}

func (f *orderedFake) slots() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.seen...)
}

// ordered 把 world 換成一個 OrderedSender 加一個 Counter，回傳兩者。
func ordered(t *testing.T, wd *world, start uint64) (*orderedFake, *txseq.Counter) {
	t.Helper()
	f := &orderedFake{}
	seq := txseq.NewCounter()
	if err := seq.Sync(context.Background(), relayerWallet, start); err != nil {
		t.Fatal(err)
	}
	wd.w.sender, wd.w.seq = f, seq
	return f, seq
}

// TestWorker_OrderedSenderGetsConsecutiveSlots：需要序號的 sender 拿到的是連號，而且序號進得了交易。
func TestWorker_OrderedSenderGetsConsecutiveSlots(t *testing.T) {
	wd := newWorld(t)
	f, seq := ordered(t, wd, 7)
	for _, id := range []string{"pi_0001", "pi_0002"} {
		wd.enqueue(t, wd.newIntent(t, id, intent.StateAuthorized))
	}

	if rep := wd.runOnce(t); rep.Outcome != OutcomeSent {
		t.Fatalf("report: %s", rep)
	}
	if rep := wd.runOnce(t); rep.Outcome != OutcomeSent {
		t.Fatalf("report: %s", rep)
	}
	if got := f.slots(); len(got) != 2 || got[0] != 7 || got[1] != 8 {
		t.Fatalf("slots = %v, want [7 8]", got)
	}
	if st := seq.Status(relayerWallet); st.Next != 9 || st.InFlight || st.HasGap {
		t.Fatalf("status = %+v, want next 9, nothing in flight, no gap", st)
	}
}

// TestWorker_NotSentReturnsTheSlot：sender 宣告確定沒出門，序號要退回去給下一筆用，序列上不能留洞。
func TestWorker_NotSentReturnsTheSlot(t *testing.T) {
	wd := newWorld(t)
	f, seq := ordered(t, wd, 7)
	wd.enqueue(t, wd.newIntent(t, "pi_0001", intent.StateAuthorized))
	f.err = fmt.Errorf("%w: signing failed", ErrNotSent)

	if rep := wd.runOnce(t); rep.Outcome != OutcomeRetry {
		t.Fatalf("report: %s", rep)
	}
	if st := seq.Status(relayerWallet); st.Next != 7 || st.HasGap {
		t.Fatalf("status = %+v, want next 7 and no gap", st)
	}
}

// TestWorker_UnknownSendLeavesAGap：沒宣告的失敗一律當成不知道，序號當成用掉、留一個洞。
// 退回去重用會撞到那筆可能已經在 mempool 裡的交易。
func TestWorker_UnknownSendLeavesAGap(t *testing.T) {
	wd := newWorld(t)
	f, seq := ordered(t, wd, 7)
	wd.enqueue(t, wd.newIntent(t, "pi_0001", intent.StateAuthorized))
	f.err = errors.New("rpc: timeout")

	if rep := wd.runOnce(t); rep.Outcome != OutcomeRetry {
		t.Fatalf("report: %s", rep)
	}
	if st := seq.Status(relayerWallet); st.Next != 8 || !st.HasGap || st.Gap != 7 {
		t.Fatalf("status = %+v, want next 8 and a gap at 7", st)
	}
}

// TestWorker_GapKeepsTheNextIntentAuthorized：帳戶有洞的時候取號會失敗，而取號擋在副作用之前，
// 所以那筆 intent 原封不動：還在 authorized、帳上沒有 hold、job 回到 queue。
func TestWorker_GapKeepsTheNextIntentAuthorized(t *testing.T) {
	wd := newWorld(t)
	f, _ := ordered(t, wd, 7)
	for _, id := range []string{"pi_0001", "pi_0002"} {
		wd.enqueue(t, wd.newIntent(t, id, intent.StateAuthorized))
	}
	f.err = errors.New("rpc: timeout")
	wd.runOnce(t) // pi_0001 製造一個洞
	f.err = nil

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeRetry || !strings.Contains(rep.Detail, "no slot") {
		t.Fatalf("report: %s", rep)
	}
	if got := wd.state(t, "pi_0002"); got.State != intent.StateAuthorized || got.Version != 2 {
		t.Fatalf("intent: state=%s v%d, want authorized v2", got.State, got.Version)
	}
	if _, err := wd.journal.Get(context.Background(), "pi_0002/hold"); err == nil {
		t.Fatal("journal has a hold for pi_0002; the slot should have been taken before any side effect")
	}
	if wd.queueLen(t) != 2 {
		t.Fatalf("queue has %d job(s), want 2 (both went back untouched)", wd.queueLen(t))
	}
}

// TestWorker_SyncClearsAGapAndWorkResumes：洞被鏈上走過去之後，同一份 job 再來就拿得到號了。
func TestWorker_SyncClearsAGapAndWorkResumes(t *testing.T) {
	wd := newWorld(t)
	f, seq := ordered(t, wd, 7)
	wd.enqueue(t, wd.newIntent(t, "pi_0001", intent.StateAuthorized))
	wd.enqueue(t, wd.newIntent(t, "pi_0002", intent.StateAuthorized))
	f.err = errors.New("rpc: timeout")
	wd.runOnce(t) // pi_0001：洞在 7
	f.err = nil
	wd.runOnce(t) // pi_0002：拿不到號

	if err := seq.Sync(context.Background(), relayerWallet, 8); err != nil {
		t.Fatal(err)
	}
	wd.tick(wd.w.cfg.RetryAfter)
	wd.runOnce(t) // pi_0001 先回來：它自己還卡在 settling，等
	if rep := wd.runOnce(t); rep.Outcome != OutcomeSent {
		t.Fatalf("report: %s", rep)
	}
}

// TestWorker_LostRaceToSettlingReturnsTheSlot：號拿到了但 CAS 輸了，什麼都沒送出去，號要退回去。
// 用一個會在拿名額時把 intent 取消掉的 Limiter 製造這個競爭。
func TestWorker_LostRaceToSettlingReturnsTheSlot(t *testing.T) {
	wd := newWorld(t)
	_, seq := ordered(t, wd, 7)
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.w.limiter = raceLimiter{func() {
		_, _, _ = intent.Advance(context.Background(), wd.intents, "pi_0001",
			intent.Request{To: intent.StateCanceled, By: intent.ActorAPI, Reason: "merchant canceled", At: wd.now()})
	}}

	if rep := wd.runOnce(t); rep.Outcome != OutcomeRetry {
		t.Fatalf("report: %s", rep)
	}
	if st := seq.Status(relayerWallet); st.Next != 7 || st.HasGap {
		t.Fatalf("status = %+v, want next 7 and no gap", st)
	}
}

// TestWorker_PlainSenderNeverTakesASlot：不需要序號的鏈（Solana、SUI）走原本那條路，發號器完全沒被碰過。
func TestWorker_PlainSenderNeverTakesASlot(t *testing.T) {
	wd := newWorld(t)
	seq := txseq.NewCounter()
	wd.w.seq = seq
	wd.enqueue(t, wd.newIntent(t, "pi_0001", intent.StateAuthorized))

	if rep := wd.runOnce(t); rep.Outcome != OutcomeSent {
		t.Fatalf("report: %s", rep)
	}
	if got := seq.Accounts(); len(got) != 0 {
		t.Fatalf("sequencer saw %v, want nothing", got)
	}
}

// raceLimiter 在 worker 拿名額的時候插一手，用來製造 CAS 競爭。
type raceLimiter struct{ before func() }

func (l raceLimiter) Acquire(context.Context) error { l.before(); return nil }
func (l raceLimiter) Release()                      {}

package relayer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/dlq"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfail"
)

// withDeadLetters 換掉 world 裡那個 worker 的收容所，測試才看得到停進去的東西。
func (wd *world) withDeadLetters(t *testing.T) *dlq.MemoryStore {
	t.Helper()
	s := dlq.NewMemoryStore()
	wd.w.dead = s
	return s
}

// countEntries 數 journal 現在有幾列，用來釘「redrive 之後帳本一列都沒有多」。
func (wd *world) countEntries(t *testing.T) int {
	t.Helper()
	n := 0
	if err := wd.journal.Scan(context.Background(), func(ledger.Entry) error { n++; return nil }); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return n
}

func onlyParked(t *testing.T, s dlq.Store) dlq.Record {
	t.Helper()
	all, err := s.List(context.Background(), dlq.StatusParked)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want exactly one parked record, got %d", len(all))
	}
	return all[0]
}

// TestPoison_ParksTheJobWithTheStateAfterTheDisposal：便條上那一格要寫收尾之後的樣子。
// 這一筆走的是 settling -> failed，人打開 DLQ 看到的就該是 failed，不是它進來時的 settling。
func TestPoison_ParksTheJobWithTheStateAfterTheDisposal(t *testing.T) {
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	dead := wd.withDeadLetters(t)
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.sendErr = fmt.Errorf("%w: %w: evm:31337 has no signer configured", ErrNotSent, txfail.ErrPoison)

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomePoison {
		t.Fatalf("report: %s", rep)
	}
	rec := onlyParked(t, dead)
	if rec.Job.ID != "pi_0001/settle" || rec.Attempts != 1 || rec.Cycles != 1 {
		t.Fatalf("record: %+v", rec)
	}
	if rec.IntentState != string(intent.StateFailed) {
		t.Fatalf("want the state after the disposal, got %q", rec.IntentState)
	}
	if !strings.Contains(rec.Reason, "retrying will not help") || !rec.ParkedAt.Equal(wd.now()) {
		t.Fatalf("record should carry the verdict and the time: %+v", rec)
	}
}

// TestPoison_ParksAJobItNeverTouched：intent 還在 created，relayer 一個 byte 都沒寫過。
// 那筆付款原封不動，被收起來的只有便條。
func TestPoison_ParksAJobItNeverTouched(t *testing.T) {
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	dead := wd.withDeadLetters(t)
	it := wd.newIntent(t, "pi_0001", intent.StateCreated)
	wd.enqueue(t, it)

	reps := wd.drain(t, 12)
	if last := reps[len(reps)-1]; last.Outcome != OutcomePoison {
		t.Fatalf("last report: %s", last)
	}
	rec := onlyParked(t, dead)
	if rec.IntentState != string(intent.StateCreated) || rec.Attempts != 3 {
		t.Fatalf("record: %+v", rec)
	}
	if wd.state(t, "pi_0001").State != intent.StateCreated {
		t.Fatal("the intent should not have moved")
	}
}

// TestRedrive_ReachesTheWorkerAgainWhenTheIntentIsReady：redrive 真正救得回來的那一種。
// 付款人終於簽了名，同一份便條放回去就走完了，沒有人需要動那筆 intent 的狀態。
func TestRedrive_ReachesTheWorkerAgainWhenTheIntentIsReady(t *testing.T) {
	ctx := context.Background()
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	dead := wd.withDeadLetters(t)
	it := wd.newIntent(t, "pi_0001", intent.StateCreated)
	wd.enqueue(t, it)
	wd.drain(t, 12)

	if _, _, err := intent.Advance(ctx, wd.intents, "pi_0001",
		intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: wd.now()}); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if _, err := dlq.Redrive(ctx, dead, wd.q, "pi_0001/settle", "ops", wd.now()); err != nil {
		t.Fatalf("redrive: %v", err)
	}

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeSent || rep.Attempt != 1 {
		t.Fatalf("report: %s", rep)
	}
	if wd.state(t, "pi_0001").State != intent.StateConfirming {
		t.Fatalf("intent: %s", wd.state(t, "pi_0001").State)
	}
}

// TestRedrive_OnAFinishedIntentIsANoop：這條就是「放回去不會讓錢多動一次」。那筆 intent 已經 failed、
// hold 已經 void 掉，同一份便條再走一次只換到一次 no-op，帳本一列都沒有多。
func TestRedrive_OnAFinishedIntentIsANoop(t *testing.T) {
	ctx := context.Background()
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	dead := wd.withDeadLetters(t)
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.sendErr = fmt.Errorf("%w: %w: evm:31337 has no signer configured", ErrNotSent, txfail.ErrPoison)
	wd.runOnce(t)

	before := wd.countEntries(t)
	wd.sendErr = nil // 節點修好了也一樣不該再送一次
	if _, err := dlq.Redrive(ctx, dead, wd.q, "pi_0001/settle", "ops", wd.now()); err != nil {
		t.Fatalf("redrive: %v", err)
	}

	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeNoop || !strings.Contains(rep.Detail, string(intent.StateFailed)) {
		t.Fatalf("report: %s", rep)
	}
	if after := wd.countEntries(t); after != before {
		t.Fatalf("journal grew from %d to %d entries", before, after)
	}
	if wd.sends["pi_0001"] != 0 {
		t.Fatalf("Send was called %d time(s) after the redrive", wd.sends["pi_0001"])
	}
	if wd.queueLen(t) != 0 {
		t.Fatalf("the redriven job should be acked, %d left", wd.queueLen(t))
	}
}

// TestRedrive_OnAReviewedIntentIsANoop：停在 needs_review 的付款不是 redrive 修得好的。
// 便條放回去只是被 Ack 一次，那一格的出口只有 operator 走得動。
func TestRedrive_OnAReviewedIntentIsANoop(t *testing.T) {
	ctx := context.Background()
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	dead := wd.withDeadLetters(t)
	it := wd.newIntent(t, "pi_0001", intent.StateAuthorized)
	wd.enqueue(t, it)
	wd.sendErr = errors.New("rpc: connection refused")
	wd.drain(t, 12)
	if wd.state(t, "pi_0001").State != intent.StateNeedsReview {
		t.Fatalf("setup: %s", wd.state(t, "pi_0001").State)
	}

	if _, err := dlq.Redrive(ctx, dead, wd.q, "pi_0001/settle", "ops", wd.now()); err != nil {
		t.Fatalf("redrive: %v", err)
	}
	wd.sendErr = nil
	rep := wd.runOnce(t)
	if rep.Outcome != OutcomeNoop {
		t.Fatalf("report: %s", rep)
	}
	got := wd.state(t, "pi_0001")
	if got.State != intent.StateNeedsReview {
		t.Fatalf("intent: %s", got.State)
	}
	if _, err := wd.journal.Get(ctx, "pi_0001/void"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("the hold should still be pending, got %v", err)
	}
}

// TestPoison_ParkingAgainAfterARedriveIsANewCycle：放回去又回來的那一種。同一份 job 在收容所裡還是一列，
// 但 Cycles 變成 2，人看得出來自己已經按過一次了。
func TestPoison_ParkingAgainAfterARedriveIsANewCycle(t *testing.T) {
	ctx := context.Background()
	wd := newWorld(t)
	wd.w.faults = shortBudget()
	dead := wd.withDeadLetters(t)
	it := wd.newIntent(t, "pi_0001", intent.StateCreated)
	wd.enqueue(t, it)
	wd.drain(t, 12)
	if _, err := dlq.Redrive(ctx, dead, wd.q, "pi_0001/settle", "ops", wd.now()); err != nil {
		t.Fatalf("redrive: %v", err)
	}
	wd.drain(t, 12)

	rec := onlyParked(t, dead)
	if rec.Cycles != 2 || rec.Status != dlq.StatusParked || rec.ResolvedBy != "" {
		t.Fatalf("record: %+v", rec)
	}
	all, _ := dead.List(ctx, "")
	if len(all) != 1 {
		t.Fatalf("want one record, got %d", len(all))
	}
}

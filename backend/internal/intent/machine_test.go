package intent

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	txA = "0xaaaa000000000000000000000000000000000000000000000000000000000001"
	txB = "0xbbbb000000000000000000000000000000000000000000000000000000000002"
)

// newTestIntent 建一筆 100 USDC、一小時內要簽名的 intent。
func newTestIntent(t *testing.T) *Intent {
	t.Helper()
	it, err := New(Spec{
		ID:        "pi_test",
		Chain:     "evm:31337",
		Token:     "0x5FbDB2315678afecb367f032d93F642f64180aa3",
		Payer:     "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
		Merchant:  "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
		Amount:    big.NewInt(100_000_000),
		ExpiresAt: t0.Add(time.Hour),
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func mustApply(t *testing.T, it *Intent, req Request) {
	t.Helper()
	applied, err := Apply(it, req)
	if err != nil {
		t.Fatalf("apply %s -> %s by %s: %v", it.State, req.To, req.By, err)
	}
	if !applied {
		t.Fatalf("apply %s -> %s by %s: not applied", it.State, req.To, req.By)
	}
}

// drive 把 intent 沿著正常路從目前所在的狀態走到 target（最遠到 settled）。
func drive(t *testing.T, it *Intent, target State) {
	t.Helper()
	steps := map[State]Request{
		StateCreated:    {To: StateAuthorized, By: ActorAPI, At: t0.Add(time.Minute)},
		StateAuthorized: {To: StateSettling, By: ActorRelayer, At: t0.Add(2 * time.Minute)},
		StateSettling:   {To: StateConfirming, By: ActorRelayer, TxHash: txA, At: t0.Add(3 * time.Minute)},
		StateConfirming: {To: StateSettled, By: ActorListener, TxHash: txA, At: t0.Add(4 * time.Minute)},
	}
	for it.State != target {
		next, ok := steps[it.State]
		if !ok {
			t.Fatalf("drive: no happy-path step out of %s", it.State)
		}
		mustApply(t, it, next)
	}
}

// TestNew_StartsInCreated：唯一的入口，而且一定是 created、版本 1、歷程為空。
func TestNew_StartsInCreated(t *testing.T) {
	it := newTestIntent(t)
	if it.State != StateCreated || it.Version != 1 || len(it.History) != 0 {
		t.Fatalf("unexpected fresh intent: %+v", it)
	}
}

// TestNew_RejectsBadSpec：只擋「資料自己看得出來」的錯，不去猜鏈上的事。
func TestNew_RejectsBadSpec(t *testing.T) {
	good := Spec{ID: "x", Chain: "evm:31337", Token: "0xt", Payer: "0xp", Merchant: "0xm", Amount: big.NewInt(1)}
	cases := map[string]func(*Spec){
		"missing id":     func(s *Spec) { s.ID = "" },
		"missing chain":  func(s *Spec) { s.Chain = "" },
		"nil amount":     func(s *Spec) { s.Amount = nil },
		"zero amount":    func(s *Spec) { s.Amount = big.NewInt(0) },
		"past expiresAt": func(s *Spec) { s.ExpiresAt = t0.Add(-time.Second) },
	}
	for name, mutate := range cases {
		s := good
		mutate(&s)
		if _, err := New(s, t0); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: want ErrInvalidSpec, got %v", name, err)
		}
	}
	if _, err := New(good, t0); err != nil {
		t.Fatalf("good spec rejected: %v", err)
	}
}

// TestApply_HappyPath：created → authorized → settling → confirming → settled，
// 每一步版本加一、歷程加一、雜湊記在 intent 上。
func TestApply_HappyPath(t *testing.T) {
	it := newTestIntent(t)
	drive(t, it, StateSettled)
	if it.State != StateSettled {
		t.Fatalf("state = %s", it.State)
	}
	if it.Version != 5 {
		t.Fatalf("version = %d, want 5 (1 + 4 transitions)", it.Version)
	}
	if len(it.History) != 4 {
		t.Fatalf("history len = %d", len(it.History))
	}
	if it.TxHash != txA {
		t.Fatalf("tx hash = %q", it.TxHash)
	}
	if it.UpdatedAt != t0.Add(4*time.Minute) {
		t.Fatalf("updatedAt = %s", it.UpdatedAt)
	}
}

// TestApply_ReplayIsNoop：同一個事件送到兩次，第二次要安靜地過：不報錯、不動版本、不加歷程。
// queue 重送、listener 重掃區塊都會造成這種事，它是常態。
func TestApply_ReplayIsNoop(t *testing.T) {
	it := newTestIntent(t)
	drive(t, it, StateSettling)
	before := it.Clone()

	applied, err := Apply(it, Request{To: StateSettling, By: ActorRelayer, At: t0.Add(time.Hour)})
	if err != nil || applied {
		t.Fatalf("replay: applied=%v err=%v", applied, err)
	}
	if it.Version != before.Version || len(it.History) != len(before.History) || it.UpdatedAt != before.UpdatedAt {
		t.Fatalf("replay mutated the intent: %+v", it)
	}

	// 終態的重放也一樣：settled 再收到一次 settled 不是錯。
	drive(t, it, StateSettled)
	applied, err = Apply(it, Request{To: StateSettled, By: ActorListener, TxHash: txA, At: t0.Add(time.Hour)})
	if err != nil || applied {
		t.Fatalf("terminal replay: applied=%v err=%v", applied, err)
	}
}

// TestApply_TerminalIsAbsorbing：三個終態 × 所有目標 × 所有角色，全部拒絕，而且一個欄位都不動。
func TestApply_TerminalIsAbsorbing(t *testing.T) {
	terminals := map[State]func(*Intent){
		StateSettled: func(it *Intent) { drive(t, it, StateSettled) },
		StateFailed: func(it *Intent) {
			drive(t, it, StateSettling)
			mustApply(t, it, Request{To: StateFailed, By: ActorRelayer, Reason: "blacklisted", At: t0})
		},
		StateCanceled: func(it *Intent) {
			mustApply(t, it, Request{To: StateCanceled, By: ActorAPI, Reason: "merchant canceled", At: t0})
		},
	}
	for term, reach := range terminals {
		it := newTestIntent(t)
		reach(it)
		snapshot := it.Clone()
		for _, to := range States() {
			if to == term {
				continue // 重放另有測試
			}
			for _, by := range Actors() {
				_, err := Apply(it, Request{To: to, By: by, TxHash: txA, Reason: "r", At: t0})
				if !errors.Is(err, ErrTerminal) {
					t.Errorf("%s -> %s by %s: want ErrTerminal, got %v", term, to, by, err)
				}
			}
		}
		if it.State != snapshot.State || it.Version != snapshot.Version || len(it.History) != len(snapshot.History) {
			t.Errorf("%s: intent mutated by rejected transitions", term)
		}
	}
}

// TestApply_EveryEdgeNotInTableIsRejected：窮舉所有 (from, to, actor)。
// 不在表上的 (from, to) 拿到 ErrIllegalTransition；在表上但角色不對拿到 ErrForbiddenActor。
// 這條測試的用意是「沒有預設放行」：表沒寫的就是不行。
func TestApply_EveryEdgeNotInTableIsRejected(t *testing.T) {
	// 每個非終態各造一筆走到那裡的 intent
	reach := map[State]func() *Intent{
		StateCreated:    func() *Intent { return newTestIntent(t) },
		StateAuthorized: func() *Intent { it := newTestIntent(t); drive(t, it, StateAuthorized); return it },
		StateSettling:   func() *Intent { it := newTestIntent(t); drive(t, it, StateSettling); return it },
		StateConfirming: func() *Intent { it := newTestIntent(t); drive(t, it, StateConfirming); return it },
		StateNeedsReview: func() *Intent {
			it := newTestIntent(t)
			drive(t, it, StateSettling)
			mustApply(t, it, Request{To: StateNeedsReview, By: ActorRelayer, Reason: "tx status unknown", At: t0})
			return it
		},
	}
	for from, mk := range reach {
		for _, to := range States() {
			if to == from {
				continue
			}
			rule, inTable := Lookup(from, to)
			for _, by := range Actors() {
				it := mk()
				req := Request{To: to, By: by, TxHash: txA, Reason: "r", At: t0}
				if from == StateConfirming && to == StateSettled {
					req.TxHash = it.TxHash // 讓「合法」的那條真的合法
				}
				applied, err := Apply(it, req)
				switch {
				case !inTable:
					if !errors.Is(err, ErrIllegalTransition) {
						t.Errorf("%s -> %s by %s: want ErrIllegalTransition, got %v", from, to, by, err)
					}
				case !rule.Allows(by):
					if !errors.Is(err, ErrForbiddenActor) {
						t.Errorf("%s -> %s by %s: want ErrForbiddenActor, got %v", from, to, by, err)
					}
				default:
					if err != nil || !applied {
						t.Errorf("%s -> %s by %s: legal edge rejected: applied=%v err=%v", from, to, by, applied, err)
					}
				}
				if err != nil && it.State != from {
					t.Errorf("%s -> %s by %s: rejected transition mutated state to %s", from, to, by, it.State)
				}
			}
		}
	}
}

// TestApply_EvidenceIsRequired：進鏈上狀態沒帶雜湊、走非正常路沒留理由，都拒絕。
func TestApply_EvidenceIsRequired(t *testing.T) {
	it := newTestIntent(t)
	drive(t, it, StateSettling)

	if _, err := Apply(it, Request{To: StateConfirming, By: ActorRelayer, At: t0}); !errors.Is(err, ErrMissingEvidence) {
		t.Errorf("confirming without tx hash: got %v", err)
	}
	if _, err := Apply(it, Request{To: StateFailed, By: ActorRelayer, At: t0}); !errors.Is(err, ErrMissingEvidence) {
		t.Errorf("failed without reason: got %v", err)
	}
	if _, err := Apply(it, Request{To: StateNeedsReview, By: ActorRelayer, At: t0}); !errors.Is(err, ErrMissingEvidence) {
		t.Errorf("needs_review without reason: got %v", err)
	}
	if it.State != StateSettling {
		t.Fatalf("state changed to %s", it.State)
	}
}

// TestApply_SettledHashMustMatchConfirming：listener 宣告 settled 的雜湊跟 relayer 記下的不同，
// 代表兩邊看的不是同一筆交易，要停下來，不能靜靜地過。
func TestApply_SettledHashMustMatchConfirming(t *testing.T) {
	it := newTestIntent(t)
	drive(t, it, StateConfirming)
	_, err := Apply(it, Request{To: StateSettled, By: ActorListener, TxHash: txB, At: t0})
	if !errors.Is(err, ErrEvidenceMismatch) {
		t.Fatalf("want ErrEvidenceMismatch, got %v", err)
	}
	if it.State != StateConfirming || it.TxHash != txA {
		t.Fatalf("mismatch mutated intent: state=%s tx=%s", it.State, it.TxHash)
	}
}

// TestApply_ExpiredCannotBeAuthorized：簽名迴圈逾時，簽名再正確也不收；時鐘可以把它取消。
func TestApply_ExpiredCannotBeAuthorized(t *testing.T) {
	it := newTestIntent(t) // expires at t0+1h
	late := t0.Add(time.Hour + time.Second)
	if _, err := Apply(it, Request{To: StateAuthorized, By: ActorAPI, At: late}); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	// 剛好在期限上還可以
	onTime := newTestIntent(t)
	mustApply(t, onTime, Request{To: StateAuthorized, By: ActorAPI, At: t0.Add(time.Hour)})

	mustApply(t, it, Request{To: StateCanceled, By: ActorSystem, Reason: "expired", At: late})
	if !it.State.Terminal() {
		t.Fatal("expired intent should be terminal")
	}
}

// TestApply_ZeroExpiryMeansNoDeadline：ExpiresAt 零值代表不設限。
func TestApply_ZeroExpiryMeansNoDeadline(t *testing.T) {
	it, err := New(Spec{ID: "x", Chain: "evm:31337", Token: "0xt", Payer: "0xp", Merchant: "0xm", Amount: big.NewInt(1)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, it, Request{To: StateAuthorized, By: ActorAPI, At: t0.Add(1000 * time.Hour)})
}

// TestApply_ReorgGoesBackToSettlingAndClearsHash：reorg 把交易吐回來，listener 把它退回 settling，
// 雜湊清空；relayer 重新送出後可以用新雜湊再進 confirming。歷程會留下完整的來回。
func TestApply_ReorgGoesBackToSettlingAndClearsHash(t *testing.T) {
	it := newTestIntent(t)
	drive(t, it, StateConfirming)

	// 只有 listener 能走這條路
	if _, err := Apply(it, Request{To: StateSettling, By: ActorRelayer, Reason: "reorg", At: t0}); !errors.Is(err, ErrForbiddenActor) {
		t.Fatalf("relayer must not walk the reorg edge: %v", err)
	}
	mustApply(t, it, Request{To: StateSettling, By: ActorListener, Reason: "reorg at block 12", At: t0.Add(5 * time.Minute)})
	if it.TxHash != "" {
		t.Fatalf("tx hash should be cleared after reorg, got %s", it.TxHash)
	}
	mustApply(t, it, Request{To: StateConfirming, By: ActorRelayer, TxHash: txB, At: t0.Add(6 * time.Minute)})
	mustApply(t, it, Request{To: StateSettled, By: ActorListener, TxHash: txB, At: t0.Add(7 * time.Minute)})
	if it.TxHash != txB {
		t.Fatalf("tx hash = %s, want %s", it.TxHash, txB)
	}
	// 歷程：authorized, settling, confirming(txA), settling(reorg), confirming(txB), settled(txB)
	if len(it.History) != 6 || it.History[2].TxHash != txA || it.History[5].TxHash != txB {
		t.Fatalf("history does not record the reorg round trip: %+v", it.History)
	}
}

// TestApply_NeedsReviewIsResolvedByOperatorOnly：停在 needs_review 的 intent 只有人能收尾，
// 而且收成 settled 要拿出雜湊。
func TestApply_NeedsReviewIsResolvedByOperatorOnly(t *testing.T) {
	it := newTestIntent(t)
	drive(t, it, StateSettling)
	mustApply(t, it, Request{To: StateNeedsReview, By: ActorRelayer, Reason: "tx status unknown after 10 blocks", At: t0})

	if _, err := Apply(it, Request{To: StateSettling, By: ActorOperator, Reason: "retry", At: t0}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("needs_review -> settling must not exist: %v", err)
	}
	if _, err := Apply(it, Request{To: StateSettled, By: ActorListener, TxHash: txA, At: t0}); !errors.Is(err, ErrForbiddenActor) {
		t.Fatalf("listener must not resolve needs_review: %v", err)
	}
	if _, err := Apply(it, Request{To: StateSettled, By: ActorOperator, Reason: "found on chain", At: t0}); !errors.Is(err, ErrMissingEvidence) {
		t.Fatalf("operator settle without tx hash: %v", err)
	}
	mustApply(t, it, Request{To: StateSettled, By: ActorOperator, TxHash: txA, Reason: "found on chain, amount matches", At: t0})
	if it.TxHash != txA {
		t.Fatalf("tx hash = %s", it.TxHash)
	}
}

// TestApply_UnknownStateIsRejected：To 不是表上的狀態，直接拒絕，不會落到「不在表上」那條錯誤裡。
func TestApply_UnknownStateIsRejected(t *testing.T) {
	it := newTestIntent(t)
	if _, err := Apply(it, Request{To: State("paid"), By: ActorAPI, At: t0}); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("want ErrUnknownState, got %v", err)
	}
}

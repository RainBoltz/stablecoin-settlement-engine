package intent

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// TestNew_DerivesRefFromTerms：intent 一落地就有 ref，而且就是它自己的條件算出來的；只差 id 的兩筆 ref 不同。
func TestNew_DerivesRefFromTerms(t *testing.T) {
	it := newTestIntent(t)
	if it.Ref.IsZero() || it.Ref != paymentref.Derive(it.Terms()) {
		t.Fatalf("ref = %s, terms derive %s", it.Ref, paymentref.Derive(it.Terms()))
	}
	other := newTestIntent(t)
	other.ID = "pi_other"
	other.Ref = paymentref.Derive(other.Terms())
	if other.Ref == it.Ref {
		t.Fatal("two intents with different ids must not share a ref")
	}
	if c := it.Clone(); c.Ref != it.Ref {
		t.Fatal("clone must keep the ref")
	}
}

// TestApply_RefSurvivesTheWholeLifecycle：ref 從 created 走到 settled 一路不變。
// 它 commit 的是「這筆付款是什麼」，不是「走到哪了」；狀態怎麼變、tx hash 換幾次，ref 都是同一個。
func TestApply_RefSurvivesTheWholeLifecycle(t *testing.T) {
	it := newTestIntent(t)
	ref := it.Ref
	drive(t, it, StateSettled)
	if it.Ref != ref || it.State != StateSettled || it.TxHash != txA {
		t.Fatalf("ref changed across lifecycle: %s -> %s (state %s)", ref, it.Ref, it.State)
	}
}

// TestMemoryStore_GetByRef：拿 ref 找得回同一筆、拿到的是拷貝、找不到是 ErrNotFound；
// intent 推進幾步之後用同一個 ref 還是找得到最新版本。這就是 listener 從鏈上回來要走的路。
func TestMemoryStore_GetByRef(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	it := newTestIntent(t)
	if err := s.Save(ctx, it, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetByRef(ctx, it.Ref)
	if err != nil || got.ID != it.ID || got.Version != 1 {
		t.Fatalf("get by ref: %v %+v", err, got)
	}
	got.State = StateSettled
	if again, _ := s.GetByRef(ctx, it.Ref); again.State != StateCreated {
		t.Fatal("GetByRef leaked its internal pointer")
	}
	if _, err := s.GetByRef(ctx, paymentref.Ref{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for the zero ref, got %v", err)
	}

	drive(t, it, StateConfirming)
	if err := s.Save(ctx, it, 1); err != nil {
		t.Fatal(err)
	}
	latest, err := s.GetByRef(ctx, it.Ref)
	if err != nil || latest.State != StateConfirming || latest.TxHash != txA || latest.Version != 4 {
		t.Fatalf("after transitions: %v %+v", err, latest)
	}
}

// TestMemoryStore_SaveRejectsTamperedTerms：金額被動過、ref 沒跟著換，Save 拒收。
// 這條測試防的是「有人改資料庫那一列的金額」：ref 已經上鏈了，鏈下這一列就不能再變。
func TestMemoryStore_SaveRejectsTamperedTerms(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	it := newTestIntent(t)
	if err := s.Save(ctx, it, 0); err != nil {
		t.Fatal(err)
	}
	tampered, _ := s.Get(ctx, it.ID)
	tampered.Amount = big.NewInt(999_000_000)
	if err := s.Save(ctx, tampered, 1); !errors.Is(err, ErrRefMismatch) {
		t.Fatalf("want ErrRefMismatch, got %v", err)
	}
	stored, _ := s.Get(ctx, it.ID)
	if stored.Amount.Cmp(big.NewInt(100_000_000)) != 0 || stored.Version != 1 {
		t.Fatalf("store must be untouched after a rejected save: %+v", stored)
	}
	// 零值的 ref 也是不對的 ref：沒算過就想存，一樣擋。
	blank := newTestIntent(t)
	blank.ID, blank.Ref = "pi_blank", paymentref.Ref{}
	if err := s.Save(ctx, blank, 0); !errors.Is(err, ErrRefMismatch) {
		t.Fatalf("zero ref: want ErrRefMismatch, got %v", err)
	}
}

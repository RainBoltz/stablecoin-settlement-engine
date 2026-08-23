package txfee

import (
	"errors"
	"math/big"
	"testing"
)

// TestBump_RaisesBothFields：EIP-1559 的兩個欄位要一起加，只加一個節點不收。
func TestBump_RaisesBothFields(t *testing.T) {
	p := DefaultPolicy()
	next, err := p.Bump(NewFee(30, 2))
	if err != nil {
		t.Fatal(err)
	}
	if got := next.String(); got != "cap 33.000 gwei tip 2.200 gwei" {
		t.Fatalf("got %q", got)
	}
}

// TestBump_RoundsUp：節點算的門檻是整數，除不盡的時候捨去會剛好差一個 wei 進不去，所以這裡無條件進位。
func TestBump_RoundsUp(t *testing.T) {
	p := Policy{BumpPercent: 10}
	// 5 wei 加一成是 5.5 wei，進位成 6。捨去的話會是 5，等於沒加價。
	next, err := p.Bump(Fee{Cap: big.NewInt(5), Tip: big.NewInt(5)})
	if err != nil {
		t.Fatal(err)
	}
	if next.Cap.Int64() != 6 || next.Tip.Int64() != 6 {
		t.Fatalf("got cap=%s tip=%s, want 6/6", next.Cap, next.Tip)
	}
}

// TestBump_StopsAtCeiling：出價到頂就回 ErrCeiling，而且不回一個「勉強等於天花板」的價，
// 那個價贏不過舊交易，送出去只是浪費一次嘗試。
func TestBump_StopsAtCeiling(t *testing.T) {
	p := DefaultPolicy()
	f := p.Base
	for i := 0; i < 4; i++ {
		next, err := p.Bump(f)
		if err != nil {
			t.Fatalf("bump %d: %v", i+1, err)
		}
		f = next
	}
	if got := f.String(); got != "cap 43.923 gwei tip 2.928 gwei" {
		t.Fatalf("after four bumps: %q", got)
	}
	if _, err := p.Bump(f); !errors.Is(err, ErrCeiling) {
		t.Fatalf("fifth bump: %v", err)
	}
}

// TestBump_ZeroFeeFallsBackToBase：沒有紀錄的時候拿零去加價還是零，所以 Bump 自己補上基準價。
func TestBump_ZeroFeeFallsBackToBase(t *testing.T) {
	p := DefaultPolicy()
	next, err := p.Bump(Fee{})
	if err != nil {
		t.Fatal(err)
	}
	if got := next.String(); got != "cap 33.000 gwei tip 2.200 gwei" {
		t.Fatalf("got %q", got)
	}
}

// TestFee_String：印出來的價要看得見加價的尾數，文章與 Report 直接貼這個格式。
func TestFee_String(t *testing.T) {
	if got := (Fee{}).String(); got != "cap - tip -" {
		t.Fatalf("zero fee: %q", got)
	}
	f := Fee{Cap: big.NewInt(36_300_000_000), Tip: big.NewInt(2_420_000_000)}
	if got := f.String(); got != "cap 36.300 gwei tip 2.420 gwei" {
		t.Fatalf("got %q", got)
	}
}

package txseq

import (
	"context"
	"testing"
)

// 不需要排隊的鏈（Solana、SUI）拿到的是一個空位置，而且永遠不會被擋住：這是「同一個帳戶可以同時送好幾筆」的形式化。
func TestUnordered_NeverBlocksAndNeverAllocates(t *testing.T) {
	var u Unordered
	for i := 0; i < 3; i++ {
		r, err := u.Reserve(context.Background(), acct)
		if err != nil || r.Ordered || r.Value != 0 || r.Account != acct {
			t.Fatalf("Reserve = %+v, %v; want an unordered slot for %s", r, err, acct)
		}
		if err := u.Resolve(context.Background(), r, SentUnknown); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
}

// Reservation 的字串是 log 與 Example 會印的東西，兩種形式各釘一次。
func TestReservation_String(t *testing.T) {
	if got, want := (Reservation{Account: acct, Value: 7, Ordered: true}).String(), "0x90F7…b906 #7"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got, want := (Reservation{Account: acct}).String(), "no slot needed"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// Status 的欄寬固定，文章會直接貼這段輸出。
func TestStatus_String(t *testing.T) {
	c := NewCounter()
	r, _ := c.Reserve(context.Background(), acct)
	if got, want := c.Status(acct).String(), "0x90F7…b906  next 1    in-flight yes  gap -"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	_ = c.Resolve(context.Background(), r, SentUnknown)
	if got, want := c.Status(acct).String(), "0x90F7…b906  next 1    in-flight -    gap 0"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got := c.Accounts(); len(got) != 1 || got[0] != acct {
		t.Fatalf("Accounts() = %v, want [%s]", got, acct)
	}
}

package txseq

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const acct = "0x90F79bf6EB2c4f870365E785982E1f101E93b906"

// reserve 取一個號，失敗就讓測試爆掉。
func reserve(t *testing.T, c *Counter, account string) Reservation {
	t.Helper()
	r, err := c.Reserve(context.Background(), account)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	return r
}

// resolve 收尾，失敗就讓測試爆掉。
func resolve(t *testing.T, c *Counter, r Reservation, s Sent) {
	t.Helper()
	if err := c.Resolve(context.Background(), r, s); err != nil {
		t.Fatalf("Resolve(%s): %v", s, err)
	}
}

// 一個帳戶連續取號拿到的是連號。這是 EVM nonce 最基本的要求：跳號的交易會卡在 mempool。
func TestCounter_ReserveHandsOutConsecutiveValues(t *testing.T) {
	c := NewCounter()
	for want := uint64(0); want < 3; want++ {
		r := reserve(t, c, acct)
		if r.Value != want || !r.Ordered {
			t.Fatalf("Reserve = %d ordered=%v, want %d ordered=true", r.Value, r.Ordered, want)
		}
		resolve(t, c, r, SentYes)
	}
}

// 確定沒出門的號要退回去給下一筆用，不然每一次簽名失敗都在序列上戳一個洞。
func TestCounter_NotSentReturnsTheValue(t *testing.T) {
	c := NewCounter()
	first := reserve(t, c, acct)
	resolve(t, c, first, SentNo)
	again := reserve(t, c, acct)
	if again.Value != first.Value {
		t.Fatalf("Reserve after not-sent = %d, want %d", again.Value, first.Value)
	}
}

// 不知道有沒有出門的號不退回去，而且這個帳戶從此不發號：後面的交易排在一個可能不存在的交易後面，
// 在 EVM 上就是整批卡在 mempool。
func TestCounter_UnknownLeavesAGapAndStopsTheAccount(t *testing.T) {
	c := NewCounter()
	r := reserve(t, c, acct)
	resolve(t, c, r, SentUnknown)

	st := c.Status(acct)
	if !st.HasGap || st.Gap != r.Value || st.Next != r.Value+1 {
		t.Fatalf("Status = %+v, want gap %d next %d", st, r.Value, r.Value+1)
	}
	if _, err := c.Reserve(context.Background(), acct); !errors.Is(err, ErrGap) {
		t.Fatalf("Reserve with a gap = %v, want ErrGap", err)
	}
}

// 洞消失的唯一方式是鏈上走過它：Sync 看到鏈上的號已經超過那一格，代表那筆下落不明的交易其實上鏈了。
func TestCounter_SyncClearsAGapTheChainWalkedPast(t *testing.T) {
	c := NewCounter()
	resolve(t, c, reserve(t, c, acct), SentUnknown) // 洞在 0
	if err := c.Sync(context.Background(), acct, 1); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if st := c.Status(acct); st.HasGap {
		t.Fatalf("Status = %+v, want no gap", st)
	}
	if r := reserve(t, c, acct); r.Value != 1 {
		t.Fatalf("Reserve after sync = %d, want 1", r.Value)
	}
}

// 鏈上還沒走過那一格，洞就還在：我們不能自己決定一個沒交代的序號不算數。
func TestCounter_SyncKeepsAGapTheChainDidNotWalkPast(t *testing.T) {
	c := NewCounter()
	resolve(t, c, reserve(t, c, acct), SentYes)     // 0 用掉
	resolve(t, c, reserve(t, c, acct), SentUnknown) // 洞在 1
	if err := c.Sync(context.Background(), acct, 1); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if st := c.Status(acct); !st.HasGap || st.Gap != 1 {
		t.Fatalf("Status = %+v, want gap 1", st)
	}
}

// 有號在手上時不給對齊：那一筆正在送，改掉計數器它收尾時就對不上了。
func TestCounter_SyncRejectsAnOutstandingAccount(t *testing.T) {
	c := NewCounter()
	r := reserve(t, c, acct)
	if err := c.Sync(context.Background(), acct, 99); !errors.Is(err, ErrBusy) {
		t.Fatalf("Sync while outstanding = %v, want ErrBusy", err)
	}
	resolve(t, c, r, SentYes)
}

// 同一個號收尾兩次是 bug（例如 relayer 兩條路徑都 defer 了），第二次要看得出來，不能默默改到別人的號。
func TestCounter_ResolveTwiceIsStale(t *testing.T) {
	c := NewCounter()
	r := reserve(t, c, acct)
	resolve(t, c, r, SentYes)
	if err := c.Resolve(context.Background(), r, SentNo); !errors.Is(err, ErrStale) {
		t.Fatalf("second Resolve = %v, want ErrStale", err)
	}
}

// 不是這個 Counter 發出去的號（或帳戶根本沒看過）也是 ErrStale。
func TestCounter_ResolveUnknownReservationIsStale(t *testing.T) {
	c := NewCounter()
	err := c.Resolve(context.Background(), Reservation{Account: acct, Value: 7, Ordered: true}, SentYes)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("Resolve = %v, want ErrStale", err)
	}
}

// 窗口是 1：第二個人要等第一個收尾。這是「同一個帳戶一次只有一筆在飛」的實際保證。
func TestCounter_SecondReserveWaitsForTheFirst(t *testing.T) {
	c := NewCounter()
	first := reserve(t, c, acct)

	got := make(chan Reservation, 1)
	go func() {
		r, err := c.Reserve(context.Background(), acct)
		if err == nil {
			got <- r
		}
	}()
	select {
	case r := <-got:
		t.Fatalf("second Reserve returned %v while the first is outstanding", r)
	case <-time.After(20 * time.Millisecond):
	}
	resolve(t, c, first, SentYes)
	select {
	case r := <-got:
		if r.Value != first.Value+1 {
			t.Fatalf("second Reserve = %d, want %d", r.Value, first.Value+1)
		}
	case <-time.After(time.Second):
		t.Fatal("second Reserve did not return after the first resolved")
	}
}

// 等不到就放手：ctx 結束時 Reserve 要回錯，呼叫端才能把工作原封不動放回 queue。
func TestCounter_ReserveGivesUpWhenContextEnds(t *testing.T) {
	c := NewCounter()
	r := reserve(t, c, acct)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.Reserve(ctx, acct); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reserve = %v, want DeadlineExceeded", err)
	}
	resolve(t, c, r, SentYes)
}

// 序列化的範圍是帳戶：另一個錢包不受影響，這就是提高吞吐的辦法。
func TestCounter_AccountsAreIndependent(t *testing.T) {
	c := NewCounter()
	held := reserve(t, c, acct)
	other := reserve(t, c, "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC")
	if other.Value != 0 {
		t.Fatalf("other account Reserve = %d, want 0", other.Value)
	}
	resolve(t, c, held, SentYes)
	resolve(t, c, other, SentYes)
}

// 併發下每個號只能發給一個人。用 -race -count=30 跑。
func TestCounter_ConcurrentReservesHandOutEachValueOnce(t *testing.T) {
	c := NewCounter()
	const n = 20
	var mu sync.Mutex
	seen := make(map[uint64]int, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.Reserve(context.Background(), acct)
			if err != nil {
				t.Errorf("Reserve: %v", err)
				return
			}
			mu.Lock()
			seen[r.Value]++
			mu.Unlock()
			_ = c.Resolve(context.Background(), r, SentYes)
		}()
	}
	wg.Wait()
	for v := uint64(0); v < n; v++ {
		if seen[v] != 1 {
			t.Fatalf("value %d handed out %d time(s), want 1 (seen=%v)", v, seen[v], seen)
		}
	}
}

// TestCounter_ReserveGapHandsTheSameNumberBack：洞那一格的號可以再發一次，而且發出去的 Reservation
// 帶著 Fill，Resolve 才知道這一次不能撥計數器。
func TestCounter_ReserveGapHandsTheSameNumberBack(t *testing.T) {
	ctx := context.Background()
	c := NewCounter()
	gapAcct := "0xabc"
	r, err := c.Reserve(ctx, gapAcct)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Resolve(ctx, r, SentUnknown); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(gapAcct); !st.HasGap || st.Gap != 0 || st.Next != 1 {
		t.Fatalf("after unknown: %s", st)
	}
	g, err := c.ReserveGap(ctx, gapAcct)
	if err != nil {
		t.Fatal(err)
	}
	if g.Value != 0 || !g.Fill || !g.Ordered {
		t.Fatalf("gap reservation: %+v", g)
	}
	if got := g.String(); got != "0xabc #0 fill" {
		t.Fatalf("String() = %q", got)
	}
}

// TestCounter_FilledGapReopensTheAccount：補洞的交易送出去了就當這一格有人認領，洞消失、帳戶恢復發號。
// 計數器不會因此往前，那一格早就算用掉了。
func TestCounter_FilledGapReopensTheAccount(t *testing.T) {
	ctx := context.Background()
	c := NewCounter()
	gapAcct := "0xabc"
	r, _ := c.Reserve(ctx, gapAcct)
	_ = c.Resolve(ctx, r, SentUnknown)
	g, _ := c.ReserveGap(ctx, gapAcct)
	if err := c.Resolve(ctx, g, SentYes); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(gapAcct); st.HasGap || st.Next != 1 || st.InFlight {
		t.Fatalf("after fill: %s", st)
	}
	next, err := c.Reserve(ctx, gapAcct)
	if err != nil {
		t.Fatalf("account should be issuing again: %v", err)
	}
	if next.Value != 1 || next.Fill {
		t.Fatalf("next reservation: %+v", next)
	}
}

// TestCounter_FailedFillKeepsTheGap：補洞的交易沒送出去（或不知道），洞就留著等下一次；
// 計數器也不會被撥回去，因為那一格可能真的有一筆交易在鏈上。
func TestCounter_FailedFillKeepsTheGap(t *testing.T) {
	ctx := context.Background()
	for _, s := range []Sent{SentNo, SentUnknown} {
		c := NewCounter()
		gapAcct := "0xabc"
		r, _ := c.Reserve(ctx, gapAcct)
		_ = c.Resolve(ctx, r, SentUnknown)
		g, _ := c.ReserveGap(ctx, gapAcct)
		if err := c.Resolve(ctx, g, s); err != nil {
			t.Fatal(err)
		}
		st := c.Status(gapAcct)
		if !st.HasGap || st.Gap != 0 || st.Next != 1 {
			t.Fatalf("%s: %s", s, st)
		}
		if _, err := c.Reserve(ctx, gapAcct); !errors.Is(err, ErrGap) {
			t.Fatalf("%s: account should still be stopped, got %v", s, err)
		}
	}
}

// TestCounter_ReserveGapWithoutGap：沒有洞就沒有東西可以替換。呼叫端拿到 ErrNoGap 要去走一般的 Reserve，
// 不是把它當錯誤往上丟。
func TestCounter_ReserveGapWithoutGap(t *testing.T) {
	ctx := context.Background()
	c := NewCounter()
	if _, err := c.ReserveGap(ctx, "0xabc"); !errors.Is(err, ErrNoGap) {
		t.Fatalf("got %v", err)
	}
	// semaphore 要還回去，不然這個帳戶會從此發不出號。
	if _, err := c.Reserve(ctx, "0xabc"); err != nil {
		t.Fatalf("account should still be usable: %v", err)
	}
}

// TestUnordered_ReserveGap：不發號的鏈沒有序列，也就沒有洞。
func TestUnordered_ReserveGap(t *testing.T) {
	if _, err := (Unordered{}).ReserveGap(context.Background(), "sol"); !errors.Is(err, ErrNoGap) {
		t.Fatalf("got %v", err)
	}
}

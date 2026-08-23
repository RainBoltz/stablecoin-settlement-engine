package txfail

import (
	"testing"
	"time"
)

// TestPolicy_BackoffDoublesAndCaps：退避階梯的形狀。關掉抖動才看得到那個數字本身。
func TestPolicy_BackoffDoublesAndCaps(t *testing.T) {
	p := Policy{MaxAttempts: 10, MaxBackoff: 40 * time.Second}
	want := []time.Duration{5, 10, 20, 40, 40, 40}
	for i, w := range want {
		attempt := uint64(i + 1)
		if got := p.Backoff(5*time.Second, attempt); got != w*time.Second {
			t.Fatalf("attempt %d: got %s, want %s", attempt, got, w*time.Second)
		}
	}
}

// TestPolicy_BackoffWithoutCeilingKeepsDoubling：MaxBackoff 是零就是沒有上限。
func TestPolicy_BackoffWithoutCeilingKeepsDoubling(t *testing.T) {
	p := Policy{MaxAttempts: 10}
	if got := p.Backoff(time.Second, 5); got != 16*time.Second {
		t.Fatalf("got %s, want 16s", got)
	}
}

// TestPolicy_BackoffHugeAttemptDoesNotWrap：第幾次交付是 queue 給的，可以很大。
// 這條在防用位移實作的那個版本：位移 64 次會繞回零，退避變成不退。
func TestPolicy_BackoffHugeAttemptDoesNotWrap(t *testing.T) {
	p := Policy{MaxBackoff: time.Minute}
	if got := p.Backoff(5*time.Second, 1<<40); got != time.Minute {
		t.Fatalf("got %s, want the ceiling", got)
	}
}

// TestPolicy_BackoffZeroBaseIsZero：沒給第一階就沒有階梯可爬。
func TestPolicy_BackoffZeroBaseIsZero(t *testing.T) {
	if got := DefaultPolicy().Backoff(0, 3); got != 0 {
		t.Fatalf("got %s, want 0", got)
	}
}

// TestEqualJitter_StaysBetweenHalfAndFull：抖動只在 [d/2, d] 裡動。下限保住「真的退了一步」，
// 上限保住「不會比原本算出來的還久」。
func TestEqualJitter_StaysBetweenHalfAndFull(t *testing.T) {
	const d = 8 * time.Second
	seen := make(map[time.Duration]bool)
	for i := 0; i < 2000; i++ {
		got := EqualJitter(d)
		if got < d/2 || got > d {
			t.Fatalf("got %s, want between %s and %s", got, d/2, d)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Fatal("equal jitter should actually spread the retries out")
	}
}

// TestEqualJitter_NonPositiveIsZero：零與負數不進隨機數產生器（rand.N 對非正數會 panic）。
func TestEqualJitter_NonPositiveIsZero(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if got := EqualJitter(d); got != 0 {
			t.Fatalf("EqualJitter(%s) = %s, want 0", d, got)
		}
	}
}

// TestDefaultPolicy_BudgetOutlastsStuckAfter：預設的預算要撐得過 relayer 的 StuckAfter（5 分鐘），
// 不然一筆卡在 settling 的付款會在救援發生之前就先被判死。這條測試釘的是兩個 package 之間的那個約定。
func TestDefaultPolicy_BudgetOutlastsStuckAfter(t *testing.T) {
	p := DefaultPolicy()
	p.Jitter = nil
	var total time.Duration
	for attempt := uint64(1); attempt < uint64(p.MaxAttempts); attempt++ {
		total += p.Backoff(5*time.Second, attempt)
	}
	if total <= 5*time.Minute {
		t.Fatalf("budget spans %s, want longer than the 5m StuckAfter", total)
	}
}

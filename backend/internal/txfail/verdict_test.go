package txfail

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func testPolicy() Policy { return Policy{MaxAttempts: 4, MaxBackoff: time.Minute} }

// TestJudge_DeclaredPoisonStopsAtTheFirstDelivery：宣告過的錯誤不用陪跑完整個階梯。
func TestJudge_DeclaredPoisonStopsAtTheFirstDelivery(t *testing.T) {
	v := testPolicy().Judge(Fault{Err: fmt.Errorf("%w: no signer for evm:31337", ErrPoison), Attempt: 1, Base: time.Second})
	if v.Class != ClassPoison || v.Reason != "retrying will not help" {
		t.Fatalf("got %s", v)
	}
	if v.Backoff != 0 {
		t.Fatalf("poison should not carry a backoff, got %s", v.Backoff)
	}
}

// TestJudge_PoisonSurvivesTwoWraps：宣告放在最裡層也算數。Sender 常常同時要說兩件事
// （這筆確定沒發送出去、而且重試不會好），兩個 %w 包在一起要都拆得開。
func TestJudge_PoisonSurvivesTwoWraps(t *testing.T) {
	outer := errors.New("relayer: transaction was not sent")
	err := fmt.Errorf("send: %w", fmt.Errorf("%w: %w: unknown token", outer, ErrPoison))
	if v := testPolicy().Judge(Fault{Err: err, Attempt: 1, Base: time.Second}); v.Class != ClassPoison {
		t.Fatalf("got %s", v)
	}
	if !errors.Is(err, outer) {
		t.Fatal("the other wrapped error should still be visible")
	}
}

// TestJudge_TransientFailuresBackOff：沒有宣告的失敗照階梯退避，預算內每一次都還能再來。
func TestJudge_TransientFailuresBackOff(t *testing.T) {
	p := testPolicy()
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}
	for i, w := range want {
		attempt := uint64(i + 1)
		v := p.Judge(Fault{Err: errors.New("rpc: timeout"), Attempt: attempt, Base: 5 * time.Second})
		if v.Class != ClassRetryable || v.Backoff != w {
			t.Fatalf("attempt %d: got %s, want retryable %s", attempt, v, w)
		}
	}
}

// TestJudge_BudgetTurnsTheLastDeliveryIntoPoison：第 MaxAttempts 次失敗的當下預算就是零，
// 不是「再放行一次」。
func TestJudge_BudgetTurnsTheLastDeliveryIntoPoison(t *testing.T) {
	p := testPolicy()
	if v := p.Judge(Fault{Err: errors.New("rpc: timeout"), Attempt: 3, Base: time.Second}); v.Class != ClassRetryable {
		t.Fatalf("third delivery: got %s", v)
	}
	v := p.Judge(Fault{Err: errors.New("rpc: timeout"), Attempt: 4, Base: time.Second})
	if v.Class != ClassPoison || v.Reason != "no luck after 4 deliveries" {
		t.Fatalf("fourth delivery: got %s", v)
	}
}

// TestJudge_NilErrorIsJudgedToo：「還沒 authorized」「輸掉 CAS」這些失敗沒有錯誤物件，
// 也不會有人宣告它們，所以只有預算收得掉。
func TestJudge_NilErrorIsJudgedToo(t *testing.T) {
	p := testPolicy()
	if v := p.Judge(Fault{Attempt: 1, Base: time.Second}); v.Class != ClassRetryable {
		t.Fatalf("first: got %s", v)
	}
	if v := p.Judge(Fault{Attempt: 9, Base: time.Second}); v.Class != ClassPoison {
		t.Fatalf("ninth: got %s", v)
	}
}

// TestJudge_ZeroMaxAttemptsNeverRunsOut：把預算設成零就是「不限次數」，留給那些寧可一直重試的佇列。
func TestJudge_ZeroMaxAttemptsNeverRunsOut(t *testing.T) {
	p := Policy{MaxBackoff: time.Minute}
	if v := p.Judge(Fault{Err: errors.New("rpc: timeout"), Attempt: 1 << 20, Base: time.Second}); v.Class != ClassRetryable {
		t.Fatalf("got %s", v)
	}
}

// TestVerdict_String：Example 直接貼這個格式，欄寬跟著測試走。
func TestVerdict_String(t *testing.T) {
	got := Verdict{Class: ClassRetryable, Backoff: 5 * time.Second, Reason: "why"}.String()
	if got != "retryable 5s      why" {
		t.Fatalf("retryable: %q", got)
	}
	if got := (Verdict{Class: ClassPoison, Reason: "why"}).String(); got != "poison    -       why" {
		t.Fatalf("poison: %q", got)
	}
}

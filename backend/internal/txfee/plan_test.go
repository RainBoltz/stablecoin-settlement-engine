package txfee

import (
	"strings"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

const stuckAfter = 5 * time.Minute

// TestDecide_Table 把決策樹整張釘死：改任何一條分支，這裡會先叫。
func TestDecide_Table(t *testing.T) {
	p := DefaultPolicy()
	cases := []struct {
		name string
		in   Stuck
		want Kind
		fee  string
	}{
		{"還年輕就等，不管上一次的結果是什麼", Stuck{Sent: txseq.SentUnknown, Fee: p.Base, Tries: 1, Age: time.Minute}, KindWait, ""},
		{"確定沒發送出去也要等：lease 可能過期了而上一個 worker 還在送",
			Stuck{Sent: txseq.SentNo, Fee: p.Base, Tries: 1, Age: time.Minute}, KindWait, ""},
		{"卡夠久了就加價重送", Stuck{Sent: txseq.SentUnknown, Fee: p.Base, Tries: 1, Age: 6 * time.Minute},
			KindSpeedUp, "cap 33.000 gwei tip 2.200 gwei"},
		{"確定沒發送出去的話沒有東西要贏，從基準價重來",
			Stuck{Sent: txseq.SentNo, Fee: NewFee(39, 3), Tries: 1, Age: 6 * time.Minute},
			KindSpeedUp, "cap 30.000 gwei tip 2.000 gwei"},
		{"送出去了卻沒寫回 confirming，一樣當成鏈上有東西要贏",
			Stuck{Sent: txseq.SentYes, Fee: p.Base, Tries: 1, Age: 6 * time.Minute},
			KindSpeedUp, "cap 33.000 gwei tip 2.200 gwei"},
		{"廣播次數用完就不再救這筆付款，改成把號清出來",
			Stuck{Sent: txseq.SentUnknown, Fee: p.Base, Tries: 3, Age: 6 * time.Minute},
			KindCancel, "cap 33.000 gwei tip 2.200 gwei"},
		{"出價到頂就送審：加速與取消都贏不過舊交易",
			Stuck{Sent: txseq.SentUnknown, Fee: NewFee(44, 3), Tries: 1, Age: 6 * time.Minute}, KindReview, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.in.StuckAfter = stuckAfter
			got := p.Decide(c.in)
			if got.Kind != c.want {
				t.Fatalf("kind %q, want %q (%s)", got.Kind, c.want, got.Reason)
			}
			if c.fee != "" && got.Fee.String() != c.fee {
				t.Fatalf("fee %q, want %q", got.Fee, c.fee)
			}
			if c.fee == "" && !got.Fee.Zero() {
				t.Fatalf("expected no fee, got %s", got.Fee)
			}
			if got.Reason == "" {
				t.Fatal("every plan needs a reason: it ends up in the intent history")
			}
		})
	}
}

// TestDecide_CeilingReasonNamesBothNumbers：送審的理由要寫得出「加到多少、天花板是多少」，
// 因為它會原封不動變成 needs_review 的理由，人得看得懂。
func TestDecide_CeilingReasonNamesBothNumbers(t *testing.T) {
	p := DefaultPolicy()
	got := p.Decide(Stuck{Sent: txseq.SentUnknown, Fee: NewFee(44, 3), Tries: 1, Age: 6 * time.Minute, StuckAfter: stuckAfter})
	if !strings.Contains(got.Reason, "48.400 gwei") || !strings.Contains(got.Reason, "45.000 gwei") {
		t.Fatalf("reason = %q", got.Reason)
	}
}

// TestDecide_EventuallyStops：一筆一直救不起來的付款不能無限迴圈。從基準價開始一路餵回去，
// 幾輪之內一定會走到 cancel，再走到 review。
func TestDecide_EventuallyStops(t *testing.T) {
	p := DefaultPolicy()
	s := Stuck{Sent: txseq.SentUnknown, Fee: p.Base, Tries: 1, Age: 6 * time.Minute, StuckAfter: stuckAfter}
	seen := map[Kind]int{}
	for i := 0; i < 20; i++ {
		plan := p.Decide(s)
		seen[plan.Kind]++
		if plan.Kind == KindReview {
			break
		}
		s.Fee, s.Tries = plan.Fee, s.Tries+1
	}
	if seen[KindCancel] == 0 || seen[KindReview] != 1 {
		t.Fatalf("never settled down: %v", seen)
	}
}

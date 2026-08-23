package txfee_test

import (
	"fmt"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfee"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// Example_ladder 是一筆卡在 settling 的付款被救到最後的樣子：先等，然後每一輪加價一成再送一次，
// 廣播次數用完就改送取消交易把號清出來，出價到天花板就交給人。
//
// 每一行左邊是這筆付款現在的樣子（卡多久、上一次廣播的結果），右邊是 Decide 回的決定。
// 出價的尾數是重點：33 gwei 加一成是 36.3，那個小數不能捨去，捨去就差一個 wei 進不了節點。
func Example_ladder() {
	p := txfee.DefaultPolicy()
	fmt.Printf("policy  bump %d%%, cap ceiling %s, at most %d broadcasts\n\n",
		p.BumpPercent, txfee.Gwei(p.MaxCap), p.MaxTries)

	s := txfee.Stuck{Sent: txseq.SentUnknown, Fee: p.Base, Tries: 1, Age: time.Minute, StuckAfter: 5 * time.Minute}
	for i := 0; i < 7; i++ {
		plan := p.Decide(s)
		fmt.Printf("%-22s %s\n", fmt.Sprintf("stuck %s, %s", s.Age, s.Sent), plan)
		if plan.Kind == txfee.KindReview {
			break
		}
		s.Age = 6 * time.Minute
		if plan.Kind != txfee.KindWait {
			s.Fee, s.Tries = plan.Fee, s.Tries+1
		}
	}

	// 上一次「確定沒發送出去」的話鏈上沒有東西要贏，號昨天就退回去了，照基準價重來一次就好。
	fmt.Println()
	notSent := txfee.Stuck{Sent: txseq.SentNo, Fee: txfee.NewFee(39, 3), Tries: 1, Age: 6 * time.Minute, StuckAfter: 5 * time.Minute}
	fmt.Printf("%-22s %s\n", fmt.Sprintf("stuck %s, %s", notSent.Age, notSent.Sent), p.Decide(notSent))

	// Output:
	// policy  bump 10%, cap ceiling 45.000 gwei, at most 3 broadcasts
	//
	// stuck 1m0s, unknown    wait      settling for 1m0s without tx hash, waiting
	// stuck 6m0s, unknown    speed-up  cap 33.000 gwei tip 2.200 gwei (try 2, last broadcast unknown)
	// stuck 6m0s, unknown    speed-up  cap 36.300 gwei tip 2.420 gwei (try 3, last broadcast unknown)
	// stuck 6m0s, unknown    cancel    cap 39.930 gwei tip 2.662 gwei (broadcast 3 times already, giving up on this payment)
	// stuck 6m0s, unknown    cancel    cap 43.923 gwei tip 2.928 gwei (broadcast 4 times already, giving up on this payment)
	// stuck 6m0s, unknown    review    txfee: bumped fee cap exceeds the ceiling: 48.315 gwei > 45.000 gwei
	//
	// stuck 6m0s, not-sent   speed-up  cap 30.000 gwei tip 2.000 gwei (try 2, last broadcast not-sent)
}

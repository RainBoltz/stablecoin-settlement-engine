package txfail_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfail"
)

// Example_budget 是一份一直失敗的 job 被判到最後的樣子：退避每次加倍、封頂，然後在預算用完的那一次停下來。
//
// 左邊的「at」是這一次交付發生在第一次失敗之後多久。最後一行的 10m35s 是重點：它比 relayer 的 StuckAfter
// 五分鐘長，所以一筆卡在 settling 的付款來得及先被救援接手，才輪得到預算把它收掉。
//
// 最後一行換一種失敗：錯誤自己宣告了 ErrPoison，第一次交付就停，不用陪跑階梯。
func Example_budget() {
	p := txfail.DefaultPolicy()
	p.Jitter = nil // 關掉抖動，輸出才固定；上線要開，理由見 Policy.Jitter
	const base = 5 * time.Second
	fmt.Printf("policy  at most %d deliveries; back off %s doubling, capped at %s\n\n",
		p.MaxAttempts, base, p.MaxBackoff)

	var at time.Duration
	for attempt := uint64(1); attempt <= uint64(p.MaxAttempts); attempt++ {
		v := p.Judge(txfail.Fault{Err: errors.New("rpc: timeout"), Attempt: attempt, Base: base})
		fmt.Printf("delivery %-2d at %-8s %s\n", attempt, at, v)
		at += v.Backoff
	}

	fmt.Println()
	declared := fmt.Errorf("%w: evm:31337 has no signer configured", txfail.ErrPoison)
	fmt.Printf("delivery %-2d at %-8s %s\n", 1, time.Duration(0), p.Judge(txfail.Fault{Err: declared, Attempt: 1, Base: base}))

	// Output:
	// policy  at most 10 deliveries; back off 5s doubling, capped at 2m0s
	//
	// delivery 1  at 0s       retryable 5s      delivery 1 failed, next one in 5s
	// delivery 2  at 5s       retryable 10s     delivery 2 failed, next one in 10s
	// delivery 3  at 15s      retryable 20s     delivery 3 failed, next one in 20s
	// delivery 4  at 35s      retryable 40s     delivery 4 failed, next one in 40s
	// delivery 5  at 1m15s    retryable 1m20s   delivery 5 failed, next one in 1m20s
	// delivery 6  at 2m35s    retryable 2m0s    delivery 6 failed, next one in 2m0s
	// delivery 7  at 4m35s    retryable 2m0s    delivery 7 failed, next one in 2m0s
	// delivery 8  at 6m35s    retryable 2m0s    delivery 8 failed, next one in 2m0s
	// delivery 9  at 8m35s    retryable 2m0s    delivery 9 failed, next one in 2m0s
	// delivery 10 at 10m35s   poison    -       no luck after 10 deliveries
	//
	// delivery 1  at 0s       poison    -       retrying will not help
}

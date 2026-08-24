package finality_test

import (
	"fmt"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
)

// Example_twoRulers 是同一筆 EVM 交易、兩把尺：
//
//   - fast 只數深度，壓 2 個區塊就算（Circle 的 Fast Transfer 在 Ethereum 上就是這個數字）。
//   - hard 等鏈自己的 finalized tag，預設就是它。
//
// 左邊是節點看到的 head。交易在 100 進區塊，fast 在 101 就放行，hard 要等到 finalized 追上 100（這裡假設 head 到 164 時追上）。
// 最後兩行是同一個位置的兩種例外：執行失敗的交易也要等到不可逆才判 failed；始終不在任何區塊裡的，過了 LostAfter 判 lost。
func Example_twoRulers() {
	fast := finality.Policy{Confirmations: 2, LostAfter: 5 * time.Minute}
	hard := finality.Defaults()["evm"]
	fmt.Printf("policy  fast  %s\n", fast)
	fmt.Printf("policy  hard  %s\n\n", hard)

	steps := []struct {
		head  uint64
		final bool
	}{{99, false}, {100, false}, {101, false}, {164, true}}
	for _, s := range steps {
		obs := finality.Observation{Included: s.head >= 100, Height: 100, Head: s.head, Final: s.final, Succeeded: true}
		fmt.Printf("head %-4d fast  %s\n", s.head, fast.Judge(obs, 0))
		fmt.Printf("head %-4d hard  %s\n", s.head, hard.Judge(obs, 0))
	}

	fmt.Println()
	reverted := finality.Observation{Included: true, Height: 100, Head: 164, Final: true, Succeeded: false}
	fmt.Printf("head %-4d hard  %s\n", reverted.Head, hard.Judge(reverted, 0))
	fmt.Printf("head %-4d hard  %s\n", 164, hard.Judge(finality.Observation{Head: 164}, 5*time.Minute))

	// Output:
	// policy  fast  final when 2 confirmations; lost after 5m0s
	// policy  hard  final when finalized; lost after 5m0s
	//
	// head 99   fast  pending  not in any block yet
	// head 99   hard  pending  not in any block yet
	// head 100  fast  pending  included at 100, 1 of 2 confirmations
	// head 100  hard  pending  included at 100, 1 deep, not yet finalized
	// head 101  fast  final    2 confirmations at 100
	// head 101  hard  pending  included at 100, 2 deep, not yet finalized
	// head 164  fast  final    65 confirmations at 100
	// head 164  hard  final    finalized at 100, 65 deep
	//
	// head 164  hard  failed   finalized at 100, 65 deep but the execution failed; gas burned, nothing moved
	// head 164  hard  lost     not in any block for 5m0s; dropped or reorged out
}

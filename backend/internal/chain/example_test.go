package chain_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfee"
)

// commas 每三位加一個逗號，跟 bulk 印報告的格式一致。
func commas(n uint64) string {
	s := strconv.FormatUint(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Example_askTheRegistry 先拿四筆 intent 的鏈名去查 adapter，再把四個 adapter
// 各問一輪：取一個位置、能不能替換、什麼算不可逆、一筆交易裝得下多少。
// 每一行都是鏈下某個元件真的會問的問題。四條鏈今天第一次到齊，而四條的答案沒有兩條完全一樣：
// ton 的最後一行數的是一則 external message 裝幾則付款 message，sui 的最後一行數的是一個 PTB
// 裝幾個 command，兩個都不是「一筆交易裡有幾筆付款」。
func Example_askTheRegistry() {
	ctx := context.Background()
	reg := chain.Default()
	fmt.Println("registry", strings.Join(reg.Protocols(), ", "))

	for _, id := range []string{"evm:31337", "solana:mainnet-beta", "ton:mainnet", "sui:mainnet"} {
		a, err := reg.For(id)
		if err != nil {
			fmt.Printf("%-20s -> %v\n", id, err)
			continue
		}
		fmt.Printf("%-20s -> %s\n", id, a.Protocol())
	}
	fmt.Println()

	wallets := map[string]string{
		"evm":    "0x0A11cE0000000000000000000000000000000001",
		"solana": "payer-wallet",
		"ton":    "0:1111111111111111111111111111111111111111111111111111111111111111",
		"sui":    "payer-address",
	}
	for _, p := range reg.Protocols() {
		a, _ := reg.For(p)
		res, _ := a.Sequencer().Reserve(ctx, wallets[p])
		fmt.Printf("%-7s slot     %s\n", p, res)
		if pol, ok := chain.Replacement(a); ok {
			fmt.Printf("%-7s replace  %s, bump %d%%, ceiling %s, at most %d broadcasts\n",
				p, pol.Base, pol.BumpPercent, txfee.Gwei(pol.MaxCap), pol.MaxTries)
		} else {
			fmt.Printf("%-7s replace  resend the same signed bytes\n", p)
		}
		fmt.Printf("%-7s final    %s\n", p, a.Finality())
		caps := make([]string, 0, 3)
		for _, r := range a.BatchLimits().Rules {
			caps = append(caps, fmt.Sprintf("%s cap %s", r.Unit, commas(r.Cap)))
		}
		fmt.Printf("%-7s batch    %s\n", p, strings.Join(caps, ", "))
	}

	// Output:
	// registry evm, solana, sui, ton
	// evm:31337            -> evm
	// solana:mainnet-beta  -> solana
	// ton:mainnet          -> ton
	// sui:mainnet          -> sui
	//
	// evm     slot     0x0A11…0001 #0
	// evm     replace  cap 30.000 gwei tip 2.000 gwei, bump 10%, ceiling 45.000 gwei, at most 3 broadcasts
	// evm     final    final when finalized; lost after 5m0s
	// evm     batch    gas cap 30,000,000
	// solana  slot     no slot needed
	// solana  replace  resend the same signed bytes
	// solana  final    final when finalized; lost after 2m0s
	// solana  batch    bytes cap 1,232, accounts cap 64
	// sui     slot     no slot needed
	// sui     replace  resend the same signed bytes
	// sui     final    final when checkpoint; lost after 2m0s
	// sui     batch    commands cap 1,024, bytes cap 131,072, objects cap 2,048
	// ton     slot     0:1111111111111111111111111111111111111111111111111111111111111111 #0
	// ton     replace  resend the same signed bytes
	// ton     final    final when masterchain; lost after 2m0s
	// ton     batch    messages cap 255, bytes cap 65,535, depth cap 512
}

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

// Example_askTheRegistry 先拿三筆 intent 的鏈名去查 adapter，再把查得到的兩個 adapter
// 各問一輪：取一個位置、能不能替換、什麼算不可逆、一筆交易裝得下多少。
// 每一行都是鏈下某個元件真的會問的問題；ton 那一行就是沒接的鏈在這個系統裡的長相。
func Example_askTheRegistry() {
	ctx := context.Background()
	reg := chain.Default()
	fmt.Println("registry", strings.Join(reg.Protocols(), ", "))

	for _, id := range []string{"evm:31337", "solana:mainnet-beta", "ton:mainnet"} {
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
		caps := make([]string, 0, 2)
		for _, r := range a.BatchLimits().Rules {
			caps = append(caps, fmt.Sprintf("%s cap %s", r.Unit, commas(r.Cap)))
		}
		fmt.Printf("%-7s batch    %s\n", p, strings.Join(caps, ", "))
	}

	// Output:
	// registry evm, solana
	// evm:31337            -> evm
	// solana:mainnet-beta  -> solana
	// ton:mainnet          -> chain: no adapter registered for this protocol: "ton"
	//
	// evm     slot     0x0A11…0001 #0
	// evm     replace  cap 30.000 gwei tip 2.000 gwei, bump 10%, ceiling 45.000 gwei, at most 3 broadcasts
	// evm     final    final when finalized; lost after 5m0s
	// evm     batch    gas cap 30,000,000
	// solana  slot     no slot needed
	// solana  replace  resend the same signed bytes
	// solana  final    final when finalized; lost after 2m0s
	// solana  batch    bytes cap 1,232, accounts cap 64
}

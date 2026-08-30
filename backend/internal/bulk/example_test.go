package bulk_test

import (
	"fmt"
	"math/big"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// payoutRun 是一份月底的撥款名單：300 筆付款給 300 個 merchant。
// 名單本身跟鏈無關，所以同一份可以拿去問兩條鏈各自要切成幾批。
func payoutRun() []bulk.Payout {
	items := make([]bulk.Payout, 0, 300)
	for i := 1; i <= 300; i++ {
		merchant := fmt.Sprintf("mch-%03d", i)
		items = append(items, bulk.Payout{
			Ref: paymentref.Derive(paymentref.Terms{
				IntentID: fmt.Sprintf("pi_%04d", i),
				Chain:    "payout-run/2026-08",
				Token:    "USDC",
				Payer:    "platform",
				Merchant: merchant,
				Amount:   "100000000",
			}),
			Merchant: merchant,
			Amount:   big.NewInt(100_000_000),
		})
	}
	return items
}

// Example_packAPayoutRun 把同一份名單分別照 EVM 與 Solana 的限制切一次。
// 兩邊差 25 倍不是因為誰比較慢，是兩條鏈拿來限制一筆交易的根本不是同一種資源。
func Example_packAPayoutRun() {
	items := payoutRun()
	fmt.Printf("run     %d payouts, 100 USDC each\n", len(items))

	evm, err := bulk.Pack(items, bulk.Defaults()["evm"])
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(evm)
	fmt.Println(evm.Batches[0])

	// 同一份名單要送上 Solana 之前得先多做一件 EVM 不用做的事：去鏈上查每個 merchant
	// 有沒有地方收這顆 token。這裡假設每 25 個有一個沒有。
	onSolana := make([]bulk.Payout, len(items))
	copy(onSolana, items)
	missing := 0
	for i := range onSolana {
		if (i+1)%25 == 0 {
			onSolana[i].NewTokenAccount = true
			missing++
		}
	}
	fmt.Printf("check   %d of the %d merchants have no token account yet\n", missing, len(onSolana))

	sol, err := bulk.Pack(onSolana, bulk.Defaults()["solana"])
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(sol)
	fmt.Println(sol.Batches[0])
	fmt.Println(sol.Batches[len(sol.Batches)-1])

	// Output:
	// run     300 payouts, 100 USDC each
	// plan    evm      300 payouts  1 batch  0 new accounts  rent 0 wei
	// batch   #1       300 items  gas 16,003,310/30,000,000
	// check   12 of the 300 merchants have no token account yet
	// plan    solana   300 payouts  25 batches  12 new accounts  rent 24,471,360 lamports
	// batch   #1       12 items  bytes 1,187/1,232  accounts 18/64
	// batch   #25      12 items  bytes 1,229/1,232  accounts 19/64
}

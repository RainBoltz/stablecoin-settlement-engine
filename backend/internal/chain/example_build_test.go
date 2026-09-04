package chain_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/big"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// buildRun 造一份 12 筆的名單：EVM 上連一個零頭都不算，Solana 上是一輪兩個區塊的 run。
// merchant 用 EVM 的地址寫法，Solana 那一半不看它，看的是旁邊那份 token 帳戶清單。
func buildRun(n int) ([]bulk.Payout, []chain.Pubkey) {
	items := make([]bulk.Payout, 0, n)
	accounts := make([]chain.Pubkey, 0, n)
	for i := 1; i <= n; i++ {
		merchant := fmt.Sprintf("0x%036x%04x", 10, i)
		items = append(items, bulk.Payout{
			Ref: paymentref.Derive(paymentref.Terms{
				IntentID: fmt.Sprintf("pi_%04d", i),
				Chain:    "payout-run/2026-09",
				Token:    "USDC",
				Payer:    "platform",
				Merchant: merchant,
				Amount:   "100000000",
			}),
			Merchant: merchant,
			Amount:   big.NewInt(100_000_000),
		})
		accounts = append(accounts, key("token-account:"+merchant))
	}
	return items, accounts
}

// countRefs 數名單上的 ref 有幾把真的躺在輸出的 bytes 裡。
func countRefs(out []byte, items []bulk.Payout) int {
	n := 0
	for _, it := range items {
		n += bytes.Count(out, it.Ref[:])
	}
	return n
}

// Example_buildTheSameRunTwice 把同一份 12 筆的名單分別組成兩條鏈的交易內容。
// EVM 一批收完，一份 calldata；Solana 先收成一輪 run（root 是 payer 唯一要簽的東西），
// 再一個區塊一份訊息。兩邊的輸出沒有一個 byte 相同，唯一都找得到的是那 12 把 ref；
// 每行括號裡的對照是 bulk 的估計第一次對上實跑：規則說多少，序列化出來就是多少。
func Example_buildTheSameRunTwice() {
	items, accounts := buildRun(12)
	fmt.Printf("run     %d payouts, 100 USDC each\n", len(items))

	calldata, err := chain.NewEVM().SettleBatchCalldata(
		"0x1000000000000000000000000000000000000001",
		"0x2000000000000000000000000000000000000002", items)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("evm     calldata %s bytes  selector 0x%x  refs %d/%d aboard\n",
		commas(uint64(len(calldata))), calldata[:4], countRefs(calldata, items), len(items))

	run, err := chain.NewSolana().NewRun(items, accounts)
	if err != nil {
		fmt.Println(err)
		return
	}
	root := run.Root()
	fmt.Printf("solana  root %x…  %d leaves  depth %d  %d blocks\n",
		root[:8], 1<<run.Depth(), run.Depth(), run.Blocks())

	plan, err := bulk.Pack(items, bulk.Defaults()["solana"])
	if err != nil {
		fmt.Println(err)
		return
	}
	acc, blockhash := runAccounts(), sha256.Sum256([]byte("recent-blockhash"))
	for _, b := range plan.Batches {
		msg, err := run.PayBatchMessage(acc, blockhash, b.Index-1)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Printf("solana  block %d  signed tx %s bytes  (bulk estimated %s)  refs %d/%d aboard\n",
			b.Index-1, commas(solanaSignedBytes(msg)), commas(b.Used[0].Used),
			countRefs(msg, items), len(items))
	}

	one, oneAccounts := buildRun(1)
	tiny, err := chain.NewSolana().NewRun(one, oneAccounts)
	if err != nil {
		fmt.Println(err)
		return
	}
	msg, err := tiny.PayBatchMessage(acc, blockhash, 0)
	if err != nil {
		fmt.Println(err)
		return
	}
	tinyPlan, err := bulk.Pack(one, bulk.Defaults()["solana"])
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("solana  a 1-payout run: signed tx %s bytes  (bulk estimated %s)\n",
		commas(solanaSignedBytes(msg)), commas(tinyPlan.Batches[0].Used[0].Used))

	// Output:
	// run     12 payouts, 100 USDC each
	// evm     calldata 1,284 bytes  selector 0xd0e1d648  refs 12/12 aboard
	// solana  root 64146f265ad5bb94…  16 leaves  depth 4  2 blocks
	// solana  block 0  signed tx 896 bytes  (bulk estimated 896)  refs 8/12 aboard
	// solana  block 1  signed tx 604 bytes  (bulk estimated 604)  refs 4/12 aboard
	// solana  a 1-payout run: signed tx 352 bytes  (bulk estimated 353)
}

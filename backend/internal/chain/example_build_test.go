package chain_test

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// buildRun 造一份 12 筆的名單：剛好是 Solana 一批的量，在 EVM 上連一個零頭都不算。
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

// Example_buildTheSameBatchTwice 把同一批 12 筆付款分別組成兩條鏈的交易內容。
// 兩邊的輸出沒有一個 byte 相同，唯一都找得到的是那 12 把 ref；Solana 那兩行的重點是
// 估計第一次對上實跑：bulk 的 bytes 規則說多少，序列化出來就是多少。
func Example_buildTheSameBatchTwice() {
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

	acc, blockhash := solanaFixture()
	sol := chain.NewSolana()
	var rule bulk.Rule
	for _, r := range sol.BatchLimits().Rules {
		if r.Unit == "bytes" {
			rule = r
		}
	}
	msg, err := sol.BatchMessage(acc, blockhash, items, accounts)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("solana  message %s bytes, signed tx %s bytes  (bulk estimated %s)  refs %d/%d aboard\n",
		commas(uint64(len(msg))), commas(solanaSignedBytes(msg)),
		commas(rule.Base+uint64(len(items))*rule.Item), countRefs(msg, items), len(items))

	one, err := sol.BatchMessage(acc, blockhash, items[:1], accounts[:1])
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("solana  1 payout: signed tx %s bytes  (bulk estimated %s)\n",
		commas(solanaSignedBytes(one)), commas(rule.Base+rule.Item))

	// Output:
	// run     12 payouts, 100 USDC each
	// evm     calldata 1,284 bytes  selector 0xd0e1d648  refs 12/12 aboard
	// solana  message 1,122 bytes, signed tx 1,187 bytes  (bulk estimated 1,187)  refs 12/12 aboard
	// solana  1 payout: signed tx 383 bytes  (bulk estimated 384)
}

package ledger_test

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// Example_holdPostVoid 是三筆付款在帳本上的長相：
//   - pi_0001 付 100 USDC，鏈上原封不動收到，hold 之後 post，兩條腿。
//   - pi_0002 付 100 USDT，token 抽了 0.1 的稅，金額對不上，operator 判定已付之後 post：
//     三條腿，payer 出 100、merchant 進 99.9、fee 接住 0.1。
//   - pi_0003 付 50 USDC，收款人在黑名單上，永遠失敗，hold 之後 void。
//
// 中間 listener 把 pi_0001 的 post 又送了一次（at-least-once 的日常），是 no-op。
// 最後印餘額：pending 全部歸零，posted 就是鏈上真的發生的事；再走一遍 hash 鏈確認沒有一列被動過。
func Example_holdPostVoid() {
	ctx := context.Background()
	j := ledger.NewMemoryJournal()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const (
		usdc     = "0x5FbDB2315678afecb367f032d93F642f64180aa3" // devnet 的 USDC
		usdt     = "0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512" // devnet 的 USDT
		payer    = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
		merchant = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
	)
	assetUSDC := ledger.Asset{Chain: "evm:31337", Token: usdc}
	assetUSDT := ledger.Asset{Chain: "evm:31337", Token: usdt}
	ref := func(id, token, amount string) paymentref.Ref {
		return paymentref.Derive(paymentref.Terms{IntentID: id, Chain: "evm:31337", Token: token, Payer: payer, Merchant: merchant, Amount: amount})
	}
	legs := func(out, in int64) []ledger.Leg {
		return []ledger.Leg{
			{Account: ledger.PayerAccount(payer), Amount: big.NewInt(-out)},
			{Account: ledger.MerchantAccount(merchant), Amount: big.NewInt(in)},
		}
	}
	add := func(e ledger.Entry) {
		stored, applied, err := j.Append(ctx, e)
		switch {
		case err != nil:
			fmt.Printf("%-16s REJECTED: %v\n", e.ID, err)
		case !applied:
			fmt.Printf("%-16s no-op (already there)\n", e.ID)
		default:
			fmt.Println(stored)
		}
	}

	// relayer：三筆都推到 settling 了，廣播之前先 hold。
	add(ledger.Entry{ID: "pi_0001/hold", Ref: ref("pi_0001", usdc, "100000000"), Kind: ledger.KindHold, Asset: assetUSDC,
		Legs: legs(100_000_000, 100_000_000), By: "relayer", At: now.Add(1 * time.Minute)})
	add(ledger.Entry{ID: "pi_0002/hold", Ref: ref("pi_0002", usdt, "100000000"), Kind: ledger.KindHold, Asset: assetUSDT,
		Legs: legs(100_000_000, 100_000_000), By: "relayer", At: now.Add(1 * time.Minute)})
	add(ledger.Entry{ID: "pi_0003/hold", Ref: ref("pi_0003", usdc, "50000000"), Kind: ledger.KindHold, Asset: assetUSDC,
		Legs: legs(50_000_000, 50_000_000), By: "relayer", At: now.Add(1 * time.Minute)})

	// listener：pi_0001 原封不動到帳，post 兩條腿。
	add(ledger.Entry{ID: "pi_0001/post", Ref: ref("pi_0001", usdc, "100000000"), Kind: ledger.KindPost, Holds: "pi_0001/hold", Asset: assetUSDC,
		Legs: legs(100_000_000, 100_000_000), By: "listener", At: now.Add(5 * time.Minute), TxHash: "0xbb"})
	// pi_0002 只到帳 99.9：金額對不上，listener 送 needs_review，operator 判定已付、收尾。差的 0.1 是 USDT 抽的稅，得有第三條腿。
	add(ledger.Entry{ID: "pi_0002/post", Ref: ref("pi_0002", usdt, "100000000"), Kind: ledger.KindPost, Holds: "pi_0002/hold", Asset: assetUSDT,
		Legs: []ledger.Leg{
			{Account: ledger.PayerAccount(payer), Amount: big.NewInt(-100_000_000)},
			{Account: ledger.MerchantAccount(merchant), Amount: big.NewInt(99_900_000)},
			{Account: ledger.FeeAccount(usdt), Amount: big.NewInt(100_000)},
		}, By: "operator", At: now.Add(6 * time.Minute), TxHash: "0xcc"})
	// listener 把 pi_0001 的 post 又送了一次：同 ID 同內容，no-op。
	add(ledger.Entry{ID: "pi_0001/post", Ref: ref("pi_0001", usdc, "100000000"), Kind: ledger.KindPost, Holds: "pi_0001/hold", Asset: assetUSDC,
		Legs: legs(100_000_000, 100_000_000), By: "listener", At: now.Add(5 * time.Minute), TxHash: "0xbb"})
	// relayer：pi_0003 永遠失敗，放掉。
	add(ledger.Entry{ID: "pi_0003/void", Ref: ref("pi_0003", usdc, "50000000"), Kind: ledger.KindVoid, Holds: "pi_0003/hold", Asset: assetUSDC,
		By: "relayer", At: now.Add(7 * time.Minute), Memo: "blacklisted, nothing moved"})
	// 有人想把 pi_0003 再 post 一次：hold 已經被 void 收尾了，拒絕。
	add(ledger.Entry{ID: "pi_0003/post", Ref: ref("pi_0003", usdc, "50000000"), Kind: ledger.KindPost, Holds: "pi_0003/hold", Asset: assetUSDC,
		Legs: legs(50_000_000, 50_000_000), By: "operator", At: now.Add(8 * time.Minute), TxHash: "0xdd"})

	for _, q := range []struct {
		name  string
		acct  ledger.Account
		asset ledger.Asset
	}{
		{"merchant USDC", ledger.MerchantAccount(merchant), assetUSDC},
		{"merchant USDT", ledger.MerchantAccount(merchant), assetUSDT},
		{"payer    USDT", ledger.PayerAccount(payer), assetUSDT},
		{"fee      USDT", ledger.FeeAccount(usdt), assetUSDT},
	} {
		b, _ := j.Balance(ctx, q.acct, q.asset)
		fmt.Printf("balance %s  %s\n", q.name, b)
	}
	n := 0
	_ = j.Scan(ctx, func(ledger.Entry) error { n++; return nil })
	if err := ledger.Verify(ctx, j); err == nil {
		fmt.Printf("verify: ok (%d entries, chain intact)\n", n)
	}

	// Output:
	// #1  hold 0xb02f8d29…  by relayer   payer:0x7099…79C8 -100000000  merchant:0x3C44…93BC +100000000
	// #2  hold 0xecb961c3…  by relayer   payer:0x7099…79C8 -100000000  merchant:0x3C44…93BC +100000000
	// #3  hold 0x7d7c1422…  by relayer   payer:0x7099…79C8 -50000000  merchant:0x3C44…93BC +50000000
	// #4  post 0xb02f8d29…  by listener  payer:0x7099…79C8 -100000000  merchant:0x3C44…93BC +100000000  tx 0xbb
	// #5  post 0xecb961c3…  by operator  payer:0x7099…79C8 -100000000  merchant:0x3C44…93BC +99900000  fee:0xe7f1…0512 +100000  tx 0xcc
	// pi_0001/post     no-op (already there)
	// #6  void 0x7d7c1422…  by relayer   (blacklisted, nothing moved)
	// pi_0003/post     REJECTED: ledger: hold already resolved: pi_0003/hold
	// balance merchant USDC  pending 0  posted 100000000
	// balance merchant USDT  pending 0  posted 99900000
	// balance payer    USDT  pending 0  posted -100000000
	// balance fee      USDT  pending 0  posted 100000
	// verify: ok (6 entries, chain intact)
}

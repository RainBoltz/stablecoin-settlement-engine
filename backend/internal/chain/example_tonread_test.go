package chain_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/boc"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/listener"
)

// Example_readATONPayout 送一則裝了三筆付款的請求，然後從錢包那筆交易出發把三筆各追一遍：
//
//   - pi_0001 走完四步，merchant 的 jetton wallet 在 masterchain 103 加了餘額：settled。
//   - pi_0002 在 merchant 的 jetton wallet 失敗（exit 13），internal_transfer 退回來，我們的 jetton wallet
//     在 104 把餘額加回去：終點是那筆 on_bounce 交易，送審。
//   - pi_0003 的 internal_transfer 還在路上：錢包早就收下請求了，所以它不會變成 lost，只能等。
//
// 三筆掛在同一則 external message、同一個 seqno 上，錢包那筆交易在 101 就不可逆了；三筆的結局卻分別
// 落在 103、104 與「還不知道」。最後兩行是對帳那一半：同一段 window 掃 merchant 的 jetton wallet，
// 只有真的加了餘額的那一筆算數，pi_0001 的 ref 在鏈上出現了三次也只算一次。
func Example_readATONPayout() {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	intents := intent.NewMemoryStore()
	journal := ledger.NewMemoryJournal()

	// 三筆 intent 在 ton:mainnet 上，名單從它們長出來，ref 是 intent 算的那一把。
	items := tonRun(3)
	ids := []string{"pi_0001", "pi_0002", "pi_0003"}
	for i, id := range ids {
		in, _ := intent.New(intent.Spec{ID: id, Chain: "ton:mainnet", Token: "USDC", Payer: tonAccounts().Wallet.String(),
			Merchant: items[i].Merchant, Amount: items[i].Amount}, now)
		items[i].Ref = in.Ref
		_ = intents.Save(ctx, in, 0)
	}
	f, reader, external, err := playTON(items)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("send    seqno 41  %d payouts in one request  external %s  the wallet took it at masterchain %d\n",
		len(items), boc.Short(external), f.txs[0].Masterchain)

	// relayer 走完它那一段：settling、hold、confirming，intent 身上記的是 external message 的雜湊。
	for i, id := range ids {
		it, _ := intents.Get(ctx, id)
		_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: now})
		_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateSettling, By: intent.ActorRelayer, At: now})
		_, _, _ = journal.Append(ctx, ledger.Entry{ID: id + "/hold", Ref: it.Ref, Kind: ledger.KindHold,
			Asset: ledger.Asset{Chain: it.Chain, Token: it.Token},
			Legs: []ledger.Leg{{Account: ledger.PayerAccount(it.Payer), Amount: new(big.Int).Neg(items[i].Amount)},
				{Account: ledger.MerchantAccount(it.Merchant), Amount: new(big.Int).Set(items[i].Amount)}},
			By: "relayer", At: now})
		_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: hex.EncodeToString(external[:]), At: now})
	}

	for i, id := range ids {
		tr, err := reader.Trace(ctx, external, items[i].Ref)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Printf("trace   %s  %s\n", id, tr)
	}

	l := listener.New(intents, journal, reader, listener.WithClock(func() time.Time { return now.Add(time.Minute) }))
	for _, id := range ids {
		rep, err := l.Check(ctx, id)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Printf("check   %s  %-12s (%s)\n", id, rep.Outcome, rep.Detail)
	}

	transfers, err := reader.Transfers(ctx, 101, f.mc)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("window  masterchain 101..%d  %d credit at the merchants' jetton wallets\n", f.mc, len(transfers))
	for _, t := range transfers {
		it, _ := intents.GetByRef(ctx, t.Ref)
		fmt.Printf("credit  %s  %s  to %s  from %s  at %d\n", it.ID, t.Amount, shortTON(t.To), shortTON(t.From), t.Height)
	}
	fmt.Printf("balance our jetton wallet %s: 300000000 sent, 100000000 bounced back, 100000000 still on the road\n",
		f.balances[tonAccounts().JettonWallet])

	// Output:
	// send    seqno 41  3 payouts in one request  external 68c24be3…  the wallet took it at masterchain 101
	// trace   pi_0001  wallet 101 -> our jetton wallet 102 -> merchant's jetton wallet 103   delivered 100000000
	// trace   pi_0002  wallet 101 -> our jetton wallet 102 -> merchant's jetton wallet 103 aborted (exit 13) -> our jetton wallet 104   bounced
	// trace   pi_0003  wallet 101 -> our jetton wallet 102 -> merchant's jetton wallet …   in flight
	// check   pi_0001  settled      (masterchain at 103, 2 deep)
	// check   pi_0002  needs_review (masterchain at 104, 1 deep but the execution failed; gas burned, nothing moved)
	// check   pi_0003  wait         (included at 102, 3 deep, not yet masterchain)
	// window  masterchain 101..104  1 credit at the merchants' jetton wallets
	// credit  pi_0001  100000000  to 0:0000…0a0001  from 0:1111…111111  at 103
	// balance our jetton wallet 800000000: 300000000 sent, 100000000 bounced back, 100000000 still on the road
}

// shortTON 把 raw 寫法的地址縮成 0:0000…0a0001，跟 ledger 印科目的縮法同一個意思。
func shortTON(a string) string {
	if len(a) < 20 {
		return a
	}
	return a[:6] + "…" + a[len(a)-6:]
}

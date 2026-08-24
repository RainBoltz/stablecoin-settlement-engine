package listener_test

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/listener"
)

// Example_confirmThreeWays 是三筆停在 confirming 的付款、同一個 listener、三種結局：
//
//   - pi_0001 的交易被 finalized 而且轉帳金額對得上：帳上記 post、intent 推到 settled。
//   - pi_0002 的交易被 reorg 吐回來，五分鐘後還是不在任何區塊裡：intent 退回 settling，交給 relayer。
//   - pi_0003 的交易被 finalized、執行成功，但交易裡沒有帶我們 ref 的轉帳（幽靈支付）：送審。
//
// 第一段三筆都在等：進區塊只有 1 個區塊深，finalized tag 還沒追上來。第二段 head 走到 164，finalized 追上 100，
// 三筆才各自走各自的路。最後一行是同一筆 intent 被看第二次。
func Example_confirmThreeWays() {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	intents := intent.NewMemoryStore()
	journal := ledger.NewMemoryJournal()

	// relayer 已經走完它的那一段：settling、hold、廣播、confirming。這裡用手推，不需要 queue 與 worker。
	ids := []string{"pi_0001", "pi_0002", "pi_0003"}
	for _, id := range ids {
		it, _ := intent.New(intent.Spec{ID: id, Chain: "evm:31337",
			Token:    "0x5FbDB2315678afecb367f032d93F642f64180aa3", // devnet 的 USDC
			Payer:    "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
			Merchant: "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
			Amount:   big.NewInt(100_000_000)}, now)
		_ = intents.Save(ctx, it, 0)
		_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: now})
		it, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateSettling, By: intent.ActorRelayer, At: now})
		_, _, _ = journal.Append(ctx, ledger.Entry{ID: id + "/hold", Ref: it.Ref, Kind: ledger.KindHold,
			Asset: ledger.Asset{Chain: it.Chain, Token: it.Token},
			Legs: []ledger.Leg{{Account: ledger.PayerAccount(it.Payer), Amount: big.NewInt(-100_000_000)},
				{Account: ledger.MerchantAccount(it.Merchant), Amount: big.NewInt(100_000_000)}},
			By: "relayer", At: it.UpdatedAt})
		_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: "0x" + id[3:], At: now})
	}

	// 節點看到的世界：三筆都在區塊 100 裡。
	sightings := map[string]listener.Sighting{}
	for _, id := range ids {
		s := listener.Sighting{Received: big.NewInt(100_000_000)}
		s.Included, s.Height, s.Head, s.Succeeded = true, 100, 100, true
		sightings[id] = s
	}
	watcher := listener.WatcherFunc(func(_ context.Context, it *intent.Intent) (listener.Sighting, error) {
		return sightings[it.ID], nil
	})
	l := listener.New(intents, journal, watcher, listener.WithClock(func() time.Time { return now }))

	check := func(id string) {
		rep, _ := l.Check(ctx, id)
		fmt.Printf("check   %s\n", rep)
	}
	fmt.Println("watch   head 100  all three included at 100")
	for _, id := range ids {
		check(id)
	}

	// 五分鐘後：finalized 追上 100；pi_0002 被 reorg 吐回來、沒有再進區塊；pi_0003 的交易裡沒有我們的轉帳。
	now = now.Add(5 * time.Minute)
	for id, s := range sightings {
		s.Head, s.Final = 164, true
		sightings[id] = s
	}
	s2 := sightings["pi_0002"]
	s2.Included, s2.Final = false, false
	sightings["pi_0002"] = s2
	s3 := sightings["pi_0003"]
	s3.Received = nil
	sightings["pi_0003"] = s3

	fmt.Println("\nwatch   head 164  finalized caught up; 0x0002 reorged out; 0x0003 moved nothing")
	for _, id := range ids {
		check(id)
	}
	check("pi_0001") // 同一筆被看第二次

	fmt.Println()
	_ = journal.Scan(ctx, func(e ledger.Entry) error {
		if e.Kind == ledger.KindPost {
			fmt.Println(e)
		}
		return nil
	})
	for _, id := range ids {
		it, _ := intents.Get(ctx, id)
		fmt.Printf("%s %-12s v%d tx=%s\n", id, it.State, it.Version, it.TxHash)
	}
	b, _ := journal.Balance(ctx, ledger.MerchantAccount("0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"),
		ledger.Asset{Chain: "evm:31337", Token: "0x5FbDB2315678afecb367f032d93F642f64180aa3"})
	fmt.Printf("balance merchant USDC  %s\n", b)

	// Output:
	// watch   head 100  all three included at 100
	// check   pi_0001  wait         tx 0x0001 (included at 100, 1 deep, not yet finalized)
	// check   pi_0002  wait         tx 0x0002 (included at 100, 1 deep, not yet finalized)
	// check   pi_0003  wait         tx 0x0003 (included at 100, 1 deep, not yet finalized)
	//
	// watch   head 164  finalized caught up; 0x0002 reorged out; 0x0003 moved nothing
	// check   pi_0001  settled      tx 0x0001 (finalized at 100, 65 deep)
	// check   pi_0002  settling     tx 0x0002 (not in any block for 5m0s; dropped or reorged out)
	// check   pi_0003  needs_review tx 0x0003 (finalized at 100, 65 deep; no transfer carrying our ref, nothing moved)
	// check   pi_0001  no-op        tx 0x0001 (already settled)
	//
	// #4  post 0xb02f8d29…  by listener  payer:0x7099…79C8 -100000000  merchant:0x3C44…93BC +100000000  tx 0x0001
	// pi_0001 settled      v5 tx=0x0001
	// pi_0002 settling     v5 tx=
	// pi_0003 needs_review v5 tx=0x0003
	// balance merchant USDC  pending 200000000  posted 100000000
}

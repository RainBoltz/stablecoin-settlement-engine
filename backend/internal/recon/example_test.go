package recon_test

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/dlq"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/listener"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/recon"
)

const (
	usdc     = "0x5FbDB2315678afecb367f032d93F642f64180aa3" // devnet 的 USDC
	payer    = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	merchant = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
)

// fakeChain 是一條只有轉帳與一個 finalized 高度的鏈，同時扮演 recon.Source（掃一段高度）與 listener.Watcher
// （拿 tx hash 問一筆）。兩個介面看的是同一份資料，這正是真的 adapter 該有的樣子。
type fakeChain struct {
	transfers []recon.Transfer
	final     uint64
}

func (c *fakeChain) Finalized(context.Context) (uint64, error) { return c.final, nil }

func (c *fakeChain) Transfers(_ context.Context, from, to uint64) ([]recon.Transfer, error) {
	var out []recon.Transfer
	for _, t := range c.transfers {
		if t.Height >= from && t.Height <= to {
			out = append(out, t)
		}
	}
	return out, nil
}

func (c *fakeChain) Lookup(_ context.Context, it *intent.Intent) (listener.Sighting, error) {
	s := listener.Sighting{}
	s.Head = c.final
	for _, t := range c.transfers {
		if t.TxHash == it.TxHash {
			s.Included, s.Height, s.Final, s.Succeeded = true, t.Height, t.Height <= c.final, true
			if t.Ref == it.Ref {
				s.Received = new(big.Int).Set(t.Amount)
			}
		}
	}
	return s, nil
}

// Example_reconcileWindow 是一次對帳：五筆付款各停在不同的地方，鏈上有七筆轉帳，其中三筆對得上、四筆對不上。
//
//   - pi_0001 早就 settled，鏈上 0x0001 就是帳上那筆 post：對得上。
//   - pi_0002 停在 settling 沒有 tx hash（relayer 送出去之後、寫回 confirming 之前死掉），鏈上 0x0002 帶著它的 ref：
//     對帳引擎補上 hash、交給 listener，走完 settled。
//   - pi_0003 停在 authorized，queue 裡沒有它的 job：鏈下掃描替它丟一份。
//   - pi_0004 停在 confirming：鏈下掃描交給 listener，finalized 了就 settled；等一下對鏈上時它已經是一筆對得上的 post。
//   - pi_0005 被 relayer 宣告 failed、hold 也 void 了，鏈上 0x0005 卻帶著它的 ref 動了錢：unexpected。
//   - 0x0006 帶著一個沒有人認識的 ref、0x0007 打到 merchant 但沒帶 ref、0x0008 又帶了一次 pi_0001 的 ref。
//
// 第二次 Run 什麼都沒變：finalized 沒有往前走就沒有新的 window，鏈下掃描只剩 pi_0003 那份還在排隊的 job。
func Example_reconcileWindow() {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	intents := intent.NewMemoryStore()
	journal := ledger.NewMemoryJournal()
	jobs := queue.NewMemoryQueue()
	dead := dlq.NewMemoryStore()

	// 五筆付款，各推到自己停的那一格。hold 跟 relayer 記的一樣；pi_0001 的 post 跟 listener 記的一樣。
	amount := big.NewInt(100_000_000)
	refs := map[string]paymentref.Ref{}
	for _, id := range []string{"pi_0001", "pi_0002", "pi_0003", "pi_0004", "pi_0005"} {
		it, _ := intent.New(intent.Spec{ID: id, Chain: "evm:31337", Token: usdc, Payer: payer, Merchant: merchant, Amount: amount}, now)
		_ = intents.Save(ctx, it, 0)
		refs[id] = it.Ref
		_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: now})
		if id == "pi_0003" {
			continue
		}
		it, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateSettling, By: intent.ActorRelayer, At: now})
		_, _, _ = journal.Append(ctx, ledger.Entry{ID: id + "/hold", Ref: it.Ref, Kind: ledger.KindHold,
			Asset: ledger.Asset{Chain: it.Chain, Token: it.Token},
			Legs: []ledger.Leg{{Account: ledger.PayerAccount(payer), Amount: new(big.Int).Neg(amount)},
				{Account: ledger.MerchantAccount(merchant), Amount: amount}},
			By: "relayer", At: it.UpdatedAt})
		switch id {
		case "pi_0001", "pi_0004":
			it, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: "0x" + id[3:], At: now})
		case "pi_0002":
			_, _ = jobs.Enqueue(ctx, queue.Job{ID: id + "/settle", Kind: queue.KindSettle, IntentID: id, Ref: it.Ref}, now)
		case "pi_0005":
			_, _, _ = journal.Append(ctx, ledger.Entry{ID: id + "/void", Ref: it.Ref, Kind: ledger.KindVoid, Holds: id + "/hold",
				Asset: ledger.Asset{Chain: it.Chain, Token: it.Token}, By: "relayer", At: it.UpdatedAt, Memo: "not sent"})
			_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateFailed, By: intent.ActorRelayer, Reason: "not sent", At: now})
		}
		if id == "pi_0001" {
			_, _, _ = journal.Append(ctx, ledger.Entry{ID: id + "/post", Ref: it.Ref, Kind: ledger.KindPost, Holds: id + "/hold",
				Asset: ledger.Asset{Chain: it.Chain, Token: it.Token},
				Legs: []ledger.Leg{{Account: ledger.PayerAccount(payer), Amount: new(big.Int).Neg(amount)},
					{Account: ledger.MerchantAccount(merchant), Amount: amount}},
				By: "listener", At: it.UpdatedAt, TxHash: "0x0001"})
			_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateSettled, By: intent.ActorListener, TxHash: "0x0001", At: now})
		}
	}

	// 鏈上的世界：七筆轉帳，finalized 已經走到 164。
	transfer := func(tx string, height uint64, ref paymentref.Ref) recon.Transfer {
		return recon.Transfer{TxHash: tx, Height: height, Ref: ref, Token: usdc, From: payer, To: merchant, Amount: amount}
	}
	stranger := paymentref.Derive(paymentref.Terms{IntentID: "pi_9999", Chain: "evm:31337", Token: usdc, Payer: payer, Merchant: merchant, Amount: "100000000"})
	chain := &fakeChain{final: 164, transfers: []recon.Transfer{
		transfer("0x0001", 90, refs["pi_0001"]),
		transfer("0x0002", 100, refs["pi_0002"]),
		transfer("0x0004", 100, refs["pi_0004"]),
		transfer("0x0005", 101, refs["pi_0005"]),
		transfer("0x0006", 102, stranger),
		transfer("0x0007", 103, paymentref.Ref{}),
		transfer("0x0008", 120, refs["pi_0001"]),
	}}

	now = now.Add(20 * time.Minute)
	l := listener.New(intents, journal, chain, listener.WithClock(func() time.Time { return now }),
		listener.WithPolicy("evm", finality.Defaults()["evm"]))
	engine := recon.New("evm:31337", recon.Deps{Intents: intents, Journal: journal, Jobs: jobs, Dead: dead, Listener: l, Source: chain},
		recon.WithClock(func() time.Time { return now }))

	run := func() {
		before := engine.Cursor()
		rep, err := engine.Run(ctx)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		for _, s := range rep.Sweeps {
			fmt.Printf("sweep   %s\n", s)
		}
		if rep.To < rep.From {
			fmt.Printf("window  nothing new: finalized still at %d\n", rep.To)
			return
		}
		fmt.Printf("window  blocks %d..%d finalized  %d matched, %d findings\n\n", rep.From, rep.To, len(rep.Matches), len(rep.Findings))
		for _, m := range rep.Matches {
			fmt.Printf("match   %s\n", m)
		}
		for _, f := range rep.Findings {
			fmt.Printf("finding %s\n", f)
		}
		fmt.Printf("\ncursor  %d -> %d\n", before, engine.Cursor())
	}
	run()
	fmt.Println()
	run()

	fmt.Println()
	for _, id := range []string{"pi_0001", "pi_0002", "pi_0003", "pi_0004", "pi_0005"} {
		it, _ := intents.Get(ctx, id)
		fmt.Printf("%s %-12s v%d tx=%s\n", id, it.State, it.Version, it.TxHash)
	}
	b, _ := journal.Balance(ctx, ledger.MerchantAccount(merchant), ledger.Asset{Chain: "evm:31337", Token: usdc})
	fmt.Printf("balance merchant USDC  %s\n", b)

	// Output:
	// sweep   pi_0002  settling     already queued
	// sweep   pi_0003  authorized   enqueued pi_0003/settle
	// sweep   pi_0004  confirming   settled (finalized at 100, 65 deep)
	// window  blocks 1..164 finalized  3 matched, 4 findings
	//
	// match   0x0001 pi_0001  settled, post matches the chain
	// match   0x0002 pi_0002  settling -> confirming -> settled (finalized at 100, 65 deep)
	// match   0x0004 pi_0004  settled, post matches the chain
	// finding unexpected   tx 0x0005 ref 0x3d54643e… (pi_0005) is failed, yet the money moved
	// finding unknown_ref  tx 0x0006 ref 0x908f939d… matches no intent
	// finding unreferenced tx 0x0007 100000000 to 0x3C44…93BC without a ref
	// finding paid_twice   tx 0x0008 ref 0xb02f8d29… (pi_0001) already settled on tx 0x0001
	//
	// cursor  0 -> 164
	//
	// sweep   pi_0003  authorized   already queued
	// window  nothing new: finalized still at 164
	//
	// pi_0001 settled      v5 tx=0x0001
	// pi_0002 settled      v5 tx=0x0002
	// pi_0003 authorized   v2 tx=
	// pi_0004 settled      v5 tx=0x0004
	// pi_0005 failed       v4 tx=
	// balance merchant USDC  pending 0  posted 300000000
}

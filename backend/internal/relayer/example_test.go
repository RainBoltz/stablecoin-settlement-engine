package relayer_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/relayer"
)

// Example_settleThroughQueue 是 relayer 這一層的長相：三筆 authorized 的 intent 排進 queue，一個 worker 一份一份領。
//   - pi_0001 正常走完：settling、hold、送出、confirming。之後同一份 job 又被排了一次（上游重送），no-op。
//   - pi_0002 被 worker A 領走之後 A 就死了；lease 過期後 worker B 領到同一份（attempt 2），照常走完。
//   - pi_0003 送出時 RPC 掛了：retry；RPC 好了也不重送，因為 relayer 不知道上一筆有沒有出門；
//     卡在 settling 超過五分鐘，推到 needs_review 讓人看。
//
// 最後印 journal：三筆 hold，pending 都在、沒有一筆 post，因為今天還沒有 listener。
func Example_settleThroughQueue() {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	q := queue.NewMemoryQueue()
	intents := intent.NewMemoryStore()
	journal := ledger.NewMemoryJournal()

	rpcDown := false
	txSeq := 0
	sender := relayer.SenderFunc(func(_ context.Context, it *intent.Intent) (string, error) {
		if rpcDown {
			return "", errors.New("rpc: connection refused")
		}
		txSeq++
		return fmt.Sprintf("0x%02x", 0xa9+txSeq), nil // 0xaa、0xab、…
	})
	w := relayer.New(q, intents, journal, sender, relayer.WithClock(clock))
	cfg := relayer.DefaultConfig()

	// API 這一側：建 intent、（簽名驗過）推到 authorized、丟一份 job。今天先用 Advance 代替簽名迴圈。
	authorize := func(id string) *intent.Intent {
		it, _ := intent.New(intent.Spec{ID: id, Chain: "evm:31337",
			Token:    "0x5FbDB2315678afecb367f032d93F642f64180aa3", // devnet 的 USDC
			Payer:    "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
			Merchant: "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
			Amount:   big.NewInt(100_000_000)}, now)
		_ = intents.Save(ctx, it, 0)
		it, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: now})
		return it
	}
	enqueue := func(it *intent.Intent) {
		job := queue.Job{ID: it.ID + "/settle", Kind: queue.KindSettle, IntentID: it.ID, Ref: it.Ref}
		if applied, _ := q.Enqueue(ctx, job, now); applied {
			fmt.Printf("enqueue %s  queued\n", job.ID)
		} else {
			fmt.Printf("enqueue %s  no-op (already queued)\n", job.ID)
		}
	}
	run := func() {
		rep, ok, _ := w.RunOnce(ctx)
		if ok {
			fmt.Println("worker  " + rep.String())
		}
	}

	pi1, pi2, pi3 := authorize("pi_0001"), authorize("pi_0002"), authorize("pi_0003")
	enqueue(pi1)
	enqueue(pi1)
	enqueue(pi2)
	enqueue(pi3)

	run() // pi_0001：正常走完
	// pi_0002：worker A 領走就死了（直接對 queue 領一份、什麼都不做），lease 過期後才輪到 worker B。
	q.Lease(ctx, now, cfg.Lease)
	rpcDown = true
	run() // pi_0003：送出失敗，retry
	rpcDown = false
	now = now.Add(cfg.Lease)
	run() // pi_0002：attempt 2，照常走完
	run() // pi_0003：attempt 2，RPC 好了，但 settling 才 30 秒，等
	now = now.Add(cfg.StuckAfter)
	run() // pi_0003：attempt 3，卡太久，送審
	enqueue(pi1)
	run() // pi_0001 的 job 又來一次：已經 confirming，no-op

	n, _ := q.Len(ctx)
	fmt.Printf("queue: %d job(s) left\n", n)
	_ = journal.Scan(ctx, func(e ledger.Entry) error { fmt.Println(e); return nil })
	for _, id := range []string{"pi_0001", "pi_0002", "pi_0003"} {
		it, _ := intents.Get(ctx, id)
		fmt.Printf("%s %-12s v%d tx=%s\n", id, it.State, it.Version, it.TxHash)
	}

	// Output:
	// enqueue pi_0001/settle  queued
	// enqueue pi_0001/settle  no-op (already queued)
	// enqueue pi_0002/settle  queued
	// enqueue pi_0003/settle  queued
	// worker  pi_0001/settle   #1  sent         tx 0xaa
	// worker  pi_0003/settle   #1  retry        (send: rpc: connection refused)
	// worker  pi_0002/settle   #2  sent         tx 0xab
	// worker  pi_0003/settle   #2  retry        (settling for 30s without tx hash, waiting)
	// worker  pi_0003/settle   #3  needs_review (settling for 5m30s without tx hash; broadcast outcome unknown)
	// enqueue pi_0001/settle  queued
	// worker  pi_0001/settle   #1  no-op        (already confirming)
	// queue: 0 job(s) left
	// #1  hold 0xb02f8d29…  by relayer   payer:0x7099…79C8 -100000000  merchant:0x3C44…93BC +100000000
	// #2  hold 0x27ec4d4d…  by relayer   payer:0x7099…79C8 -100000000  merchant:0x3C44…93BC +100000000
	// #3  hold 0xfce5e296…  by relayer   payer:0x7099…79C8 -100000000  merchant:0x3C44…93BC +100000000
	// pi_0001 confirming   v4 tx=0xaa
	// pi_0002 confirming   v4 tx=0xab
	// pi_0003 needs_review v4 tx=
}

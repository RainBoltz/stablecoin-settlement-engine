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
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfail"
)

// Example_poisonJob 是三種失敗各自停在哪裡：同樣是「這份 job 不再交付」，那筆付款的下場差很多。
//
//   - pi_0001：Sender 宣告了兩件事——這筆確定沒發送出去，而且重試不會好。錢確定沒動，所以第一次交付就收工：
//     hold 放掉、intent 走 settling -> failed。
//   - pi_0002：只是一直逾時，沒有人宣告任何事。退避到預算用完為止，然後推 needs_review——
//     那筆交易可能已經在鏈上，relayer 不敢說它失敗了。
//   - pi_0003：從頭到尾還沒 authorized。relayer 一個 byte 都沒寫過，這一格也不是它的地盤，
//     所以丟掉的只是那張便條，intent 原封不動。
//
// Example 把預算縮到 3 次、關掉抖動，輸出才短又固定；預設是 10 次、加抖動（見 txfail.DefaultPolicy）。
func Example_poisonJob() {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := queue.NewMemoryQueue()
	intents := intent.NewMemoryStore()
	journal := ledger.NewMemoryJournal()

	sender := relayer.SenderFunc(func(_ context.Context, it *intent.Intent) (string, error) {
		if it.ID == "pi_0001" {
			return "", fmt.Errorf("%w: %w: evm:31337 has no signer configured", relayer.ErrNotSent, txfail.ErrPoison)
		}
		return "", errors.New("rpc: connection refused")
	})

	p := txfail.DefaultPolicy()
	p.MaxAttempts, p.Jitter = 3, nil
	base := relayer.DefaultConfig().RetryAfter
	w := relayer.New(q, intents, journal, sender,
		relayer.WithClock(func() time.Time { return now }), relayer.WithFaultPolicy(p))
	fmt.Printf("policy  at most %d deliveries; back off %s doubling, capped at %s\n\n",
		p.MaxAttempts, base, p.MaxBackoff)

	ids := []string{"pi_0001", "pi_0002", "pi_0003"}
	for _, id := range ids {
		it, _ := intent.New(intent.Spec{ID: id, Chain: "evm:31337",
			Token:    "0x5FbDB2315678afecb367f032d93F642f64180aa3", // devnet 的 USDC
			Payer:    "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
			Merchant: "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
			Amount:   big.NewInt(100_000_000)}, now)
		_ = intents.Save(ctx, it, 0)
		if id != "pi_0003" { // pi_0003 的付款人一直沒簽
			it, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: now})
		}
		// 一次只放一份 job 進去，三組輸出才不會交錯在一起。
		_, _ = q.Enqueue(ctx, queue.Job{ID: id + "/settle", Kind: queue.KindSettle, IntentID: id, Ref: it.Ref}, now)
		for attempt := uint64(1); ; attempt++ {
			rep, ok, _ := w.RunOnce(ctx)
			if !ok {
				break
			}
			fmt.Printf("worker  %s\n", rep)
			if rep.Outcome != relayer.OutcomeRetry {
				break
			}
			now = now.Add(p.Backoff(base, attempt)) // 撥到下一次交付看得見
		}
	}

	fmt.Println()
	for _, id := range ids {
		it, _ := intents.Get(ctx, id)
		hold := "-"
		if _, err := journal.Get(ctx, id+"/hold"); err == nil {
			hold = "pending"
		}
		if _, err := journal.Get(ctx, id+"/void"); err == nil {
			hold = "voided"
		}
		fmt.Printf("%s %-12s hold %s\n", id, it.State, hold)
	}

	// Output:
	// policy  at most 3 deliveries; back off 5s doubling, capped at 2m0s
	//
	// worker  pi_0001/settle   #1  poison       (retrying will not help; nothing was sent, so the intent failed)
	// worker  pi_0002/settle   #1  retry        (send: rpc: connection refused)
	// worker  pi_0002/settle   #2  retry        (settling for 5s without tx hash, waiting)
	// worker  pi_0002/settle   #3  poison       (no luck after 3 deliveries; last broadcast unknown, needs review)
	// worker  pi_0003/settle   #1  retry        (not authorized yet)
	// worker  pi_0003/settle   #2  retry        (not authorized yet)
	// worker  pi_0003/settle   #3  poison       (no luck after 3 deliveries; job dropped, intent still created)
	//
	// pi_0001 failed       hold voided
	// pi_0002 needs_review hold pending
	// pi_0003 created      hold -
}

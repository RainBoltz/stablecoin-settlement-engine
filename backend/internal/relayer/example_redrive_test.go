package relayer_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/dlq"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/relayer"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfail"
)

// Example_redriveJob 是同一個動作、三種結果：三份被放棄的 job 停進收容所，人把它們原封不動放回 queue，
// 而決定接下來會發生什麼的不是收容所，是那筆 intent 現在停在哪一格。
//
//   - pi_0001 的 intent 已經 failed，hold 也放掉了。放回去只換到一次 no-op，帳本一列都不會多。
//   - pi_0002 停在 needs_review。那一格的出口只有 operator 走得動，便條放回去照樣是 no-op。
//   - pi_0003 從頭到尾還沒 authorized，這是 redrive 真正救得回來的那一種：付款人補簽之後放回去就走完了。
//
// 最後一行是同一份紀錄被按第二次：Resolve 是 CAS，晚的那個人會知道自己晚了。
// Example 把 max attempts 縮到 3 次、關掉 jitter，第一段輸出才短又固定（見 txfail.DefaultPolicy）。
func Example_redriveJob() {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := queue.NewMemoryQueue()
	intents := intent.NewMemoryStore()
	journal := ledger.NewMemoryJournal()
	dead := dlq.NewMemoryStore()

	fixed := false // 第二段才把節點修好
	sender := relayer.SenderFunc(func(_ context.Context, it *intent.Intent) (string, error) {
		switch {
		case it.ID == "pi_0001":
			return "", fmt.Errorf("%w: %w: evm:31337 has no signer configured", relayer.ErrNotSent, txfail.ErrPoison)
		case fixed:
			return "0x" + it.ID[3:], nil
		}
		return "", errors.New("rpc: connection refused")
	})

	p := txfail.DefaultPolicy()
	p.MaxAttempts, p.Jitter = 3, nil
	base := relayer.DefaultConfig().RetryAfter
	w := relayer.New(q, intents, journal, sender,
		relayer.WithClock(func() time.Time { return now }),
		relayer.WithFaultPolicy(p), relayer.WithDeadLetters(dead))

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
		// 一次只放一份 job 進去，三組結果才不會交錯在一起。
		_, _ = q.Enqueue(ctx, queue.Job{ID: id + "/settle", Kind: queue.KindSettle, IntentID: id, Ref: it.Ref}, now)
		for attempt := uint64(1); ; attempt++ {
			rep, ok, _ := w.RunOnce(ctx)
			if !ok || rep.Outcome != relayer.OutcomeRetry {
				break
			}
			now = now.Add(p.Backoff(base, attempt)) // 撥到下一次交付看得見
		}
	}

	waiting, _ := dead.List(ctx, dlq.StatusParked)
	fmt.Printf("dlq     %d parked\n", len(waiting))
	for _, r := range waiting {
		fmt.Printf("        %s\n", r)
	}

	// 付款人終於簽了名，那筆 intent 又需要 relayer 動它了。
	fixed = true
	_, _, _ = intent.Advance(ctx, intents, "pi_0003", intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: now})
	fmt.Printf("\nsign    pi_0003 signed at last, back to authorized\n\n")

	redrive := func(jobID string) {
		if _, err := dlq.Redrive(ctx, dead, q, jobID, "ops", now); err != nil {
			fmt.Printf("redrive %-16s refused (%v)\n", jobID, err)
			return
		}
		fmt.Printf("redrive %-16s by ops\n", jobID)
		if rep, ok, _ := w.RunOnce(ctx); ok {
			fmt.Printf("worker  %s\n", rep)
		}
	}
	for _, id := range ids {
		redrive(id + "/settle")
	}
	redrive("pi_0003/settle") // 同一份紀錄按第二次

	left, _ := dead.List(ctx, dlq.StatusParked)
	back, _ := dead.List(ctx, dlq.StatusRedriven)
	fmt.Printf("\ndlq     %d parked, %d redriven\n", len(left), len(back))

	// Output:
	// dlq     3 parked
	//         pi_0001/settle   #1  parked   failed       -    retrying will not help; nothing was sent, so the intent failed
	//         pi_0002/settle   #3  parked   needs_review -    no luck after 3 deliveries; last broadcast unknown, needs review
	//         pi_0003/settle   #3  parked   created      -    no luck after 3 deliveries; job dropped, intent still created
	//
	// sign    pi_0003 signed at last, back to authorized
	//
	// redrive pi_0001/settle   by ops
	// worker  pi_0001/settle   #1  no-op        (already failed)
	// redrive pi_0002/settle   by ops
	// worker  pi_0002/settle   #1  no-op        (already needs_review)
	// redrive pi_0003/settle   by ops
	// worker  pi_0003/settle   #1  sent         tx 0x0003
	// redrive pi_0003/settle   refused (dlq: record is not parked: pi_0003/settle is redriven)
	//
	// dlq     0 parked, 3 redriven
}

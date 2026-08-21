package queue_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
)

// Example_leaseAckNack 是 at-least-once 的長相：同一份 job 排兩次只算一次；領走之後 30 秒內別人看不到；
// 領走的 worker 死了，30 秒後 job 再度可見、attempt 變 2；死掉那個 worker 醒來想 Ack，憑證已經作廢。
func Example_leaseAckNack() {
	ctx := context.Background()
	q := queue.NewMemoryQueue()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ref := paymentref.Derive(paymentref.Terms{IntentID: "pi_0001", Chain: "evm:31337",
		Token: "0x5FbDB2315678afecb367f032d93F642f64180aa3", Payer: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
		Merchant: "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC", Amount: "100000000"})
	job := queue.Job{ID: "pi_0001/settle", Kind: queue.KindSettle, IntentID: "pi_0001", Ref: ref}

	enqueue := func(at time.Duration) {
		applied, _ := q.Enqueue(ctx, job, now.Add(at))
		if applied {
			fmt.Printf("t+%-4s enqueue %s  queued\n", at, job.ID)
		} else {
			fmt.Printf("t+%-4s enqueue %s  no-op (already queued)\n", at, job.ID)
		}
	}
	lease := func(at time.Duration) (queue.Delivery, bool) {
		d, ok, _ := q.Lease(ctx, now.Add(at), 30*time.Second)
		if ok {
			fmt.Printf("t+%-4s lease   %s  attempt %d, lease until t+%s\n", at, d.Job.ID, d.Attempt, d.LeaseUntil.Sub(now))
		} else {
			fmt.Printf("t+%-4s lease   nothing visible\n", at)
		}
		return d, ok
	}

	enqueue(0)
	enqueue(1 * time.Second)
	dead, _ := lease(2 * time.Second) // 這個 worker 領走之後就死了
	lease(10 * time.Second)           // 別人看不到它
	alive, _ := lease(32 * time.Second)
	if err := q.Ack(ctx, dead); errors.Is(err, queue.ErrStaleReceipt) {
		fmt.Printf("t+%-4s ack     %s  by the dead worker: REJECTED (stale receipt)\n", 33*time.Second, dead.Job.ID)
	}
	if err := q.Ack(ctx, alive); err == nil {
		fmt.Printf("t+%-4s ack     %s  by attempt %d: ok\n", 34*time.Second, alive.Job.ID, alive.Attempt)
	}
	n, _ := q.Len(ctx)
	fmt.Printf("queue: %d job(s) left\n", n)

	// Output:
	// t+0s   enqueue pi_0001/settle  queued
	// t+1s   enqueue pi_0001/settle  no-op (already queued)
	// t+2s   lease   pi_0001/settle  attempt 1, lease until t+32s
	// t+10s  lease   nothing visible
	// t+32s  lease   pi_0001/settle  attempt 2, lease until t+1m2s
	// t+33s  ack     pi_0001/settle  by the dead worker: REJECTED (stale receipt)
	// t+34s  ack     pi_0001/settle  by attempt 2: ok
	// queue: 0 job(s) left
}

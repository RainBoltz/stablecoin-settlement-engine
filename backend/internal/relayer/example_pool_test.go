package relayer_test

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/relayer"
)

// Example_poolDrain 是收工的長相：六份 job、兩個 worker，兩筆交易正在送的時候收到停止訊號。
// pool 不再領新的 job，但等那兩筆送完、Ack 之後才回來；剩下四份留在 queue 裡，沒有人碰過。
func Example_poolDrain() {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := queue.NewMemoryQueue()
	intents := intent.NewMemoryStore()
	journal := ledger.NewMemoryJournal()

	// Sender 會卡住，直到我們說可以：這樣才看得到「在手上的 job」是怎麼被等完的。tx hash 從 intent id 推，輸出才可預測。
	entered := make(chan string, 6)
	release := make(chan struct{})
	sender := relayer.SenderFunc(func(ctx context.Context, it *intent.Intent) (string, error) {
		entered <- it.ID
		select {
		case <-release:
			return "0x" + it.ID[3:], nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	w := relayer.New(q, intents, journal, sender,
		relayer.WithClock(func() time.Time { return now }),
		relayer.WithLimiter(relayer.NewThrottle(2, 0, 0)))
	pool := relayer.NewPool(w, relayer.PoolConfig{Size: 2, Idle: time.Millisecond, DrainTimeout: 5 * time.Second})

	ids := []string{"pi_0001", "pi_0002", "pi_0003", "pi_0004", "pi_0005", "pi_0006"}
	for _, id := range ids {
		it, _ := intent.New(intent.Spec{ID: id, Chain: "evm:31337",
			Token:    "0x5FbDB2315678afecb367f032d93F642f64180aa3", // devnet 的 USDC
			Payer:    "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
			Merchant: "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
			Amount:   big.NewInt(100_000_000)}, now)
		_ = intents.Save(ctx, it, 0)
		it, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: now})
		_, _ = q.Enqueue(ctx, queue.Job{ID: id + "/settle", Kind: queue.KindSettle, IntentID: id, Ref: it.Ref}, now)
	}
	fmt.Printf("enqueue %d job(s); pool of 2 workers, 2 sends in flight at most\n", len(ids))

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan relayer.Stats, 1)
	go func() { done <- pool.Run(runCtx) }()

	busy := []string{<-entered, <-entered}
	sort.Strings(busy)
	fmt.Printf("send    %s in flight\n", busy)
	stop()
	fmt.Println("stop    requested: stop leasing, let the in-flight sends finish")
	close(release)
	st := <-done
	fmt.Printf("pool    stopped: sent %d, retry %d, abandoned %d, panics %d\n", st.Sent, st.Retry, st.Abandoned, st.Panics)

	left, _ := q.Len(ctx)
	_, leased, _ := q.Lease(ctx, now, time.Second) // 還領得到，代表剩下的從來沒被領走過
	fmt.Printf("queue   %d job(s) left, next lease ok=%v\n", left, leased)
	for _, id := range ids {
		it, _ := intents.Get(ctx, id)
		fmt.Printf("%s %-12s v%d tx=%s\n", id, it.State, it.Version, it.TxHash)
	}

	// Output:
	// enqueue 6 job(s); pool of 2 workers, 2 sends in flight at most
	// send    [pi_0001 pi_0002] in flight
	// stop    requested: stop leasing, let the in-flight sends finish
	// pool    stopped: sent 2, retry 0, abandoned 0, panics 0
	// queue   4 job(s) left, next lease ok=true
	// pi_0001 confirming   v4 tx=0x0001
	// pi_0002 confirming   v4 tx=0x0002
	// pi_0003 authorized   v2 tx=
	// pi_0004 authorized   v2 tx=
	// pi_0005 authorized   v2 tx=
	// pi_0006 authorized   v2 tx=
}

// Example_throttle 是限流的長相：每秒 2 筆、burst 1。第一筆不用等，之後每一筆都照缺口等 500ms。
// 時鐘是假的：sleep 不真的睡，直接把時鐘撥過去。
func Example_throttle() {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	var waited time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		waited = d
		return nil
	}
	th := relayer.NewThrottle(1, 2, 1, relayer.WithThrottleClock(func() time.Time { return now }, sleep))
	for i := 1; i <= 4; i++ {
		at := now.Sub(start)
		waited = 0
		_ = th.Acquire(context.Background())
		th.Release()
		if waited == 0 {
			fmt.Printf("acquire #%d  t+%-6s  no wait\n", i, at)
		} else {
			fmt.Printf("acquire #%d  t+%-6s  waited %s\n", i, at, waited)
		}
	}

	// Output:
	// acquire #1  t+0s      no wait
	// acquire #2  t+0s      waited 500ms
	// acquire #3  t+500ms   waited 500ms
	// acquire #4  t+1s      waited 500ms
}

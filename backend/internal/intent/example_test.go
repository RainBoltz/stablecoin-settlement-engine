package intent_test

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
)

// Example_lifecycle 走一遍最常見的一生：付款人簽名、relayer 送上鏈、進區塊、reorg 吐回來、
// 重送、確認、最後 settled。中間穿插兩個會被拒絕的請求（API 想直接宣告 settled、queue 重送同一個 job），
// 印出來的就是 History。
func Example_lifecycle() {
	ctx := context.Background()
	store := intent.NewMemoryStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	it, _ := intent.New(intent.Spec{
		ID:        "pi_0001",
		Chain:     "evm:31337",
		Token:     "0x5FbDB2315678afecb367f032d93F642f64180aa3", // devnet 的 USDC
		Payer:     "0x70997970C51812dc3A010C7d01b50e0d17dc79C8", // payer
		Merchant:  "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC", // merchant
		Amount:    big.NewInt(100_000_000),                      // 100 USDC
		ExpiresAt: now.Add(15 * time.Minute),
	}, now)
	_ = store.Save(ctx, it, 0)

	step := func(req intent.Request) {
		got, applied, err := intent.Advance(ctx, store, it.ID, req)
		switch {
		case err != nil:
			fmt.Printf("%-12s -> %-12s by %-8s  REJECTED: %v\n", it.State, req.To, req.By, err)
		case !applied:
			fmt.Printf("%-12s -> %-12s by %-8s  no-op (already there)\n", it.State, req.To, req.By)
		default:
			it = got
			fmt.Println(it.History[len(it.History)-1])
		}
	}

	step(intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: now.Add(1 * time.Minute)})
	step(intent.Request{To: intent.StateSettled, By: intent.ActorAPI, TxHash: "0xaa", At: now.Add(1 * time.Minute)})
	step(intent.Request{To: intent.StateSettling, By: intent.ActorRelayer, At: now.Add(2 * time.Minute)})
	step(intent.Request{To: intent.StateSettling, By: intent.ActorRelayer, At: now.Add(2 * time.Minute)})
	step(intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: "0xaa", At: now.Add(3 * time.Minute)})
	step(intent.Request{To: intent.StateSettling, By: intent.ActorListener, Reason: "reorg at block 12", At: now.Add(4 * time.Minute)})
	step(intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: "0xbb", At: now.Add(5 * time.Minute)})
	step(intent.Request{To: intent.StateSettled, By: intent.ActorListener, TxHash: "0xbb", At: now.Add(6 * time.Minute)})
	step(intent.Request{To: intent.StateFailed, By: intent.ActorOperator, Reason: "changed my mind", At: now.Add(7 * time.Minute)})

	fmt.Printf("final: %s v%d tx=%s\n", it.State, it.Version, it.TxHash)

	// Output:
	// created      -> authorized   by api
	// authorized   -> settled      by api       REJECTED: intent: transition not in table: authorized -> settled
	// authorized   -> settling     by relayer
	// settling     -> settling     by relayer   no-op (already there)
	// settling     -> confirming   by relayer   tx 0xaa
	// confirming   -> settling     by listener  (reorg at block 12)
	// settling     -> confirming   by relayer   tx 0xbb
	// confirming   -> settled      by listener  tx 0xbb
	// settled      -> failed       by operator  REJECTED: intent: state is terminal: pi_0001 is settled
	// final: settled v7 tx=0xbb
}

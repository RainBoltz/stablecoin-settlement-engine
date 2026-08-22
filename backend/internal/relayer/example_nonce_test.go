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
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// evmSender 是一個假的 EVM chain adapter：它需要 nonce，所以實作 relayer.OrderedSender。
// 所有付款都從同一個錢包出去（devnet 的 relayer，Anvil 助記詞 index 3），這正是要排隊的原因。
type evmSender struct {
	account string
	err     error  // 非 nil 時 SendAt 一律失敗
	slot    string // 這一輪拿到的號，Example 印出來用
}

// Account 實作 OrderedSender。
func (s *evmSender) Account(*intent.Intent) string { return s.account }

// Send 實作 Sender。需要 nonce 的鏈組不出沒有序號的交易，worker 也不會呼叫它。
func (s *evmSender) Send(context.Context, *intent.Intent) (string, error) {
	return "", fmt.Errorf("%w: evm transactions need a nonce", relayer.ErrNotSent)
}

// SendAt 實作 OrderedSender：把號記下來，然後回一個從 intent id 推出來的 tx hash（輸出才可預測）。
func (s *evmSender) SendAt(_ context.Context, it *intent.Intent, res txseq.Reservation) (string, error) {
	s.slot = fmt.Sprintf("%d", res.Value)
	if s.err != nil {
		return "", s.err
	}
	return "0x" + it.ID[3:], nil
}

// Example_nonceGap 是序號在 relayer 裡的長相：一個發送錢包、五筆付款，中間有一筆確定沒出門、一筆不知道有沒有出門。
//   - pi_0001 正常送出，用掉 nonce 7。
//   - pi_0002 簽名失敗（包了 ErrNotSent），確定沒出門，8 退回去。
//   - pi_0003 拿到退回去的 8，正常送出。
//   - pi_0004 送出時 RPC 逾時，不知道有沒有出門：9 當成用掉，序列上留一個洞。
//   - pi_0005 因此拿不到號，原封不動回 queue，intent 還在 authorized、帳上沒有 hold。
//
// 洞消失的方式是跟鏈上對齊：eth_getTransactionCount 回 10，代表 9 其實上鏈了。之後剩下的 job 照常繼續，
// 只有 pi_0004 自己還卡在 settling（沒有 tx hash 可看，照 Worker.process 的規則等到 StuckAfter 才送審）。
func Example_nonceGap() {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := queue.NewMemoryQueue()
	intents := intent.NewMemoryStore()
	journal := ledger.NewMemoryJournal()

	sender := &evmSender{account: "0x90F79bf6EB2c4f870365E785982E1f101E93b906"}
	seq := txseq.NewCounter()
	w := relayer.New(q, intents, journal, sender,
		relayer.WithClock(func() time.Time { return now }), relayer.WithSequencer(seq))

	// 接真的鏈時第一件事：問鏈上這個錢包用到哪了。這裡假裝它回 7。
	_ = seq.Sync(ctx, sender.account, 7)
	fmt.Printf("sync    %s\n", seq.Status(sender.account))

	ids := []string{"pi_0001", "pi_0002", "pi_0003", "pi_0004", "pi_0005"}
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

	run := func(sendErr error) {
		sender.err, sender.slot = sendErr, "-"
		rep, ok, _ := w.RunOnce(ctx)
		if ok {
			fmt.Printf("worker  nonce %-4s %s\n", sender.slot, rep)
		}
	}

	run(nil)
	run(fmt.Errorf("%w: signing failed", relayer.ErrNotSent)) // 確定沒出門
	run(nil)
	run(errors.New("rpc: timeout")) // 不知道
	run(nil)                        // 拿不到號
	fmt.Printf("count   %s\n", seq.Status(sender.account))

	_ = seq.Sync(ctx, sender.account, 10) // 鏈上說 9 已經上鏈了
	fmt.Printf("sync    %s\n", seq.Status(sender.account))
	now = now.Add(relayer.DefaultConfig().RetryAfter)
	run(nil)
	run(nil)
	run(nil)

	for _, id := range ids {
		it, _ := intents.Get(ctx, id)
		fmt.Printf("%s %-12s v%d tx=%s\n", id, it.State, it.Version, it.TxHash)
	}

	// Output:
	// sync    0x90F7…b906  next 7    in-flight -    gap -
	// worker  nonce 7    pi_0001/settle   #1  sent         tx 0x0001
	// worker  nonce 8    pi_0002/settle   #1  retry        (send: relayer: transaction was not sent: signing failed)
	// worker  nonce 8    pi_0003/settle   #1  sent         tx 0x0003
	// worker  nonce 9    pi_0004/settle   #1  retry        (send: rpc: timeout)
	// worker  nonce -    pi_0005/settle   #1  retry        (no slot: txseq: account has an unfilled gap at 9)
	// count   0x90F7…b906  next 10   in-flight -    gap 9
	// sync    0x90F7…b906  next 10   in-flight -    gap -
	// worker  nonce -    pi_0002/settle   #2  retry        (settling for 5s without tx hash, waiting)
	// worker  nonce -    pi_0004/settle   #2  retry        (settling for 5s without tx hash, waiting)
	// worker  nonce 10   pi_0005/settle   #2  sent         tx 0x0005
	// pi_0001 confirming   v4 tx=0x0001
	// pi_0002 settling     v3 tx=
	// pi_0003 confirming   v4 tx=0x0003
	// pi_0004 settling     v3 tx=
	// pi_0005 confirming   v4 tx=0x0005
}

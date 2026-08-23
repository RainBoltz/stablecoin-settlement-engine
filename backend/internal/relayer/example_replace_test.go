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
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfee"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// evmReplacer 在 evmSender 之上多實作 relayer.ReplacingSender：同一個號可以再送一次。
// Replace 送的還是那筆付款（tx hash 加一個 r，看得出是第二次嘗試），Cancel 送的是一筆不動錢的交易。
type evmReplacer struct {
	account string
	err     error
	slot    string
	fee     string
}

func (s *evmReplacer) Account(*intent.Intent) string { return s.account }

func (s *evmReplacer) Send(context.Context, *intent.Intent) (string, error) {
	return "", fmt.Errorf("%w: evm transactions need a nonce", relayer.ErrNotSent)
}

func (s *evmReplacer) SendAt(_ context.Context, it *intent.Intent, res txseq.Reservation) (string, error) {
	s.slot = fmt.Sprintf("%d", res.Value)
	if s.err != nil {
		return "", s.err
	}
	return "0x" + it.ID[3:], nil
}

func (s *evmReplacer) Replace(_ context.Context, it *intent.Intent, res txseq.Reservation, fee txfee.Fee) (string, error) {
	s.slot, s.fee = fmt.Sprintf("%d", res.Value), txfee.Gwei(fee.Cap)
	if s.err != nil {
		return "", s.err
	}
	return "0x" + it.ID[3:] + "r", nil
}

func (s *evmReplacer) Cancel(_ context.Context, _ string, res txseq.Reservation, fee txfee.Fee) (string, error) {
	s.slot, s.fee = fmt.Sprintf("%d", res.Value), txfee.Gwei(fee.Cap)
	if s.err != nil {
		return "", s.err
	}
	return fmt.Sprintf("0xc%d", res.Value), nil
}

// Example_replaceStuck 是替換在 relayer 裡的長相：一個發送錢包、三筆付款，中間有一筆送出時 RPC 逾時。
//
//   - pi_0001 正常送出，用掉 nonce 7。
//   - pi_0002 送出時 RPC 逾時，不知道有沒有出門：8 當成用掉，序列上留一個洞。
//   - pi_0003 因此連號都拿不到，原封不動回 queue（intent 還在 authorized、帳上沒有 hold）。
//   - 卡過 StuckAfter 之後，pi_0002 把 8 搶回來，用高一成的出價再送一次同一筆付款。同一個 nonce 最多只有一筆會進區塊，
//     所以錢還是只動一次；洞補起來，帳戶恢復發號。
//   - pi_0003 於是拿到 9，正常送出。
//
// 值得注意的是 pi_0002 最後帶的 tx hash 是第二次嘗試的那一個。它是「我們最後送出去的那一筆」，不是
// 「最後進區塊的那一筆」——舊那筆也可能才是贏家。哪一個 hash 真的上鏈了要對鏈才知道。
func Example_replaceStuck() {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := queue.NewMemoryQueue()
	intents := intent.NewMemoryStore()
	journal := ledger.NewMemoryJournal()

	sender := &evmReplacer{account: "0x90F79bf6EB2c4f870365E785982E1f101E93b906"}
	seq := txseq.NewCounter()
	w := relayer.New(q, intents, journal, sender,
		relayer.WithClock(func() time.Time { return now }), relayer.WithSequencer(seq))

	_ = seq.Sync(ctx, sender.account, 7) // 問鏈上這個錢包用到哪了
	fmt.Printf("sync    %s\n", seq.Status(sender.account))

	ids := []string{"pi_0001", "pi_0002", "pi_0003"}
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
		sender.err, sender.slot, sender.fee = sendErr, "-", "-"
		rep, ok, _ := w.RunOnce(ctx)
		if ok {
			fmt.Printf("worker  nonce %-4s %s\n", sender.slot, rep)
		}
	}

	run(nil)
	run(errors.New("rpc: timeout")) // 不知道有沒有出門
	run(nil)                        // 拿不到號
	fmt.Printf("count   %s\n", seq.Status(sender.account))

	now = now.Add(relayer.DefaultConfig().StuckAfter)
	run(nil) // pi_0002 卡夠久了：把 8 搶回來加價重送
	fmt.Printf("count   %s\n", seq.Status(sender.account))
	run(nil) // pi_0003 拿到 9

	for _, id := range ids {
		it, _ := intents.Get(ctx, id)
		fmt.Printf("%s %-12s v%d tx=%s\n", id, it.State, it.Version, it.TxHash)
	}

	// Output:
	// sync    0x90F7…b906  next 7    in-flight -    gap -
	// worker  nonce 7    pi_0001/settle   #1  sent         tx 0x0001
	// worker  nonce 8    pi_0002/settle   #1  retry        (send: rpc: timeout)
	// worker  nonce -    pi_0003/settle   #1  retry        (no slot: txseq: account has an unfilled gap at 8)
	// count   0x90F7…b906  next 9    in-flight -    gap 8
	// worker  nonce 8    pi_0002/settle   #2  replaced     tx 0x0002r (speed-up #8 fill, cap 33.000 gwei tip 2.200 gwei)
	// count   0x90F7…b906  next 9    in-flight -    gap -
	// worker  nonce 9    pi_0003/settle   #2  sent         tx 0x0003
	// pi_0001 confirming   v4 tx=0x0001
	// pi_0002 confirming   v4 tx=0x0002r
	// pi_0003 confirming   v4 tx=0x0003
}

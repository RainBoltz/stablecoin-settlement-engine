package relayer

import (
	"context"
	"errors"
	"fmt"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfee"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// ReplacingSender 是「同一個號可以再送一次」的那類鏈的 Sender：EVM 與 TON。實作了它，worker 才敢去救一筆卡住的付款。
//
// 為什麼安全，只靠鏈上一條規則：同一個帳戶、同一個號的交易最多只有一筆會被收下。所以替換的兩種結局都不會讓錢多動一次
// ——新的那筆贏了，付款照樣完成；舊的那筆已經進區塊了，新的那筆會因為號用過而被拒。
//
// 沒實作的鏈（Solana、SUI）卡住的時候只能原封不動重送同一筆簽好的交易，那不是替換，也不需要 relayer 介入
// （見 internal/txfee 的 package 註解）。
type ReplacingSender interface {
	OrderedSender
	// Replace 在 res 那一格再送一次同一筆付款，出價 fee 比上一次高。
	Replace(ctx context.Context, it *intent.Intent, res txseq.Reservation, fee txfee.Fee) (txHash string, err error)
	// Cancel 在 res 那一格送一筆不動錢的交易（0 元自我轉帳），把那一格用掉。
	//
	// 它不帶 intent：要送的東西跟哪一筆付款無關，只跟帳戶與那個號有關。它比 Replace 便宜的原因也在這裡，
	// 一筆純轉帳燒的 gas 是 21000（https://ethereum.org/en/developers/docs/gas/#what-is-gas-limit），
	// 一筆 ERC-20 transfer 是它的好幾倍；出價一樣，總價差很多。
	Cancel(ctx context.Context, account string, res txseq.Reservation, fee txfee.Fee) (txHash string, err error)
}

// rescue 處理「intent 卡在 settling」那一格。
//
// 這一格的意思從第一天就沒變過：有一筆交易可能在鏈上，也可能不在，我們自己說不準。在這之前 relayer 對它只有兩招
// ——還年輕就等，超過 StuckAfter 就送審。今天多一招：把那個號搶回來，用更高的出價再送一次。
//
// 要送什麼、出多少價由 txfee.Decide 決定，這裡只負責把決定執行完：取號、送、記一筆、把 intent 推到下一格。
// 讀哪個號則是這裡的事——有洞就補洞（那才是替換），沒洞就拿一個新的（上一次確定沒發送出去，等於重來一次）。
func (w *Worker) rescue(ctx context.Context, d queue.Delivery, it *intent.Intent) (Report, error) {
	last, tries, ok, err := w.broadcasts.Last(ctx, it.ID)
	if err != nil {
		return Report{}, err
	}
	stuck := txfee.Stuck{
		Sent: last.Sent, Fee: last.Fee, Tries: tries,
		Age: w.now().Sub(it.UpdatedAt), StuckAfter: w.cfg.StuckAfter,
	}
	if !ok {
		// 沒有紀錄：這筆 intent 比紀錄本還老，或 worker 剛好死在 Send 與 Record 之間。當成「不知道」，
		// 那是三種發送結果裡最保守的一種，也是唯一不會讓我們誤以為鏈上乾淨的一種。
		stuck.Sent, stuck.Fee, stuck.Tries = txseq.SentUnknown, w.fee.Base, 1
	}
	plan := w.fee.Decide(stuck)

	switch plan.Kind {
	case txfee.KindWait:
		return Report{Outcome: OutcomeRetry, Detail: plan.Reason}, nil
	case txfee.KindReview:
		return w.review(ctx, it, plan.Reason)
	}

	repl, canReplace := w.sender.(ReplacingSender)
	if !canReplace {
		// 這條鏈換不掉已經送出去的交易（Solana、SUI），relayer 沒有第二招，只能照舊送審：理由跟以前一樣，
		// 那筆交易下落不明。
		return w.review(ctx, it, fmt.Sprintf("settling for %s without tx hash; broadcast outcome unknown", stuck.Age))
	}

	// 跟 authorized 那條路一樣要過限流：這一步一樣是往 RPC 送一筆交易，名額限的是整個 pool 對外的總量。
	actx, cancel := context.WithTimeout(ctx, d.LeaseUntil.Sub(w.now()))
	err = w.limiter.Acquire(actx)
	cancel()
	if err != nil {
		return Report{Outcome: OutcomeRetry, Detail: "throttled: " + err.Error()}, nil
	}
	defer w.limiter.Release()

	account := repl.Account(it)
	res, rerr := w.reserveFor(ctx, d, account, plan.Kind)
	if rerr != nil {
		if errors.Is(rerr, txseq.ErrNoGap) {
			// 要送取消交易、卻沒有洞可以補：那個號早就好好地用掉了，沒有東西可以搶回來。人來看。
			return w.review(ctx, it, "no slot to clear: "+plan.Reason)
		}
		return Report{Outcome: OutcomeRetry, Detail: "no slot: " + rerr.Error()}, nil
	}

	// 收尾規則跟昨天一模一樣：預設「確定沒發送」，只有真的走到送出去那一步才改。差別在於補洞的那一次，
	// 送成功只是把洞填起來，不會讓計數器往前（見 txseq.Counter.Resolve）。
	sent := txseq.SentNo
	defer func() { _ = w.seq.Resolve(context.WithoutCancel(ctx), res, sent) }()

	var txHash string
	var serr error
	if plan.Kind == txfee.KindCancel {
		txHash, serr = repl.Cancel(ctx, account, res, plan.Fee)
	} else {
		txHash, serr = repl.Replace(ctx, it, res, plan.Fee)
	}
	if serr != nil {
		if !errors.Is(serr, ErrNotSent) {
			sent = txseq.SentUnknown
		}
		w.record(ctx, it, res, plan.Fee, "", sent)
		return Report{Outcome: OutcomeRetry, Detail: string(plan.Kind) + ": " + serr.Error()}, nil
	}
	sent = txseq.SentYes
	w.record(ctx, it, res, plan.Fee, txHash, sent)
	// detail 只寫「站哪一格、出多少價」，帳戶不寫：一行要塞得進 log，而帳戶在 Broadcast 的紀錄裡。
	slot := fmt.Sprintf("#%d", res.Value)
	if res.Fill {
		slot += " fill"
	}
	detail := fmt.Sprintf("%s %s, %s", plan.Kind, slot, plan.Fee)

	if plan.Kind == txfee.KindCancel {
		// 取消交易送出去了，號清出來了，但這筆付款的結局還沒定：要等那一格真的進區塊，才知道贏的是取消還是原本那筆。
		// 那件事要有人盯著鏈才看得到，所以這裡只能把 intent 交出去，不能自己宣告 failed。
		reason := fmt.Sprintf("%s; sent no-op tx %s to clear the slot, outcome still unknown", detail, txHash)
		if _, err := w.reviewIntent(ctx, it, reason); err != nil {
			return Report{}, err
		}
		return Report{Outcome: OutcomeCleared, TxHash: txHash, Detail: detail}, nil
	}

	// 加速成功。intent 走到 confirming，帶的是最後一次嘗試的 tx hash——不是「最後上鏈的那一筆」的 hash，
	// 因為舊那筆也可能才是贏家。tx hash 識別的是一次嘗試，不是一筆付款；哪一個 hash 真的進了區塊要對鏈才知道。
	if err := w.advance(ctx, it, intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: txHash, At: w.now()}); err != nil {
		if errors.Is(err, intent.ErrVersionConflict) {
			return Report{Outcome: OutcomeRetry, Detail: "lost the race to confirming, will re-read"}, nil
		}
		return Report{}, err
	}
	return Report{Outcome: OutcomeReplaced, TxHash: txHash, Detail: detail}, nil
}

// reserveFor 決定這一次要站哪一格：先看序列上有沒有洞，有就把那個號搶回來（那才是替換），
// 沒有就拿一個新的號重來一次。
//
// 取消交易只走補洞這條路：沒有洞的時候本來就沒有東西擋著後面的交易，再送一筆不動錢的交易只是白燒 gas。
func (w *Worker) reserveFor(ctx context.Context, d queue.Delivery, account string, kind txfee.Kind) (txseq.Reservation, error) {
	rctx, cancel := context.WithTimeout(ctx, d.LeaseUntil.Sub(w.now()))
	defer cancel()
	res, err := w.seq.ReserveGap(rctx, account)
	if err == nil || kind == txfee.KindCancel || !errors.Is(err, txseq.ErrNoGap) {
		return res, err
	}
	return w.seq.Reserve(rctx, account)
}

// review 把一筆 intent 推到 needs_review，然後回一份 Report。輸掉 CAS 的話不算失敗：別人動了它，重讀再說。
func (w *Worker) review(ctx context.Context, it *intent.Intent, reason string) (Report, error) {
	conflict, err := w.reviewIntent(ctx, it, reason)
	if err != nil {
		return Report{}, err
	}
	if conflict {
		return Report{Outcome: OutcomeRetry, Detail: "lost the race to needs_review, will re-read"}, nil
	}
	return Report{Outcome: OutcomeReview, Detail: reason}, nil
}

// reviewIntent 只做狀態轉移那一步，回報是不是輸了 CAS。
func (w *Worker) reviewIntent(ctx context.Context, it *intent.Intent, reason string) (conflict bool, err error) {
	err = w.advance(ctx, it, intent.Request{To: intent.StateNeedsReview, By: intent.ActorRelayer, Reason: reason, At: w.now()})
	if errors.Is(err, intent.ErrVersionConflict) {
		return true, nil
	}
	return false, err
}

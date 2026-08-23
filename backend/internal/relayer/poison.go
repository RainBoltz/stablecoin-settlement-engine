package relayer

import (
	"context"
	"errors"
	"fmt"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/dlq"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfail"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// poison 是「這份 job 不再交付」之後的處置，兩件事：那筆付款怎麼收尾（giveUp），還有那份 job 停到哪裡去。
//
// 順序是付款先、便條後。便條上要寫「停進去的當下那筆 intent 在哪一格」，那是人打開 DLQ 第一眼看的東西，
// 寫成收尾之前的樣子沒有用。停不進去的話這次不 Ack，job 的 lease 過期後會再交付一次，判決再走一遍。
//
// 這裡有一個洞：intent 收尾成功、Park 之前程序死掉的話，下一次交付會看到一個已經收尾的 intent，
// 走的是 process 的 no-op 那條路，便條就不會停進來了。掉的是便利不是安全——那筆 intent 還在它該在的地方，
// failed 是終態，needs_review 本來就是一面舉起來的旗子。
func (w *Worker) poison(ctx context.Context, d queue.Delivery, rep Report, v txfail.Verdict) (Report, error) {
	out, state, err := w.giveUp(ctx, d, rep, v)
	if err != nil || out.Outcome != OutcomePoison {
		return out, err
	}
	if _, err := w.dead.Park(ctx, dlq.Record{
		Job: d.Job, Attempts: d.Attempt, Reason: out.Detail, IntentState: state,
	}, w.now()); err != nil {
		return Report{}, err
	}
	return out, nil
}

// giveUp 是判決落在那筆付款上的那一半，順便回報收尾之後 intent 停在哪一格（給 poison 寫進便條）。
//
// 判決判的是 job，不是付款：job 只是一張「去看看它」的便條，便條再交付幾次都一樣，不代表那筆付款失敗了。
// 所以這裡先重讀一次 intent，照它現在停在哪一格決定要不要動它：
//
//   - created / authorized：relayer 一個 byte 都還沒寫（沒有 settling、沒有 hold、沒有廣播），
//     而且轉移表上它在這兩格只有一條出口，就是往前推到 settling。宣告失敗不是它的權力，所以這裡只丟便條，
//     intent 原封不動。之後誰再丟一份新的 job 進來，它照樣被處理（同 ID 的 job Ack 之後再 Enqueue
//     就是新的一份工作，見 queue 的 package 註解），而現在那個「誰」可以是拿著 dlq 的人。
//   - settling：帳上有 hold，鏈上可能有一筆交易。上一次廣播「確定沒發送出去」的話錢確定沒動，
//     這時候 relayer 才有資格走轉移表上那條 settling -> failed；另外兩種發送結果都說不準，推 needs_review。
//     沒有紀錄也算說不準：worker 可能死在 Send 與 Record 之間（跟 rescue 對 !ok 的處理同一條規矩）。
//
// 宣告 failed 之前要先把 hold void 掉，順序跟記 hold 一樣是「帳先動、狀態後走」：中間死掉重來一次，
// void 重放是 no-op、intent 再走一次 failed；反過來先宣告 failed 的話那筆 hold 會永遠掛在 pending 上，
// 因為 failed 是 terminal，沒有人回得來收尾它。
func (w *Worker) giveUp(ctx context.Context, d queue.Delivery, rep Report, v txfail.Verdict) (Report, string, error) {
	it, err := w.intents.Get(ctx, d.Job.IntentID)
	if err != nil {
		if errors.Is(err, intent.ErrNotFound) {
			// 指著一筆不存在的 intent 的 job 也要停進 DLQ，而且那是最該有人去看的一種：誰丟的？
			// 便條上那一格寫 -，因為真的沒有東西可以寫。
			return Report{Outcome: OutcomePoison, Detail: v.Reason + "; intent not found, dropping job"}, "-", nil
		}
		return Report{}, "", err
	}
	if it.State != intent.StateSettling {
		return Report{Outcome: OutcomePoison,
			Detail: fmt.Sprintf("%s; job dropped, intent still %s", v.Reason, it.State)}, string(it.State), nil
	}

	sent := txseq.SentUnknown
	if last, _, ok, berr := w.broadcasts.Last(ctx, it.ID); berr != nil {
		return Report{}, "", berr
	} else if ok {
		sent = last.Sent
	}
	// 理由欄要留得住整件事：判決一句、最後一次失敗的細節一句。人與之後的對帳都是看這一句決定下一步。
	reason := fmt.Sprintf("%s (%s)", v.Reason, rep.Detail)

	if sent != txseq.SentNo {
		reason = fmt.Sprintf("%s; last broadcast %s, outcome unknown", reason, sent)
		conflict, rerr := w.reviewIntent(ctx, it, reason)
		if rerr != nil {
			return Report{}, "", rerr
		}
		if conflict {
			return Report{Outcome: OutcomeRetry, Detail: "lost the race to needs_review, will re-read"}, "", nil
		}
		return Report{Outcome: OutcomePoison,
			Detail: fmt.Sprintf("%s; last broadcast %s, needs review", v.Reason, sent)}, string(it.State), nil
	}

	// 沒有 hold 可以放（worker 死在 CAS 與記帳之間），或者它已經被收尾過了，都不影響接下來要做的事。
	if _, _, err := w.journal.Append(ctx, voidEntry(it)); err != nil &&
		!errors.Is(err, ledger.ErrNoSuchHold) && !errors.Is(err, ledger.ErrAlreadyResolved) {
		return Report{}, "", err
	}
	reason += "; nothing was broadcast, no money moved"
	if err := w.advance(ctx, it, intent.Request{To: intent.StateFailed, By: intent.ActorRelayer, Reason: reason, At: w.now()}); err != nil {
		if errors.Is(err, intent.ErrVersionConflict) {
			return Report{Outcome: OutcomeRetry, Detail: "lost the race to failed, will re-read"}, "", nil
		}
		return Report{}, "", err
	}
	return Report{Outcome: OutcomePoison, Detail: v.Reason + "; nothing was sent, so the intent failed"}, string(it.State), nil
}

// voidEntry 是宣告 failed 之前放掉那筆 hold 的那一列：void 沒有腿，pending 歸零就是全部。
//
// ID 是 <intent id>/void、At 用 intent 進 settling 的時間、Memo 是固定的一句話——三個都不帶「這是第幾次交付」
// 之類的資訊，這樣不管哪個 worker 來算，算出來的 void 都一模一樣，journal 對它才是冪等的
// （重放 no-op，而不是 ErrConflict）。為什麼放棄的理由不寫在這裡：它寫在 intent 的 History 上，
// 那裡才是「這筆付款發生過什麼」的地方，帳本只回答「錢在哪」。
func voidEntry(it *intent.Intent) ledger.Entry {
	return ledger.Entry{
		ID:    it.ID + "/void",
		Ref:   it.Ref,
		Kind:  ledger.KindVoid,
		Holds: it.ID + "/hold",
		Asset: ledger.Asset{Chain: it.Chain, Token: it.Token},
		By:    string(intent.ActorRelayer),
		At:    it.UpdatedAt,
		Memo:  "nothing was broadcast, releasing the hold",
	}
}

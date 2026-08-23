// Package dlq 是停止重試的 job 的收容所：一份被判 poison 的 job 不再直接從 queue 上消失，
// 而是連著「它為什麼被放棄、放棄的當下那筆 intent 停在哪一格」一起停在這裡，等人來看。
//
// 名字沿用業界的 dead-letter queue，但它在這裡不是第二條 queue：沒有 Lease、沒有 Ack、後面沒有 worker 在領。
// 理由就是 poison 的定義本身——「再交付幾次結果都一樣」——後面接一個會自己去領的 consumer，
// 只是把同一個迴圈拉長。業界的幾個實作也都是這樣：Azure Service Bus 的 dead-letter queue 明講
// 「There's no automatic cleanup of the DLQ. Messages remain in the DLQ until you explicitly retrieve them」
// （https://learn.microsoft.com/en-us/azure/service-bus-messaging/service-bus-dead-letter-queues），
// Sidekiq 的 Dead set 講得更短「Sidekiq will not retry those jobs, you must manually retry them via the UI」
// （https://github.com/sidekiq/sidekiq/wiki/Error-Handling）。
//
// 人能做的只有兩件事，而且都要明確按下去：Redrive（把那份 job 原封不動放回 queue）與 Drop（承認它沒有用了）。
// 原封不動這件事也是抄來的，SQS 的 redrive 同樣不給改內容
// （「Amazon SQS doesn't support filtering and modifying messages while redriving them from the dead-letter queue」，
// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-configure-dead-letter-queue-redrive.html）。
// 對我們來說改也沒有意義：一份 job 只帶 intent id 與 ref，決定接下來會發生什麼的是 intent store 現在的內容。
//
// 「放回去不會讓錢多動一次」也是同一個理由，而且它不是這個 package 給的保證：redrive 只是讓那份 job 再被交付一次，
// 領到它的 worker 每一次都重讀 intent、照它現在的狀態決定要做什麼（見 relayer.Worker.process）。
// 所以放回一筆已經走掉的 intent 只會換到一次 no-op；redrive 真正救得回來的只有「那筆 intent 還在等 relayer 動它」這一種。
//
// 這個 package 不 import intent：Record 上那個狀態只是一個字串，是停進來那一刻的照片，給人看的，不給程式判斷。
// 人打開它的時候照片多半已經過期了，要判斷就得去 intent store 重讀。
//
// 本 package 為本系列從零設計，只取公開設計裡需要的那部分。
package dlq

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
)

// Status 是一筆 Record 目前的處置。只有 parked 是「還沒有人處理」，另外兩種都代表有人按過按鈕。
type Status string

const (
	// StatusParked：還停著，等人來看。
	StatusParked Status = "parked"
	// StatusRedriven：人把那份 job 放回 queue 了。
	StatusRedriven Status = "redriven"
	// StatusDropped：人看過了，決定不放回去。
	StatusDropped Status = "dropped"
)

var (
	// ErrInvalidRecord：紀錄缺欄位，或想把它處置成一個不存在的狀態。
	ErrInvalidRecord = errors.New("dlq: invalid record")
	// ErrNotFound：沒有這一筆。
	ErrNotFound = errors.New("dlq: record not found")
	// ErrNotParked：這一筆已經被處置過了。兩個人同時按下 redrive 時，晚的那個會拿到它。
	ErrNotParked = errors.New("dlq: record is not parked")
)

// Record 是一份被放棄的 job 停在這裡的樣子。
type Record struct {
	// Job 是原封不動的那一份，redrive 就是把它照原樣放回 queue。
	Job queue.Job
	// Attempts 是這一趟被交付過幾次（queue.Delivery.Attempt）。redrive 之後 queue 對同 ID 的 job 重新計次，
	// 所以它數的是這一趟，不是這份 job 的一生；跨趟的次數看 Cycles。
	Attempts uint64
	// Reason 是判決那一句加上最後一次失敗的細節，一路從 txfail.Verdict 傳過來。寫給人看，不給程式判斷。
	Reason string
	// IntentState 是停進來那一刻那筆 intent 停在哪一格。它是字串不是 intent.State，因為它是一張過期的照片：
	// 人看到它的時候那筆 intent 可能早就走掉了。它的用途是讓人一眼分出「這筆付款已經結案」與「這筆還在等」。
	IntentState string
	// Cycles 是這份 job 停進來過幾趟，從 1 起算。放回去又回來的東西多半不是再放一次就會好，
	// 所以它是「該不該再按一次 redrive」的訊號。
	Cycles uint64
	// Status 與 ParkedAt 由 Store.Park 填，呼叫端給的值會被蓋掉。
	Status   Status
	ParkedAt time.Time
	// ResolvedBy 與 ResolvedAt 記的是誰、什麼時候按的按鈕。稽核要看的是這兩欄：一筆付款被人推了一把
	// 跟它自己走完，在帳本上長得一樣，差別只留在這裡。
	ResolvedBy string
	ResolvedAt time.Time
}

// Validate 檢查一筆紀錄自己就看得出來的問題。Reason 是必填的：一份沒有理由的放棄，人打開來也不知道該做什麼。
func (r Record) Validate() error {
	if err := r.Job.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	if r.Reason == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalidRecord)
	}
	return nil
}

// String 用固定格式印一列：job、這一趟被交付過幾次、目前的處置、停進來時 intent 停在哪一格、誰處置的、理由。
// 還沒有人處置的那一欄印一個 -，欄位數才不會跟著狀態變。Example 直接貼這個格式。
func (r Record) String() string {
	by := "-"
	if r.ResolvedBy != "" {
		by = r.ResolvedBy
	}
	return fmt.Sprintf("%-16s #%-2d %-8s %-12s %-4s %s",
		r.Job.ID, r.Attempts, r.Status, r.IntentState, by, r.Reason)
}

// Store 是收容所本身。它跟 queue.Queue 長得不一樣的地方就是它想強調的事：沒有 Lease、沒有 Ack，
// 因為沒有 worker 會來領；能讓一筆紀錄動起來的只有 Resolve，而 Resolve 一定有人簽名。
//
// 時間一律由呼叫端傳進來，跟 queue.Queue 與 idempotency.Store 一樣，這樣測得動。
type Store interface {
	// Park 停一份 job 進來。同一份 job 已經停著就是 no-op，回傳 applied=false：去重的範圍只有「還停著」，
	// 跟 queue.Enqueue 的規矩一樣。處置過的同 ID 再進來算新的一趟，Cycles 加一。
	Park(ctx context.Context, r Record, now time.Time) (applied bool, err error)
	// Get 讀一筆。
	Get(ctx context.Context, jobID string) (Record, error)
	// List 依停進來的先後列出某一種處置的紀錄；status 給空字串就全部列出來。
	List(ctx context.Context, status Status) ([]Record, error)
	// Resolve 把一筆從 parked 改成 redriven 或 dropped，並記下是誰做的。已經處置過的回 ErrNotParked。
	// 這是這個 package 唯一的原子點：兩個人同時按下 redrive，只有一個會成功。
	Resolve(ctx context.Context, jobID string, to Status, by string, now time.Time) (Record, error)
}

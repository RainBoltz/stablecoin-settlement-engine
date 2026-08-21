// Package queue 是 relayer 的工作佇列：API 把 intent 推到 authorized 之後，丟一份「去處理 pi_xxx」的 job 進來，
// relayer 的 worker 從這裡領工作。它是 API 與 relayer 之間唯一的交接點，兩邊不直接呼叫對方。
//
// 這個 package 只保證一件事：**至少一次（at-least-once）**。一份 job 被領走之後有一段 lease，
// 期限內沒有 Ack 就會再被領一次；所以 job 可能被交付超過一次，但不會憑空消失。
// 「恰好一次」不是 queue 能給的承諾（AWS 對 SQS standard queue 的說法是 at-least-once、
// 「more than one copy of a message might be delivered」，
// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/standard-queues.html），
// 所以錢只動一次這件事不靠 queue，靠 worker 每一步都冪等：狀態機重放是 no-op、ledger 同 ID 重放是 no-op、
// 存檔靠 CAS。queue 只負責「工作不會掉」。
//
// job 裡只有指標（intent id 與 ref），沒有金額、沒有地址。intent store 才是唯一的真相；job 只是一張「去看看它」的便條。
// 帶著 payload 的 job 被重送時，worker 拿到的可能是舊資料，而重讀 intent store 永遠拿到最新的那份。
//
// 三個機制都沿用 SQS 的形狀，因為那是最多人熟悉的至少一次 queue：
//   - lease 對應 visibility timeout（https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-visibility-timeout.html）：
//     領走的 job 在期限內對別的 worker 隱形，期限一過就再度可見。
//   - Receipt 對應 receipt handle：Ack 與 Nack 要帶著領工作時拿到的憑證，lease 過期後被別人領走，
//     舊憑證就作廢（ErrStaleReceipt）。跟 idempotency 的 Attempt、intent 的 Version 是同一招：晚到的寫入蓋不掉新的。
//   - Nack 對應「把 visibility timeout 改成 N 秒後」：這次做不完，過一會兒再交付一次，attempt 計次保留。
//
// 去重的範圍只有「還在 queue 裡」：同 ID 的 job 已經在排隊或被領走，再 Enqueue 是 no-op；Ack 之後同 ID 再進來就是新的一份工作
// （之後 listener 看到 reorg 要 relayer 重送，就是這樣進來的）。今天只有記憶體版；換成 SQS 或資料庫時介面不變。
package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// Kind 是 job 的種類。今天只有一種：把一筆 authorized 的 intent 送上鏈。
type Kind string

// KindSettle：去處理一筆 authorized 的 intent（推到 settling、記 hold、廣播、推到 confirming）。
const KindSettle Kind = "settle"

// Job 是一份工作。ID 由呼叫端給，慣例是「<intent id>/<kind>」，例如 pi_0001/settle，跟 ledger 的 entry ID 同一種取法：
// 同一筆 intent 的同一種工作，在 queue 裡最多排一份。
//
// 只有 IntentID 與 Ref，沒有金額與地址（見 package 註解）。Ref 帶著走是為了 log 與追蹤：從 API 到鏈上，
// 每一層印出來的都是同一個 ref。
type Job struct {
	ID       string
	Kind     Kind
	IntentID string
	Ref      paymentref.Ref
}

// Validate 檢查一份 job 自己就看得出來的問題。
func (j Job) Validate() error {
	switch {
	case j.ID == "":
		return fmt.Errorf("%w: id is required", ErrInvalidJob)
	case j.Kind != KindSettle:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidJob, j.Kind)
	case j.IntentID == "":
		return fmt.Errorf("%w: intent id is required", ErrInvalidJob)
	case j.Ref.IsZero():
		return fmt.Errorf("%w: ref is required", ErrInvalidJob)
	}
	return nil
}

// Receipt 是一次交付的憑證。Lease 發、Ack 與 Nack 收；只對「目前這一次交付」有效。
type Receipt string

// Delivery 是 Lease 交出來的東西：job 本身、這是第幾次交付、憑證、lease 到什麼時候。
type Delivery struct {
	Job Job
	// Attempt 從 1 起算，每被 Lease 一次加一。Nack 之後再交付也算新的一次。
	Attempt uint64
	Receipt Receipt
	// LeaseUntil 過了，這份 job 就會再度可見；worker 在這之前沒 Ack，工作會被別人領走。
	LeaseUntil time.Time
}

var (
	// ErrInvalidJob：job 缺欄位或種類不對。
	ErrInvalidJob = errors.New("queue: invalid job")
	// ErrNotFound：沒有這份 job（多半是已經 Ack 掉了）。
	ErrNotFound = errors.New("queue: job not found")
	// ErrStaleReceipt：憑證不是目前這一次交付的。你的 lease 過期、job 被別人領走了，你的 Ack / Nack 作廢。
	ErrStaleReceipt = errors.New("queue: receipt is stale")
)

// Queue 是工作佇列的介面。今天只有記憶體版；換成 SQS 或資料庫時介面不變，因為它要求的只有 at-least-once 加 lease。
//
// 時間一律由呼叫端傳進來（跟 idempotency.Store 一樣），queue 才測得動。
type Queue interface {
	// Enqueue 放一份 job。同 ID 已在 queue 裡（排隊中或被領走）就是 no-op，回傳 applied=false。
	Enqueue(ctx context.Context, job Job, now time.Time) (applied bool, err error)
	// Lease 領一份現在可見的 job，並讓它在 lease 期間對別人隱形。沒有可領的回傳 ok=false，不是錯誤。
	Lease(ctx context.Context, now time.Time, lease time.Duration) (d Delivery, ok bool, err error)
	// Ack 宣告這份工作做完了，job 從 queue 消失。憑證要是目前這一次交付的。
	Ack(ctx context.Context, d Delivery) error
	// Nack 宣告這次做不完（還沒輪到、送出失敗、輸了競爭），retryAfter 之後再交付。attempt 計次保留。
	Nack(ctx context.Context, d Delivery, retryAfter time.Duration, now time.Time) error
	// Len 回傳還沒 Ack 的 job 數（含被領走的），測試與 Example 用。
	Len(ctx context.Context) (int, error)
}

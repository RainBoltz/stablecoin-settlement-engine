package txfail

import (
	"errors"
	"fmt"
	"time"
)

// Fault 是一次失敗的樣子，Judge 的全部輸入。
//
// 它刻意只有三個欄位、而且都是值：Judge 不去問 queue、不去問 intent store、不看時鐘，才能被當成一張表來測。
type Fault struct {
	// Err 是這次失敗的原因。可能是 nil：「還沒 authorized」「輸掉 CAS」這種失敗沒有錯誤物件，
	// 它們也永遠不會被宣告成 poison，只會被預算收掉。
	Err error
	// Attempt 是這份 job 被交付過幾次，從 1 起算，由 queue 給（queue.Delivery.Attempt）。
	// 用 queue 的計次而不是自己記，是因為中途死掉的那幾次也要算：worker 死在半路一樣是一次白跑的交付。
	Attempt uint64
	// Base 是退避階梯的第一階，沿用 relayer 那三個時間常數裡的 RetryAfter。
	// 跟 txfee.Stuck 帶著 StuckAfter 進來是同一個做法：時間常數只有一份，放在 queue 那一側。
	Base time.Duration
}

// Verdict 是 Judge 的結果：這份 job 接下來怎麼辦、為什麼。Reason 寫給人看，會一路傳到 Report 的 detail
// 與 needs_review 的理由欄，不寫給程式判斷。
type Verdict struct {
	Class Class
	// Backoff 只有 ClassRetryable 有意義：Nack 之後隔多久再交付。
	Backoff time.Duration
	Reason  string
}

// String 用固定格式印一個判決，Example 會直接貼這個格式。
func (v Verdict) String() string {
	d := "-"
	if v.Class == ClassRetryable {
		d = v.Backoff.String()
	}
	return fmt.Sprintf("%-9s %-7s %s", v.Class, d, v.Reason)
}

// Judge 是整個 package 的決策樹，三條分支照這個順序判：
//
//  1. 錯誤宣告了 ErrPoison：直接停。這條擺第一，是因為它是唯一「知道為什麼」的一條，
//     知道的時候就不該再浪費一次交付去確認。
//  2. 交付次數用完了：也停。沒有人宣告不代表它有救，只代表沒有人事先想到這一種。
//  3. 其他：退避之後再來一次。
//
// 為什麼比較的是 Attempt >= MaxAttempts 而不是大於：Attempt 是「這次是第幾次」，這次已經失敗了，
// 所以第 MaxAttempts 次失敗的當下預算就是零。
func (p Policy) Judge(f Fault) Verdict {
	if errors.Is(f.Err, ErrPoison) {
		return Verdict{Class: ClassPoison, Reason: "retrying will not help"}
	}
	if p.MaxAttempts > 0 && f.Attempt >= uint64(p.MaxAttempts) {
		return Verdict{Class: ClassPoison, Reason: fmt.Sprintf("no luck after %d deliveries", f.Attempt)}
	}
	d := p.Backoff(f.Base, f.Attempt)
	return Verdict{Class: ClassRetryable, Backoff: d,
		Reason: fmt.Sprintf("delivery %d failed, next one in %s", f.Attempt, d)}
}

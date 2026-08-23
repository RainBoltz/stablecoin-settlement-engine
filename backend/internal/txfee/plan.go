package txfee

import (
	"fmt"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// Kind 是一筆卡住的付款下一步能做的四件事。
type Kind string

const (
	// KindWait：鏈上可能有一筆在排隊，還沒久到值得多花一次手續費。什麼都不做。
	KindWait Kind = "wait"
	// KindSpeedUp：再送一次同一筆付款，出價比上次高。舊那筆若還在 mempool 就被換掉，
	// 若已經進區塊則這一筆會被拒（nonce 用過了），兩種結果都不會讓錢多動一次。
	KindSpeedUp Kind = "speed-up"
	// KindCancel：不再想辦法讓這筆付款成功，改成在同一個號上送一筆不動錢的交易（0 元自我轉帳），
	// 把那一格用掉。救的是帳戶不是這筆付款：號清出來，後面排隊的付款才走得動。
	KindCancel Kind = "cancel"
	// KindReview：出價已經到天花板，加速與取消都贏不過舊交易了。交給人。
	KindReview Kind = "review"
)

// Stuck 是一筆卡在 settling 的付款目前的樣子，Decide 的全部輸入。
//
// 它刻意只有五個欄位、而且都是值：Decide 不去問鏈、不去問資料庫，才能被當成一張表來測。
type Stuck struct {
	// Sent 是上一次廣播的結果，昨天定下來的三種之一。SentNo 是唯一「鏈上確定沒有東西」的那一種。
	Sent txseq.Sent
	// Fee 是上一次出的價。要替換就得贏過它。
	Fee Fee
	// Tries 是這筆 intent 已經廣播過幾次（含第一次）。
	Tries int
	// Age 是這筆 intent 卡在 settling 多久。
	Age time.Duration
	// StuckAfter 是多久算卡住，沿用 relayer 那三個時間常數裡的同一個值。
	StuckAfter time.Duration
}

// Plan 是 Decide 的結果：做什麼、出多少價、為什麼。Reason 會一路傳到 Report 與 needs_review 的理由欄，
// 所以它寫給人看，不寫給程式判斷。
type Plan struct {
	Kind   Kind
	Fee    Fee
	Reason string
}

// String 用固定格式印一個決定，Example 會直接貼這個格式。
func (p Plan) String() string {
	if p.Fee.Zero() {
		return fmt.Sprintf("%-9s %s", p.Kind, p.Reason)
	}
	return fmt.Sprintf("%-9s %s (%s)", p.Kind, p.Fee, p.Reason)
}

// Decide 是整個 package 的決策樹，四條分支照這個順序判：
//
//  1. 還沒卡到 StuckAfter：等。這條在最前面，而且對三種發送結果一視同仁——就算上一次「確定沒發送出去」，
//     也不代表現在沒有人在動這筆 intent：lease 可能過期了而上一個 worker 還在送。StuckAfter 的意思一直都是
//     「這一格已經沒有人在動它」，不是「鏈上那筆等很久了」。
//  2. 加價之後超過天花板：加速與取消都贏不過舊交易，送出去只是多浪費一次嘗試。送審。
//  3. 廣播次數用完：不再想辦法讓這筆付款成功，改成把號清出來（見 KindCancel）。
//  4. 其他：加速。
//
// 為什麼「確定沒發送出去」也算進 Tries：簽名連續失敗三次是設定壞了，不是鏈上塞車，那種情況要人看，
// 不是一直重送。
func (p Policy) Decide(s Stuck) Plan {
	if s.Age < s.StuckAfter {
		// 這句話會原封不動變成 Report 的 detail：卡在 settling 的 intent 照定義就是沒有 tx hash 的那一種。
		return Plan{Kind: KindWait, Reason: fmt.Sprintf("settling for %s without tx hash, waiting", s.Age)}
	}

	// 只有「鏈上可能有東西」的時候才需要贏過上一次的出價；確定沒發送出去的話號早就退回去了，從基準價重來。
	fee := p.Base
	if s.Sent != txseq.SentNo {
		next, err := p.Bump(s.Fee)
		if err != nil {
			return Plan{Kind: KindReview, Reason: err.Error()}
		}
		fee = next
	}

	if s.Tries >= p.MaxTries {
		return Plan{Kind: KindCancel, Fee: fee,
			Reason: fmt.Sprintf("broadcast %d times already, giving up on this payment", s.Tries)}
	}
	return Plan{Kind: KindSpeedUp, Fee: fee,
		Reason: fmt.Sprintf("try %d, last broadcast %s", s.Tries+1, s.Sent)}
}

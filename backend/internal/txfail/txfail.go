// Package txfail 判一件事：一份失敗的 job 值不值得再交付一次。
//
// 在這之前 relayer 對失敗只有一種反應：Nack 回 queue、固定五秒後再來一次，然後永遠這樣下去。
// 對「RPC 抽筋」那種失敗它是對的，可是同一套反應套在「這條鏈根本沒有設定簽名金鑰」上，就是每五秒
// 把同一個錯誤重打一次，而且沒有終點：queue 裡永遠留著這份 job，每一圈吃掉一個 worker 的一次 Lease，
// 卻不會有任何一次成功。這個 package 就是那個終點。
//
// 分成兩類（見 Class）：
//
//   - retryable：換個時間、換個 worker 就可能不一樣。RPC 逾時、節點回 429、限流沒排到、輸掉 CAS 都是。
//   - poison：再交付幾次結果都一樣。內容本身出不去（參數組不出來、金鑰沒設定），或者已經試到沒有理由再試。
//
// 進 poison 有兩條路，缺一不可。第一條是錯誤自己宣告（包一個 ErrPoison），這條路快、精準，
// 但它要求呼叫端事先想得到；第二條是重試預算用完，這條路不需要任何人事先分類，代價是慢——一個一眼就沒救的錯誤
// 也要陪跑完整個階梯。所以預設偏樂觀（沒宣告的一律當成 retryable），跟昨天的 txseq 剛好相反：
// 那邊猜錯會撞上自己躺在 mempool 裡的交易，這邊猜錯只是多試幾次，而且有預算接住。
//
// 退避沿用 AWS 那篇 Exponential Backoff And Jitter
// （https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/）：每次加倍、有上限、加抖動。
// 抖動用的是那篇的 equal jitter（一半固定、一半隨機）而不是 full jitter，因為 full jitter 可以退到接近零，
// 等於沒退；而我們這裡每一次重新交付都要吃掉一份 lease 與一個 worker 的一圈，退避的下限得保住。
//
// 它跟 txfee 一樣是純函式：不碰鏈、不碰資料庫、不看時鐘，呼叫端把「這次的錯誤、這是第幾次交付」交給它，
// 它回一個 Verdict。判決之後那份 job 與那筆 intent 怎麼收尾是呼叫端的事（見 relayer.Worker.poison）。
//
// 本 package 為本系列從零設計，只取公開設計裡需要的那部分。
package txfail

import (
	"errors"
	"math/rand/v2"
	"time"
)

// Class 是一次失敗的分類：這份 job 再交付一次，會不會有不同的結果。
//
// 它判的是 job，不是付款。一份 job 被判 poison，意思是「這張便條再交付幾次都一樣」，
// 不是「這筆付款失敗了」；付款的結局要看副作用走到哪，那是呼叫端的事。
type Class string

const (
	// ClassRetryable：換個時間、換個 worker 就可能成功。退避之後再交付一次。
	ClassRetryable Class = "retryable"
	// ClassPoison：再交付幾次結果都一樣。停止重試。
	ClassPoison Class = "poison"
)

// ErrPoison 是呼叫端說「這個錯誤重試不會好」的方式：包在錯誤裡
// （fmt.Errorf("%w: chain %s has no signer", txfail.ErrPoison, chain)）Judge 才看得到。
//
// 跟 relayer.ErrNotSent 一樣是「要主動宣告」而不是「預設如此」，但預設的方向相反：那個預設偏保守
// （沒宣告的一律當成不知道有沒有送出去），這個預設偏樂觀（沒宣告的一律當成重試會好），
// 因為猜錯的代價不一樣——那邊猜錯是付兩次錢，這邊猜錯只是多試幾次，而且 Policy.MaxAttempts 接得住。
var ErrPoison = errors.New("txfail: not retryable")

// Policy 是重試的規則，三個旋鈕。第一階的長度不在這裡，由呼叫端隨每次失敗傳進來（見 Fault.Base），
// 因為那個數字是 queue 那一側的常數（relayer.Config.RetryAfter），不該有第二份。
type Policy struct {
	// MaxAttempts 是一份 job 最多交付幾次（含第一次）。超過就算沒有人宣告過，也停止重試。
	//
	// 這個數字要跟 relayer.Config.StuckAfter 對一下：預算的總長度必須撐得過 StuckAfter，
	// 不然一筆卡在 settling 的付款會在救援發生之前就先被判死，替換那一整套等於沒接上。
	MaxAttempts int
	// MaxBackoff 是退避的上限。沒有上限的加倍會讓一份 job 隔幾個小時才被看一次。
	MaxBackoff time.Duration
	// Jitter 把算出來的退避時間打散。N 個 worker 同時撞上同一個壞掉的節點時，它們的重試會排在同一秒，
	// 醒來又一起撞一次；抖動就是為了拆掉這種同步。nil 代表不抖，測試與 Example 才印得出固定的輸出。
	Jitter func(time.Duration) time.Duration
}

// DefaultPolicy：最多交付 10 次、退避加倍到 2 分鐘為止、equal jitter。
//
// 10 次配上 5 秒的第一階，加起來大約十分鐘（5+10+20+40+80+120×5），比 relayer 的 StuckAfter 五分鐘長，
// 救援才來得及在預算用完之前發生。三個數字都是這裡設的，不是誰的建議值。
func DefaultPolicy() Policy {
	return Policy{MaxAttempts: 10, MaxBackoff: 2 * time.Minute, Jitter: EqualJitter}
}

// Backoff 算第 attempt 次交付失敗之後要等多久：base 每次加倍、封頂在 MaxBackoff，最後過一次 Jitter。
//
// 用迴圈加倍而不是位移，是因為 attempt 可以很大：位移 64 次就繞回去了，迴圈碰到上限就停。
func (p Policy) Backoff(base time.Duration, attempt uint64) time.Duration {
	if base <= 0 {
		return 0
	}
	d := base
	for i := uint64(1); i < attempt && (p.MaxBackoff <= 0 || d < p.MaxBackoff); i++ {
		d *= 2
	}
	if p.MaxBackoff > 0 && d > p.MaxBackoff {
		d = p.MaxBackoff
	}
	if p.Jitter != nil {
		d = p.Jitter(d)
	}
	return d
}

// EqualJitter 回一個落在 [d/2, d] 的時間：一半固定、一半隨機。
//
// 固定的那一半是下限，保證真的退了一步；隨機的那一半負責把 N 個 worker 的重試拆開。
// 這是 AWS 那篇量過的三種抖動裡的中間那一種，選它的理由見 package 註解。
func EqualJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.N(int64(d-half)+1))
}

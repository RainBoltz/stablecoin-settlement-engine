// Package idempotency 是 API 層的去重機制：同一個客戶端對同一筆請求重送幾次，
// 都只執行一次、都拿到同一個答案。
//
// 這個 package 只認三樣東西：誰（Scope）、哪一次請求（Key）、請求長什麼樣（Fingerprint）。
// 它不知道 Payment Intent 是什麼，也不碰鏈；它保護的是「handler 只跑一次」這件事本身，
// 所以任何會產生副作用的 POST 都能包在它後面。
//
// 設計參考兩份公開文件：IETF httpapi 工作組的 Idempotency-Key header 草案
// （https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-idempotency-key-header-07：
// 缺 key 回 400、同 key 不同內容回 422、原請求還在跑回 409），以及 Stripe 的公開行為
// （https://docs.stripe.com/api/idempotent_requests：key 最長 255 字元、24 小時後可回收、
// 只要 endpoint 開始執行，結果不論成敗都存下來、重放時帶 Idempotent-Replayed: true）。
package idempotency

import (
	"crypto/sha256"
	"errors"
	"fmt"
)

// Scope 是 key 的主人。key 只在 scope 內唯一：兩個 merchant 各自用 "order-1" 當 key 互不影響，
// 別人也猜不到、拿不走你的 key 對應的答案。今天 scope 就是 API 憑證的字串本身；
// 之後接上真正的驗證時，只換「從 request 算出 scope」那一個函式。
type Scope string

// Key 是客戶端替「這一次意圖」取的名字。它是客戶端的承諾：同一個 key 代表同一件事；
// 伺服器的承諾則是：同一個 key 只做一次、每次都回同一個答案。
//
// 伺服器不能替客戶端保證 key 有足夠亂度（Stripe 建議 UUID v4），只能保證別人的 key 不會撞到你的。
type Key string

// MaxKeyLen 是 key 的長度上限，沿用 Stripe 公開的 255。太長的 key 多半是有人把整個 request 塞進來當 key。
const MaxKeyLen = 255

// ErrInvalidKey：key 為空、太長、或含有非可見 ASCII。
var ErrInvalidKey = errors.New("idempotency: invalid key")

// Validate 只做語法檢查：1 到 255 個可見 ASCII 字元（0x21 到 0x7E）。
//
// 為什麼不收空白與非 ASCII：header 值經過各種 proxy 與 SDK 時，空白會被修剪、非 ASCII 會被重新編碼，
// 到伺服器手上可能已經不是客戶端送出的那個字串，同一個 key 就會被認成兩個。
func (k Key) Validate() error {
	if len(k) == 0 {
		return fmt.Errorf("%w: empty", ErrInvalidKey)
	}
	if len(k) > MaxKeyLen {
		return fmt.Errorf("%w: %d bytes, max %d", ErrInvalidKey, len(k), MaxKeyLen)
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7E {
			return fmt.Errorf("%w: byte %d is not visible ASCII", ErrInvalidKey, i)
		}
	}
	return nil
}

// Fingerprint 是「這個請求長什麼樣」的摘要。同一個 key 配上不同的 fingerprint 是客戶端的 bug
// （把上一筆訂單的 key 拿來付下一筆），要大聲拒絕，不能默默回上一筆的答案。
type Fingerprint [sha256.Size]byte

// FingerprintOf 對 method、path 與原始 body bytes 做 SHA-256。
//
// 刻意用原始 bytes 而不是解析後的 JSON：{"a":1,"b":2} 與 {"b":2,"a":1} 會被當成兩個不同的請求。
// 這會多出一些「其實一樣卻被判不一樣」的 422，但反過來（不一樣卻被判一樣）就是付錯錢，
// 而且不需要在去重層知道每個 endpoint 的 body 格式。寧可誤殺。
func FingerprintOf(method, path string, body []byte) Fingerprint {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	var fp Fingerprint
	copy(fp[:], h.Sum(nil))
	return fp
}

// String 回傳前 8 個 bytes 的 hex，給錯誤訊息與 log 用；完整值沒有人需要用眼睛看。
func (f Fingerprint) String() string {
	return fmt.Sprintf("%x", f[:8])
}

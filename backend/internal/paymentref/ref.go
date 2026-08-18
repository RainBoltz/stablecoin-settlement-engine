// Package paymentref 是貫穿鏈上鏈下的追蹤鍵 PaymentRef：一筆 Payment Intent 落地那一刻就算得出來，
// 之後每一層帶著它走，最後寫進鏈上交易（EVM 的 calldata 與 event、Solana 的 memo、TON 的 comment、
// SUI 的 event），再由 listener 從鏈上撈回來，換回 intent。
//
// 它跟另外兩把 key 的分工：
//   - Idempotency Key 是客戶端取的、只在 scope 內唯一、24 小時後回收，只活在 API 邊界，鏈上不知道它存在。
//   - intent id（pi_…）是伺服器產生的、全系統唯一、永遠不變，鏈下每一層拿它找資料。
//   - PaymentRef 是從 intent id 與付款條件推導出來的 32 bytes，唯一會跨過鏈的邊界的 key。
//
// 為什麼不直接把 intent id 放上鏈：鏈上要的是固定長度（EVM 的 bytes32、mapping 的 key、event 的 topic），
// 而且 pi_ 開頭的字串放上公開帳本，等於把 API 的識別碼攤在所有人面前。
// 為什麼不用亂數：PaymentRef 是對「這筆付款的條件」做的 commitment，拿著 intent 的任何人都能重算、
// 對得上就代表這一列從落地到現在沒被動過。稽核要的就是這個。
//
// 雜湊用 SHA-256 而不是 keccak256：Go 標準函式庫就有（這個 module 到目前為止零外部依賴），
// 而且四條鏈的合約與程式都算得動；32 bytes 也是四條鏈都塞得下的長度。
//
// 銀行體系早就有同一個概念：SWIFT gpi 的 UETR 是一串 36 字元、由發起行產生、
// 沿途每一家銀行只能原樣轉傳不能改的追蹤碼（https://www.swift.com/payments/what-unique-end-end-transaction-reference-uetr）。
// 鏈上也有前例：Request Network 把 8 bytes 的 paymentReference 當參數傳進代理合約、寫進 event
// （https://github.com/RequestNetwork/requestNetwork/blob/master/packages/advanced-logic/specs/payment-network-erc20-fee-proxy-contract-0.1.0.md）。
// 這裡選 32 bytes 而不是 8 bytes：EVM 的 topic 與 mapping key 本來就是一個 word，
// 而且 32 bytes 才夠當 commitment 用，8 bytes 只夠當標籤。
package paymentref

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Size 是 PaymentRef 的位元組數。
const Size = sha256.Size

// Ref 是一筆付款的 PaymentRef。零值代表「還沒算」，任何地方看到零值都是 bug。
type Ref [Size]byte

// DomainV1 是雜湊前綴。它讓 PaymentRef 跟任何其他系統對同一組欄位算出來的 SHA-256 不會撞在一起，
// 也讓之後若要換編碼有地方放版本號：換版本就換前綴，舊的 ref 不會被誤認成新的。
const DomainV1 = "stablecoin-settlement-engine/payment-ref/v1"

// Terms 是被 commit 進 PaymentRef 的「付款條件」：這筆付款是什麼，而不是它走到哪了。
//
// 只放建立之後就不會再變的欄位。State、Version、TxHash 會變，時間欄位是簽名迴圈的事，都不放：
// ref 在 intent 落地那一刻算出來之後就要一路不變，變了 listener 就對不回來。
//
// 欄位一律是字串，而且是「intent 上存的那個字串」：這裡不做大小寫或格式正規化。
// 正規化是 API 收請求時的事；PaymentRef 只保證「跟存下來的那一列一模一樣」。
type Terms struct {
	IntentID string
	Chain    string
	Token    string
	Payer    string
	Merchant string
	// Amount 是十進位整數字串（最小單位），跟 API 的 body 同一種寫法。
	Amount string
}

// Derive 從付款條件算出 PaymentRef。同樣的 Terms 永遠算出同樣的 Ref，這是整個 package 唯一的承諾。
func Derive(t Terms) Ref {
	return Ref(sha256.Sum256(Preimage(t)))
}

// Preimage 是被雜湊的原始位元組：前綴加上六個欄位，每個欄位前面帶 uvarint 長度。
//
// 為什麼要長度前綴而不是用分隔符：("ab","c") 與 ("a","bc") 用分隔符拼起來可能一樣，帶長度就不會；
// 而且欄位裡不管出現什麼字元都不用跳脫。公開這個函式是給稽核工具用的：拿到一筆 intent，
// 不必信任我們的資料庫，自己算一次就能對鏈上那 32 bytes。
func Preimage(t Terms) []byte {
	fields := []string{DomainV1, t.IntentID, t.Chain, t.Token, t.Payer, t.Merchant, t.Amount}
	var b []byte
	for _, f := range fields {
		b = binary.AppendUvarint(b, uint64(len(f)))
		b = append(b, f...)
	}
	return b
}

// IsZero 回報這是不是還沒算過的零值。
func (r Ref) IsZero() bool {
	return r == Ref{}
}

// String 是 PaymentRef 的文字形式：0x 加 64 個小寫 hex。
// 走 EVM 時放的是原始 32 bytes；走 memo、comment、log、URL 這類只能放文字的地方，就放這個字串。
func (r Ref) String() string {
	return "0x" + hex.EncodeToString(r[:])
}

// ErrInvalidRef：不是 0x 加 64 個 hex 字元。
var ErrInvalidRef = errors.New("paymentref: invalid ref")

// Parse 把文字形式讀回 Ref。只收 String 印得出來的形狀（0x 前綴、64 個 hex，大小寫不拘）：
// 一把追蹤鍵如果有兩種以上的寫法，遲早會有兩個系統對同一筆付款算出不同的字串。
func Parse(s string) (Ref, error) {
	var r Ref
	if len(s) != 2+2*Size || !strings.HasPrefix(s, "0x") {
		return r, fmt.Errorf("%w: want 0x followed by %d hex chars, got %d chars", ErrInvalidRef, 2*Size, len(s))
	}
	if _, err := hex.Decode(r[:], []byte(s[2:])); err != nil {
		return r, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	return r, nil
}

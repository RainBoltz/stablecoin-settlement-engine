// Package chain 把「一條鏈要回答的問題」收成一個介面：Adapter，一條鏈一個代表，向 Registry 註冊。
//
// 在這之前，每條鏈的答案散在各個 package 自己的 Defaults() 裡：finality.Defaults() 知道四條鏈的
// 不可逆規則、bulk.Defaults() 只有兩條鏈的交易上限、txseq 把鏈分成「自己發號」與「不用發號」兩類、
// txfee 只服務發號的那一類。每一份都用 "evm"、"solana" 這種字串當 key，而沒有任何一個地方保證
// 它們對同一條鏈的認知一致：一條鏈可以在這份設定裡存在、在另一份裡查不到，而這件事要等到某筆付款
// 真的走到那一步才會被發現（listener 的 ErrNoPolicy、bulk 的 ErrNoRules 都是那個時候才回得出來的錯）。
// Registry 把同一類錯誤搬到接線的那一刻：答不齊全部問題的 adapter 根本註冊不進去。
//
// Adapter 介面只放每條鏈都答得出來的問題。答不出來就不成立的能力，照這個 repo 既有的做法另開一個
// 小介面、用型別斷言問：relayer.OrderedSender 與 relayer.ReplacingSender 都是這一款，標準函式庫的
// http.Flusher（https://pkg.go.dev/net/http#Flusher）是同一個模式。介面自己則遵守「越大越弱」：
// Go Proverbs（https://go-proverbs.github.io/）那句 "The bigger the interface, the weaker the
// abstraction." 就是這個 package 的設計判斷標準。
//
// adapter 只當索引：不可逆規則還是住在 finality、交易上限還是住在 bulk、替換規則還是
// 住在 txfee，這裡不抄任何數字。搬進來的話同一個數字就有兩個家，遲早不同步。
//
// 本 package 為本系列從零設計，只取公開設計裡需要的那部分。
package chain

import (
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfee"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// Adapter 是一條鏈在這個 process 裡的唯一代表，回答鏈下每個元件都會問到的四個問題。
//
// 這四題是「必答題」：任何一條鏈接進來都得答得出來，答不出來的鏈根本接不進這個系統。
// 有些鏈答不出來的問題（例如「卡住的交易怎麼替換」）刻意不在這裡，見 Replacer。
type Adapter interface {
	// Protocol 回報這個 adapter 服務的協定名（evm、solana），對應 intent.Chain 冒號前面那一段。
	Protocol() string
	// Sequencer 回報這條鏈發號的做法。同一個 adapter 永遠回同一個 sequencer：
	// 發號的狀態（下一個號、有沒有空缺）只能有一份，兩份就是兩條互相撞號的線。
	// 不用發號的鏈回 txseq.Unordered，呼叫端照常取號、收尾，不用寫 if（見 txseq 的 package 註解）。
	Sequencer() txseq.Sequencer
	// Finality 回報這條鏈的不可逆規則，listener 拿它判 confirming 的 intent。
	Finality() finality.Policy
	// BatchLimits 回報這條鏈一筆交易裝得下多少項付款，bulk.Pack 拿它切撥款名單。
	BatchLimits() bulk.Limits
}

// Replacer 是「同一個號上可以再送一筆更貴的交易」的那類鏈才實作的能力，回答替換的規則：
// 起價多少、每次加多少、天花板在哪、最多廣播幾次。
//
// 它不進 Adapter，因為這一題 Solana 與 SUI 沒有答案：交易由簽名識別，改了出價就是另一筆交易，
// 兩筆都可能上鏈（見 txfee 的 package 註解）。沒有答案的問題硬要回答，答案就只能是謊話：
// 回一個零值 Policy，呼叫端拿到的是「起價零、加價零」這種看起來像設定錯誤的東西。
// 行為那一半的對應介面是 relayer.ReplacingSender；實作了這裡就該實作那裡，反過來也一樣。
type Replacer interface {
	ReplacementPolicy() txfee.Policy
}

// Replacement 問一個 adapter 能不能替換。第二個回傳值是 false 時，卡住的交易只有一條路：
// 原封不動重送同一筆簽好的交易，而那件事不需要 relayer 介入。
func Replacement(a Adapter) (txfee.Policy, bool) {
	r, ok := a.(Replacer)
	if !ok {
		return txfee.Policy{}, false
	}
	return r.ReplacementPolicy(), true
}

package chain

import (
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// Solana 是 solana 協定的 adapter，也是 Adapter 介面的第一塊試金石：它四題都答得出來，
// 但其中一題的答案是「不需要」。交易帶 recent blockhash、不帶帳戶層級的序號，所以 Sequencer
// 回 txseq.Unordered：呼叫端照常取號、收尾，只是拿到的位置是空的。
//
// 它刻意不實作 Replacer。Solana 的交易由簽名識別，改了出價就是另一份簽名、另一筆交易，
// 兩筆都可能上鏈（https://solana.com/docs/advanced/confirmation）；卡住的時候唯一安全的動作
// 是原封不動重送同一筆簽好的交易。「不能替換」不是一個可以用空實作矇混的答案：替換的空實作
// 等於把一筆可能重複付款的交易照樣送出去。
//
// 什麼時候用空實作、什麼時候乾脆不實作，分界線就在這兩題之間：呼叫端照常走完流程也不會出事的，
// 用空實作頂上（Unordered）；呼叫這件事本身就危險的，用能力介面擋在型別這一層（Replacer）。
type Solana struct{}

// NewSolana 建立 solana 的 adapter。它沒有狀態：不發號的鏈沒有需要活一輩子的東西。
func NewSolana() *Solana {
	return &Solana{}
}

// Protocol 實作 Adapter。
func (s *Solana) Protocol() string { return "solana" }

// Sequencer 實作 Adapter：不發號，回 Unordered，讓 relayer 那一側不用寫 if。
func (s *Solana) Sequencer() txseq.Sequencer { return txseq.Unordered{} }

// Finality 實作 Adapter：拿的是 finality.Defaults() 裡 solana 那一條，不自己抄一份。
func (s *Solana) Finality() finality.Policy { return finality.Defaults()["solana"] }

// BatchLimits 實作 Adapter：拿的是 bulk.Defaults() 裡 solana 那一條，兩條互相獨立的規則
// （bytes 與 accounts）都在裡面，出處跟著它走。
func (s *Solana) BatchLimits() bulk.Limits { return bulk.Defaults()["solana"] }

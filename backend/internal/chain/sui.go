package chain

import (
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// SUI 是 sui 協定的 adapter，四條鏈裡最後接進來的一條。四題它都答得出來，而且每一題的答案
// 都能在前三條鏈裡找到同款：不發號（跟 Solana 一樣，交易指名的是 object 的版本，送出當下從
// 鏈上讀）、等 checkpoint 才算不可逆、一批就是一個 PTB 裝得下幾個 command。
//
// 差別在「錢」這個字指的東西。前三條鏈的錢是帳戶裡的一個數字，合約改數字就是搬錢；這條鏈的錢
// 是一顆一顆的 Coin object，誰能動它寫在 object 的 owner 上，由執行期而不是由合約檢查
// （https://docs.sui.io/concepts/object-ownership）。所以結算模組拿不到 allowance，payer 的 coin
// 只有 payer 簽的交易帶得動：鏈上那一半見 contracts/sui/settlement。
//
// 它不實作 Replacer，而且是四條鏈裡最不能實作的一條：交易指名了 owned object 的某個版本，同一個
// 版本被兩筆交易用到就是 equivocation，「The effects of this type of equivocation can lock the
// objects your code interacts with until the end of the current epoch」
// （https://docs.sui.io/guides/developer/sui-101/avoid-equivocation）。卡住的時候唯一安全的動作
// 跟 Solana 一樣是原封不動重送同一筆簽好的交易。
type SUI struct{}

// NewSUI 建立 sui 的 adapter。它沒有狀態：不發號的鏈沒有需要活一輩子的東西。
func NewSUI() *SUI {
	return &SUI{}
}

// Protocol 實作 Adapter。
func (s *SUI) Protocol() string { return "sui" }

// Sequencer 實作 Adapter：不發號，回 Unordered。要注意的是「不發號」講的是鏈沒有帳戶層級的序號，
// 不代表同一個帳戶可以同時送好幾筆：兩筆交易指名同一顆 owned object 的同一個版本會互相撞掉，
// 而撞掉的下場是那顆 object 鎖到 epoch 結束。所以這條鏈的並行單位是 object，不是帳戶。
func (s *SUI) Sequencer() txseq.Sequencer { return txseq.Unordered{} }

// Finality 實作 Adapter：拿的是 finality.Defaults() 裡 sui 那一條，不自己抄一份。
func (s *SUI) Finality() finality.Policy { return finality.Defaults()["sui"] }

// BatchLimits 實作 Adapter：拿的是 bulk.Defaults() 裡 sui 那一條，三條互相獨立的規則
// （commands、bytes、objects）都在裡面，出處跟著它走。
func (s *SUI) BatchLimits() bulk.Limits { return bulk.Defaults()["sui"] }

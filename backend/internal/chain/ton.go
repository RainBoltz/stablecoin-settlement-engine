package chain

import (
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// TON 是 ton 協定的 adapter。四題它都答得出來，而且每一題的答案都跟前兩條鏈長得像：
// 自己發號（錢包合約裡的 seqno）、等 masterchain 引用才算不可逆、一筆 external message 裝得下多少則 message。
// 差別不在答案，在答案指的東西：前兩條鏈的「一筆交易」就是一筆付款，TON 上 relayer 送出去的
// 是一則 external message，它讓錢包合約跑一筆交易，而那筆交易只是把 N 則付款 message「送出去」；
// 錢有沒有動，發生在之後另外幾個合約各自的交易裡（見 tonmsg.go）。
//
// 它不實作 Replacer。錢包合約只收剛好等於當前 seqno 的那一則 external message，一則收下就加一
// （https://docs.ton.org/contracts/standard/wallets/how-it-works），external message 也沒有「出價」
// 這種欄位可以加：一則簽好的 message 在 valid_until 之前可以原封不動重送，過期就重簽一則
// 新的，兩者都不是替換。所以卡住的時候唯一的動作跟 Solana 一樣是重送同一段 bytes。
type TON struct {
	seq *txseq.Counter
}

// NewTON 建立 ton 的 adapter。sequencer 在這裡建、跟著 adapter 活一輩子，理由跟 EVM 一樣：
// 一條鏈的發號線在一個 process 裡只能有一條。接真的鏈時要先對每個發送錢包跑 seqno 的
// get method 做一次 Sync。
func NewTON() *TON {
	return &TON{seq: txseq.NewCounter()}
}

// Protocol 實作 Adapter。
func (t *TON) Protocol() string { return "ton" }

// Sequencer 實作 Adapter：seqno 是發送方自己往上加的，所以跟 EVM 一樣是一個真的發號器。
// 跟 nonce 不同的是跳號沒有成本：seqno 對不上的 message 錢包直接不收，不會卡住後面；
// 所以 SentUnknown 留下的空缺在 valid_until 過了之後一次 Sync 就能收掉。
func (t *TON) Sequencer() txseq.Sequencer { return t.seq }

// Finality 實作 Adapter：拿的是 finality.Defaults() 裡 ton 那一條，不自己抄一份。
func (t *TON) Finality() finality.Policy { return finality.Defaults()["ton"] }

// BatchLimits 實作 Adapter：拿的是 bulk.Defaults() 裡 ton 那一條，出處跟著它走。
func (t *TON) BatchLimits() bulk.Limits { return bulk.Defaults()["ton"] }

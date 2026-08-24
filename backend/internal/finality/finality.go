// Package finality 判一件事：鏈上那筆交易，現在可以當成不可逆了嗎。
//
// relayer 把 intent 推到 confirming 的意思只有「有一筆交易進區塊了」，但進區塊不等於結束：EVM 的區塊在 finality 之前
// 可以被換掉、Solana 的 block 在 finalized 之前可以被丟掉、TON 的 shardchain block 要等 masterchain 引用、SUI 的交易要進
// checkpoint。四條鏈各自給了一個不同的東西來說「這筆從此不會消失」，而這個 package 的工作是把那四個東西收成一個
// Verdict，讓 listener 不用知道自己在看哪條鏈。
//
// 四條鏈的「不可逆」各是什麼（這一段是 Observation.Final 那一欄由 chain adapter 填的依據）：
//
//   - EVM：JSON-RPC 的 block tag 有 latest / safe / finalized 三級
//     （https://ethereum.org/en/developers/docs/apis/json-rpc/#default-block），finalized 是 PoS 兩個 epoch、
//     三分之二以上的 stake 投過票的區塊（https://ethereum.org/en/developers/docs/consensus-mechanisms/pos/#finality）。
//     交易所在的區塊高度 <= finalized 的高度，就是 Final。
//   - Solana：commitment 有 processed / confirmed / finalized 三級（https://solana.com/docs/rpc），
//     finalized 是「maximum lockout」的那一級，confirmed 只代表超過三分之二的 stake 投過票。交易的 confirmationStatus
//     到 finalized 就是 Final。
//   - TON：「Once a transaction from a shardchain appears in a masterchain block, it becomes irreversible」
//     （https://docs.ton.org/blockchain-basics/payments/overview）。交易所在的 shard block 被某個 masterchain block 引用，就是 Final。
//   - SUI：「Inclusion in a certified checkpoint is itself proof of finality」
//     （https://docs.sui.io/develop/transactions/transaction-lifecycle）。交易身上有 checkpoint 編號，就是 Final。
//
// 沒有一個叫「N 個 confirmations」。數深度是 PoW 留下來的習慣，在上面四條鏈裡只有 EVM 還能拿它當 finalized 之前的代替品；
// 而它仍然有用，因為 finalized 要等十幾分鐘，一個收款的人未必願意等那麼久。Circle 的 CCTP 就公開分成兩級
// （https://developers.circle.com/cctp/concepts/finality-and-block-confirmations）：Fast Transfer 在 Ethereum 上等 2 個區塊，
// Standard Transfer 等大約 65 個區塊、十幾二十分鐘。所以 Policy 有兩個旋鈕：要不要等鏈自己的 marker、要不要再壓幾個區塊。
// 預設全部等 marker；把 marker 關掉、只數深度，是「我願意承擔 reorg 的風險換時間」的商業決定，不是預設。
//
// 它跟 txfee、txfail 一樣是純函式：不碰鏈、不碰資料庫、不看時鐘。呼叫端把「鏈回了什麼、intent 進 confirming 多久了」
// 交給它，它回一個 Verdict；判完之後 intent 要推到哪一格、帳要怎麼記，是 listener 的事（見 internal/listener）。
//
// 本 package 為本系列從零設計，只取公開設計裡需要的那部分。
package finality

import (
	"fmt"
	"time"
)

// Observation 是鏈對「這筆交易現在怎麼樣」的一次回答，由 chain adapter 讀鏈之後填。
//
// 欄位刻意用鏈中立的名字。「高度」在四條鏈上各是不同的東西，adapter 負責翻譯，這裡只比大小：
//
//	鏈      Height              Head                Final
//	EVM     區塊高度             latest 的高度        Height <= finalized tag 的高度
//	Solana  slot                confirmed 的 slot    confirmationStatus == finalized
//	TON     引用它的 masterchain seqno   最新 masterchain seqno   已被 masterchain block 引用
//	SUI     checkpoint 編號      最新 checkpoint      已在 certified checkpoint 裡
type Observation struct {
	// Included：進了某個區塊（或被執行了）。false 代表節點找不到它在任何區塊裡：可能還在 mempool、
	// 可能被 reorg 吐回來、可能已經被丟掉。這三種對 listener 來說是同一件事——它不在鏈上。
	Included bool
	// Height 是它所在的高度；Included 為 false 時沒有意義。
	Height uint64
	// Head 是節點目前看到的最新高度。深度就是 Head - Height + 1。
	Head uint64
	// Final：鏈自己說這筆已經不可逆。每條鏈的依據不一樣，見 package 註解。
	Final bool
	// Succeeded：執行成功。EVM 是 receipt 的 status、Solana 是 meta.err 為空、SUI 是 effects 的 status、
	// TON 是 transaction 沒有 aborted。false 代表交易在鏈上、gas 燒了、錢沒動。
	Succeeded bool
}

// Depth 是它上面壓了幾個區塊，含自己。節點落後（Head 比 Height 小）時回 0，不會繞回去變成一個很大的數。
func (o Observation) Depth() uint64 {
	if !o.Included || o.Head < o.Height {
		return 0
	}
	return o.Head - o.Height + 1
}

// Policy 是一條鏈的不可逆規則，兩個旋鈕加一個逾時。
type Policy struct {
	// Marker 是這條鏈自己的「不可逆」叫什麼名字（finalized、masterchain、checkpoint），只用來印理由。
	Marker string
	// RequireMarker：要等鏈自己說不可逆（Observation.Final）。預設開；關掉就只剩深度。
	RequireMarker bool
	// Confirmations：至少要有幾個區塊壓在上面（含自己）才算，0 代表不看深度。
	// 這是 Circle 那種「Fast Transfer」的旋鈕：Ethereum 上等 2 個區塊就放行。它可以跟 RequireMarker 疊加，
	// 兩個都開就是兩個都要滿足。
	Confirmations uint64
	// LostAfter：intent 進 confirming 之後多久還沒在任何區塊裡，就當成被吐回來或被丟掉，交回 relayer。
	// 要比「一筆交易正常從 mempool 到進區塊」的時間長很多，不然 RPC 落後一點就會把一筆好好的付款退回 settling。
	// 0 代表永遠等。
	LostAfter time.Duration
}

// Defaults 是四條鏈的預設規則，以協定名為 key（intent.Chain 冒號前面那一段）。四條都等鏈自己的 marker、不數深度。
//
// LostAfter 的差別來自各鏈「送出去但沒進區塊」能拖多久：EVM 的交易可以在 mempool 躺很久，給 5 分鐘（跟 relayer 的
// StuckAfter 一樣長）；Solana 的 blockhash 60 到 90 秒就過期（https://solana.com/docs/advanced/confirmation），
// TON 的 external message 帶 valid_until、SUI 的交易不進 checkpoint 就不存在，這三條給 2 分鐘。數字都是這裡設的。
func Defaults() map[string]Policy {
	return map[string]Policy{
		"evm":    {Marker: "finalized", RequireMarker: true, LostAfter: 5 * time.Minute},
		"solana": {Marker: "finalized", RequireMarker: true, LostAfter: 2 * time.Minute},
		"ton":    {Marker: "masterchain", RequireMarker: true, LostAfter: 2 * time.Minute},
		"sui":    {Marker: "checkpoint", RequireMarker: true, LostAfter: 2 * time.Minute},
	}
}

// String 印成一行給人看：等什麼、等幾個、多久算丟了。
func (p Policy) String() string {
	s := "final when "
	switch {
	case p.RequireMarker && p.Confirmations > 0:
		s += fmt.Sprintf("%s and %d confirmations", p.Marker, p.Confirmations)
	case p.RequireMarker:
		s += p.Marker
	case p.Confirmations > 0:
		s += fmt.Sprintf("%d confirmations", p.Confirmations)
	default:
		s += "included"
	}
	if p.LostAfter > 0 {
		s += fmt.Sprintf("; lost after %s", p.LostAfter)
	}
	return s
}

// Kind 是 Judge 的四種結果。只有 pending 是「再等」，其他三種 listener 都要動 intent。
type Kind string

const (
	// KindPending：還不能算數。在 mempool、深度不夠、marker 還沒到，都是這一種。
	KindPending Kind = "pending"
	// KindFinal：不可逆，而且執行成功。listener 接下來要看的是錢有沒有真的動（見 listener）。
	KindFinal Kind = "final"
	// KindFailed：不可逆，但執行失敗（EVM 的 revert）。gas 燒了、錢沒動，而且這件事從此不會變。
	KindFailed Kind = "failed"
	// KindLost：太久沒有在任何區塊裡，當成被吐回來或被丟掉。這筆交易的結局要交回 relayer 重新處理。
	KindLost Kind = "lost"
)

// Verdict 是 Judge 的結果。Reason 寫給人看，會一路傳到 listener 的 Report 與 intent 的理由欄。
type Verdict struct {
	Kind   Kind
	Reason string
}

// String 用固定格式印一個判決，Example 會直接貼這個格式。
func (v Verdict) String() string { return fmt.Sprintf("%-8s %s", v.Kind, v.Reason) }

// Judge 是整個 package 的決策樹，順序是刻意排的：
//
//  1. 不在任何區塊裡：年輕就等，超過 LostAfter 就判 lost。這條擺最前面，因為後面每一條都以「它在某個區塊裡」為前提。
//  2. 深度不夠：等。
//  3. 鏈自己還沒說不可逆：等。深度擺在 marker 前面只是因為它便宜，兩個都開就兩個都要過。
//  4. 到這裡它已經不可逆了，才看執行成功還是失敗。
//
// 第 4 條擺最後是這棵樹最重要的決定：一筆 revert 的交易在 finalized 之前跟一筆成功的交易一樣可以被 reorg 換掉，
// 換掉之後同一筆交易可能在另一個區塊裡成功。「鏈上說失敗」跟「鏈上說成功」要用同一把尺量，都要等它不可逆才能信；
// 早一步把它送去人工介入，人看到的會是一張可能翻盤的照片。
func (p Policy) Judge(obs Observation, age time.Duration) Verdict {
	if !obs.Included {
		if p.LostAfter > 0 && age >= p.LostAfter {
			return Verdict{Kind: KindLost, Reason: fmt.Sprintf("not in any block for %s; dropped or reorged out", age)}
		}
		return Verdict{Kind: KindPending, Reason: "not in any block yet"}
	}
	depth := obs.Depth()
	if p.Confirmations > 0 && depth < p.Confirmations {
		return Verdict{Kind: KindPending, Reason: fmt.Sprintf("included at %d, %d of %d confirmations", obs.Height, depth, p.Confirmations)}
	}
	if p.RequireMarker && !obs.Final {
		return Verdict{Kind: KindPending, Reason: fmt.Sprintf("included at %d, %d deep, not yet %s", obs.Height, depth, p.Marker)}
	}
	// 理由裡寫的是「憑什麼算不可逆」：等 marker 的寫 marker，只數深度的寫深度。人看理由就知道這條鏈用的是哪一種尺。
	where := fmt.Sprintf("%d confirmations at %d", depth, obs.Height)
	if p.RequireMarker {
		where = fmt.Sprintf("%s at %d, %d deep", p.Marker, obs.Height, depth)
	}
	if !obs.Succeeded {
		return Verdict{Kind: KindFailed, Reason: where + " but the execution failed; gas burned, nothing moved"}
	}
	return Verdict{Kind: KindFinal, Reason: where}
}

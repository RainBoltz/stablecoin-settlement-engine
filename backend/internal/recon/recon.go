// Package recon 是對帳引擎：把鏈上一段已經不可逆的區塊裡所有動過的錢，跟鏈下的 intent 與 ledger 對一遍。
//
// listener 已經會看鏈了，為什麼還要一個對帳引擎：listener 是拿著 intent 身上的 tx hash 去問「這一筆結束了沒」，
// 一次一筆、只看我們自己送出去的那筆交易。有三種錢它從頭到尾看不到：
//
//   - relayer 送出交易之後、把 tx hash 寫回 intent 之前掛掉，intent 停在 settling 沒有 tx hash，
//     鏈上那筆交易照樣進區塊、照樣帶著我們的 ref。listener 手上沒有 hash 可問。
//   - 一筆帶著我們 ref 的轉帳，但不是我們送出去的：同一個 ref 出現在第二筆交易裡（Solana 上原封不動重送的
//     兩筆都可能上鏈，見 txseq 的 package 註解）、或是一筆我們早就宣告 failed 的付款，錢卻動了。
//   - 打到 merchant 地址、沒有帶 ref 的轉帳：付款人繞過結算合約直接對 token 合約 transfer，鏈上沒有地方放 ref。
//
// 對帳引擎從另一頭出發：不是拿一個 hash 去問，而是把一段區塊裡所有跟我們有關的轉帳全部撈出來，
// 每一筆拿 ref 去 intent store 找主人。複式記帳的原理擺到鏈上鏈下之間就是：每一個 ref 在兩邊都該剛好出現一次，
// 少一邊或多一邊就是一筆 Finding。這個形狀跟 Modern Treasury 的 Expected Payment 對 Transaction 是同一件事
// （https://docs.moderntreasury.com/payments/docs/managing-externally-originated-payments：
// 「Create an Expected Payment to represent and reconcile a payment that you expect to receive or send」），
// 我們的 intent 就是 expected payment，差別是我們有一把精確的鍵（ref），不用靠金額與日期的範圍去猜。
//
// 三條設計紀律：
//
//   - 只對不可逆的那一段。window 的上界是鏈自己說 finalized 的高度（見 internal/finality），cursor 只走到那裡。
//     Finding 是要叫人來看的，叫人之前得確定它不會自己消失；代價是 EVM 上晚十幾分鐘才發現問題。
//   - 對帳引擎能做的只有兩種事：補證據、叫人。它可以把一筆鏈上看得到交易的 settling intent 推到 confirming
//     （轉移表上這一列本來就寫著 listener），然後交給 listener.Check 判；它不自己宣告 settled 或 failed，
//     判斷只住在一個地方（finality.Judge 與 listener），不然對帳引擎就是第二個 listener，帶著自己的 bug。
//   - 重跑必須是 no-op。對帳是排程跑的，排程會重疊、程序會重啟，同一段 window 對兩次要得到同一份 Finding、
//     帳本不會多任何紀錄：Enqueue 對同 ID 是 no-op、Check 對不在 confirming 的 intent 是 no-op、
//     Apply 重放是 no-op。
//
// 對帳引擎順便做一件跟鏈無關的事：每一次 Run 先掃一遍鏈下「還沒有人推它」的 intent。authorized 與 settling 的
// 丟一份 settle job 進 queue（Enqueue 冪等，queue 還記得的就是 no-op；已經停在 dlq 裡等人的不碰），
// confirming 的交給 listener.Check。這就是 Brandur 那篇 Transactionally Staged Job Drains 裡的 enqueuer
// （https://brandur.org/job-drain：「Jobs are only removed after they're successfully transmitted to the queue」），
// 只是我們沒有另開一張 staged jobs 表：intent store 本身就是那張表，state 是 authorized 就是一份還沒進 queue 的 job。
//
// 還沒做的：cursor 與 Finding 都只在記憶體裡，資料庫版要把 cursor 跟 Finding 一起寫進同一個 transaction；
// 「settled 了但鏈上從來沒出現過那筆交易」這個方向今天沒有查，它需要記住每一筆 post 有沒有被哪個 window 對到過。
//
// 本 package 為本系列從零設計，只取公開設計裡需要的那部分。
package recon

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// Transfer 是鏈上一筆動了錢的紀錄，由 chain adapter 讀鏈之後填：哪一筆交易、在哪個高度、帶著哪個 ref、
// 哪顆 token、誰付給誰多少。
//
// Ref 是零值的話代表這筆轉帳沒有帶 ref：EVM 上直接對 token 合約 transfer 就是這樣，沒有地方放。
// 這種轉帳 adapter 只在它打到我們認識的 merchant 地址時才回報。
type Transfer struct {
	TxHash string
	Height uint64
	Ref    paymentref.Ref
	Token  string
	From   string
	To     string
	Amount *big.Int
}

// Source 是 chain adapter 讀鏈的另一半。listener 的 Watcher 拿一個 tx hash 問一筆；Source 拿一段高度問全部。
//
// EVM 上這就是 eth_getLogs 帶 fromBlock 與 toBlock，address 是結算合約、topic 是帶 ref 的那個 event
// （https://ethereum.org/en/developers/docs/apis/json-rpc/#eth-getlogs）；Solana 是掃 slot 裡帶 memo 的交易，
// TON 是掃 jetton wallet 收到的 transfer notification，SUI 是依 event type 查 checkpoint。今天只有測試用的 fake。
type Source interface {
	// Finalized 回報這條鏈目前最高的不可逆高度：EVM 的 finalized tag、Solana 的 finalized slot、
	// TON 最新的 masterchain seqno、SUI 最新的 certified checkpoint。cursor 只走到這裡。
	Finalized(ctx context.Context) (uint64, error)
	// Transfers 回報 [from, to] 這段高度裡所有跟我們有關的轉帳：帶著 ref 的，以及打到我們認識的 merchant 地址
	// 但沒帶 ref 的。順序不要求，Engine 會照高度與 tx hash 排。
	Transfers(ctx context.Context, from, to uint64) ([]Transfer, error)
}

// Kind 是 Finding 的五種。每一種都是「鏈上鏈下對不上」的一種形狀，而且都不是對帳引擎自己能收尾的：
// 它能做的是把對不上的那一筆連著理由列出來。
type Kind string

const (
	// KindUnknownRef：帶著一個 intent store 裡找不到的 ref。不是我們的付款、或是 intent store 掉了資料，都是人要看的。
	KindUnknownRef Kind = "unknown_ref"
	// KindUnreferenced：打到 merchant 地址、沒有帶 ref。交易所對「沒填 memo 的入金」的處理是不自動入帳、人工找回
	// （https://support.kraken.com/articles/crypto-assets-deposit-recovery：「Omitting or entering an incorrect tag/memo
	// can prevent your deposit from being credited to your account」），我們也一樣：這筆錢不對應任何 intent，
	// 帳本上沒有它的 hold，listener 不會替它記 post。
	KindUnreferenced Kind = "unreferenced"
	// KindPaidTwice：同一個 ref 在第二筆交易裡又動了一次錢。這是整個系列在防的那件事，走到這裡代表前面某一層
	// 沒守住（或者有人拿我們的 ref 自己送了一筆）。帳本不會跟著記第二筆 post，因為一筆 hold 只能收尾一次。
	KindPaidTwice Kind = "paid_twice"
	// KindUnexpected：ref 找得到 intent，但那筆 intent 已經不在等錢：failed、canceled、needs_review、
	// 或還沒 authorized；或者轉帳的 token、付款人、收款人跟 intent 上寫的不一樣。錢動了、鏈下卻沒有人在等它，
	// 對 needs_review 來說這是 operator 收尾要的證據，對 failed 來說這是一筆我們告訴 merchant 沒付、其實付了的錢。
	KindUnexpected Kind = "unexpected"
	// KindMismatch：intent 已經 settled、post 也指著這筆交易，但帳上 post 的金額跟鏈上轉帳的金額不一樣。
	// listener 記 post 之前比過金額，所以走到這裡多半是 adapter 的 bug 或帳本被動過（ledger.Verify 抓得到後者）。
	KindMismatch Kind = "mismatch"
)

// Finding 是一筆對不上的錢：哪一種、鏈上那筆轉帳、對到的 intent（可能沒有）、給人看的一句話。
type Finding struct {
	Kind     Kind
	Transfer Transfer
	IntentID string
	Detail   string
}

// String 用固定格式印一行：種類、tx hash、細節。Example 直接貼這個格式。
func (f Finding) String() string {
	return fmt.Sprintf("%-12s tx %-6s %s", f.Kind, f.Transfer.TxHash, f.Detail)
}

// Match 是一筆對得上的錢，或是對帳引擎替它補了證據之後對上的：鏈上那筆轉帳、它的 intent、發生了什麼。
type Match struct {
	Transfer Transfer
	IntentID string
	Action   string
}

// String 用固定格式印一行：tx hash、intent、發生了什麼。
func (m Match) String() string {
	return fmt.Sprintf("%-6s %-8s %s", m.Transfer.TxHash, m.IntentID, m.Action)
}

// Sweep 是鏈下掃描碰到的一筆 intent：掃到它時停在哪一格、對帳引擎對它做了什麼。
type Sweep struct {
	IntentID string
	State    intent.State
	Action   string
}

// String 用固定格式印一行：intent、掃到時的狀態、做了什麼。
func (s Sweep) String() string {
	return fmt.Sprintf("%-8s %-12s %s", s.IntentID, s.State, s.Action)
}

// Report 是 Run 一次的結果。From 大於 To 代表這一次沒有新的 finalized 區塊可對，只做了鏈下掃描。
type Report struct {
	Chain    string
	From, To uint64
	Sweeps   []Sweep
	Matches  []Match
	Findings []Finding
}

// shortHex 把 0x 開頭的長 hex 縮成前 10 碼加 …，跟 ledger.Entry.String 印 ref 的方式一樣。
func shortHex(s string) string {
	if len(s) > 12 && strings.HasPrefix(s, "0x") {
		return s[:10] + "…"
	}
	return s
}

// shortAddr 把地址縮成 0x1234…abcd，跟 ledger 印科目的方式一樣。
func shortAddr(a string) string {
	if len(a) != 42 || !strings.HasPrefix(a, "0x") {
		return a
	}
	return a[:6] + "…" + a[38:]
}

// Package bulk 把一份撥款名單切成「一筆交易裝得下」的幾批。
//
// 結算端的批次入口只負責照著跑，「一批該放幾項」從來不是鏈上回答得了的問題：
// 裝不裝得下是鏈的規則，而每條鏈拿來限制一筆交易的根本不是同一種資源。EVM 數的是 gas，
// 一筆交易最多用掉一個區塊的量；Solana 數的是這筆交易序列化之後有多長、以及它列出了幾個帳戶。
//
// 兩條鏈切出來的批也不是同一種形狀。EVM 一批是「塞得下就多塞一項」的貪心切法；
// Solana 的批對齊在 merkle 樹的區塊上：payer 簽過的 root 蓋住整份名單，每一批帶一份
// 「區塊走回 root」的證明上鏈，所以批的大小固定是 Align，切在對齊的邊界上，證明才共用得起來。
// 還有 merchant 的 token 帳戶要先開好這件事：它不再讓某一項變貴，而是整個搬到付款之前，
// 變成一段自己的 prepare batch，這樣付款批的長度全部一樣好算，rent 也在送錢之前就備齊。
//
// 所以這個 package 只做一件事：拿一份名單與一條鏈的限制，切出幾批各自送得出去的交易，
// 順便回報送出去之前要先準備什麼（要先開幾個帳戶、要先墊多少 rent、樹有幾層）。
// 它不建樹、不組交易、不簽名、不認識 RPC，也不知道 relayer 存在。真正要送之前還是得把
// 交易序列化一次算出真正的長度；這裡算的是「不要先組出一批一定送不出去的名單」，
// 所以每一條規則寧可高估、不低估。
//
// 本 package 為本系列從零設計，只取公開設計裡需要的那部分。
package bulk

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// Payout 是撥款名單上的一項。欄位跟結算合約那個同名的 struct 對得起來：
// 一項就是一筆完整的付款，有自己的 merchant、自己的金額、自己的 ref，批次不給付款第二種身分。
type Payout struct {
	Ref      paymentref.Ref
	Merchant string
	Amount   *big.Int

	// NewTokenAccount 是這一項在鏈上多出來的一件工作：這個 merchant 還沒有地方收這顆 token。
	// EVM 上沒有這件事，一個地址第一次拿到一顆 ERC-20 只是在 token 合約的 mapping 上多一筆紀錄；
	// Solana 上得先有人替他開一個帳戶，而且開帳戶要先墊 rent。這件事不改變付款那一批的價錢：
	// 開帳戶整個發生在付款之前的 prepare batch 裡，付款批裡每一項一樣貴。
	//
	// 這個欄位要先去鏈上查過才填得出來，鏈上程式永遠不會知道，這也是為什麼組批住在鏈下。
	NewTokenAccount bool
}

// Batch 是一筆交易送得出去的那幾項，以及它把每一種資源用掉多少。
type Batch struct {
	// Index 從 1 開始，只拿來印報告與寫錯誤訊息。prepare batch 與付款 batch 各自從 1 起算。
	Index int
	Items []Payout
	Used  []Usage
	// Prep 標記這一批是「送錢之前先開帳戶」的 prepare batch：
	// 它的 Items 是需要開帳戶的那幾項，錢在這一批裡一毛都不會動。
	Prep bool
	// NewAccounts 是這一批裡面要先開帳戶的 merchant 數。
	NewAccounts int
}

// Usage 是一批在某一種資源上的用量與上限。
type Usage struct {
	Unit string
	Used uint64
	Cap  uint64
}

// Plan 是整份名單切完的結果。付款批之間沒有先後關係：一批一筆交易，能不能並行送出去是
// relayer 的事；唯一的先後在 Prepare 與 Batches 之間，帳戶沒開好，付款批會在轉帳那一步失敗。
type Plan struct {
	Chain   string
	Payouts int
	// Prepare 是「送錢之前先做掉」的 batch：替還沒有 token 帳戶的 merchant 開帳戶。
	// 沒有 prepare 階段的鏈（EVM）這裡恆為空。
	Prepare []Batch
	Batches []Batch
	// Levels 與 ProofHashes 描述蓋住這份名單的 merkle 樹：名單墊到 2 的冪次之後樹有幾層、
	// 每一批要帶幾個雜湊的證明。切貪心批的鏈（Align 為 0）兩個都是 0。
	Levels      int
	ProofHashes int
	// NewAccounts 與 Rent 是「送這批之前要先準備什麼」：
	// 有幾個 merchant 要先開帳戶，以及那些帳戶總共要墊多少 rent。
	NewAccounts int
	Rent        uint64
	RentUnit    string
}

// ErrEmptyRun：名單是空的。空批次在結算端會被擋下來，鏈下沒有理由先組一份出來。
var ErrEmptyRun = errors.New("bulk: the payout run is empty")

// ErrItemTooLarge：某一項自己一個人就塞不進一筆交易。走到這裡多半是設定寫錯了，
// 或這條鏈的限制真的太緊；再切細也救不了它，所以直接讓呼叫端知道。
var ErrItemTooLarge = errors.New("bulk: a single payout does not fit in one transaction")

// ErrBlockTooLarge：對齊區塊自己（含證明）就超過某一條上限。Align 與 Rules 是一組
// 要一起調的設定，這個錯只會在設定彼此矛盾時出現，所以一樣直接回報而不是硬切。
var ErrBlockTooLarge = errors.New("bulk: an aligned block does not fit in one transaction")

// String 印一份計畫的總結行。格式固定，Example 與文章會直接貼這一行。
func (p Plan) String() string {
	return fmt.Sprintf("plan    %-8s %d payouts  %d %s  %d new accounts  rent %s %s",
		p.Chain, p.Payouts, len(p.Batches), plural(len(p.Batches), "batch", "batches"),
		p.NewAccounts, thousands(p.Rent), p.RentUnit)
}

// TreeString 印蓋住這份名單的樹長什麼樣。沒有樹的計畫（Align 為 0 的鏈）回空字串，
// 呼叫端自己決定要不要印那一行。
func (p Plan) TreeString() string {
	if p.Levels == 0 {
		return ""
	}
	return fmt.Sprintf("tree    %d leaves  depth %d  proof %d %s per batch",
		1<<p.Levels, p.Levels, p.ProofHashes, plural(p.ProofHashes, "hash", "hashes"))
}

// String 印一批的用量。每一種資源一格 "unit used/cap"，資源有幾種由那條鏈決定，
// 同一份計畫的同一個階段裡不會變。prepare batch 印的名目是 accounts：它開帳戶，不搬錢。
func (b Batch) String() string {
	parts := make([]string, 0, len(b.Used))
	for _, u := range b.Used {
		parts = append(parts, fmt.Sprintf("%s %s/%s", u.Unit, thousands(u.Used), thousands(u.Cap)))
	}
	if b.Prep {
		return fmt.Sprintf("prep    #%-7d %d accounts  %s", b.Index, len(b.Items), strings.Join(parts, "  "))
	}
	return fmt.Sprintf("batch   #%-7d %d items  %s", b.Index, len(b.Items), strings.Join(parts, "  "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// thousands 每三位加一個逗號。輸出是給人看的，數字大到七八位時沒有逗號很難比大小。
func thousands(n uint64) string {
	s := strconv.FormatUint(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

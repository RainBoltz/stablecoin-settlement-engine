// Package bulk 把一份撥款名單切成「一筆交易裝得下」的幾批。
//
// 結算合約的批次入口只負責照著跑迴圈，「一批該放幾項」從來不是它回答得了的問題：
// 裝不裝得下是鏈的規則，而每條鏈拿來限制一筆交易的根本不是同一種資源。EVM 數的是 gas，
// 一筆交易最多用掉一個區塊的量；Solana 數的是這筆交易序列化之後有多長、以及它列出了幾個帳戶，
// 而「merchant 有沒有地方收這顆 token」還會回頭改變前面兩個數字。
//
// 所以這個 package 只做一件事：拿一份名單與一條鏈的限制，切出幾批各自送得出去的付款，
// 順便回報送出去之前要先準備多少錢。它不組交易、不簽名、不認識 RPC，也不知道 relayer 存在。
// 真正要送之前還是得把交易序列化一次算出真正的長度；這裡算的是「不要先組出一批一定送不出去的名單」，
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
	// Solana 上得先有人替他開一個帳戶，而且開帳戶要先墊 rent，所以這一項比別項貴。
	//
	// 這個欄位要先去鏈上查過才填得出來，合約永遠不會知道，這也是為什麼組批住在鏈下。
	NewTokenAccount bool
}

// Batch 是一筆交易送得出去的那幾項，以及它把每一種資源用掉多少。
type Batch struct {
	// Index 從 1 開始，只拿來印報告與寫錯誤訊息。
	Index int
	Items []Payout
	Used  []Usage
	// NewAccounts 是這一批裡面要先開帳戶的 merchant 數。
	NewAccounts int
}

// Usage 是一批在某一種資源上的用量與上限。
type Usage struct {
	Unit string
	Used uint64
	Cap  uint64
}

// Plan 是整份名單切完的結果。批次之間沒有先後關係：一批一筆交易，能不能並行送出去是 relayer 的事。
type Plan struct {
	Chain   string
	Payouts int
	Batches []Batch
	// NewAccounts 與 Rent 是「送這批之前要先準備什麼」：
	// 有幾個 merchant 要先開帳戶，以及那些帳戶總共要墊多少 rent。
	NewAccounts int
	Rent        uint64
	RentUnit    string
}

// ErrEmptyRun：名單是空的。空批次在合約那一側會被擋下來，鏈下沒有理由先組一份出來。
var ErrEmptyRun = errors.New("bulk: the payout run is empty")

// ErrItemTooLarge：某一項自己一個人就塞不進一筆交易。走到這裡多半是設定寫錯了，
// 或這條鏈的限制真的太緊；再切細也救不了它，所以直接讓呼叫端知道。
var ErrItemTooLarge = errors.New("bulk: a single payout does not fit in one transaction")

// String 印一份計畫的總結行。格式固定，Example 與文章會直接貼這一行。
func (p Plan) String() string {
	return fmt.Sprintf("plan    %-8s %d payouts  %d %s  %d new accounts  rent %s %s",
		p.Chain, p.Payouts, len(p.Batches), plural(len(p.Batches), "batch", "batches"),
		p.NewAccounts, thousands(p.Rent), p.RentUnit)
}

// String 印一批的用量。每一種資源一格 "unit used/cap"，資源有幾種由那條鏈決定，同一份計畫裡不會變。
func (b Batch) String() string {
	parts := make([]string, 0, len(b.Used))
	for _, u := range b.Used {
		parts = append(parts, fmt.Sprintf("%s %s/%s", u.Unit, thousands(u.Used), thousands(u.Cap)))
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

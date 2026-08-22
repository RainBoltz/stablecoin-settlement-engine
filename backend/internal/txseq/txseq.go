// Package txseq 決定一筆交易在「發送帳戶」的序列裡站哪一格。
//
// 鏈下的世界到今天為止沒有順序問題：N 個 worker 各領各的 job，誰先誰後都一樣。但那 N 筆交易在鏈上是從同一個錢包出去的，
// 而每條鏈都用某種數字把同一個帳戶送出的交易排成一列。四條鏈排的方式不一樣：
//
//   - EVM：nonce，每個帳戶一個從 0 開始的計數器，交易自己帶。太小直接被拒，太大就排進節點的 queued 區
//     （geth 的說法是 "scheduled for future execution only"，
//     https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-txpool），等前面的空位被填掉才進 pending。
//     鏈上只回報「已經用到哪」
//     （eth_getTransactionCount，https://ethereum.org/en/developers/docs/apis/json-rpc/#eth_gettransactioncount），
//     下一個要用幾號是發送方自己算的。
//   - Solana：沒有帳戶層級的序號。交易帶一個 recent blockhash，只在最近 151 個 block（大約 60 到 90 秒）內有效，
//     驗證節點在這個窗口內記住處理過的簽名來去重
//     （https://solana.com/docs/advanced/confirmation）。要離線簽、要長期有效才用 durable nonce account。
//   - TON：seqno，存在錢包合約裡的計數器，送之前先跟合約要。合約「只收剛好等於當前 seqno 的那一個，收下就加一」
//     （https://docs.ton.org/contracts/standard/wallets/how-it-works），對不上的訊息直接不收，不會卡住後面。
//   - SUI：owned object 的 version，交易要指名它動到的 object 是哪一版。同一組 (ObjectId, SequenceNumber) 被兩筆
//     還沒 finalize 的交易同時用到就是 equivocation，那個 object 會被鎖到這個 epoch 結束
//     （https://docs.sui.io/guides/developer/sui-101/avoid-equivocation）。
//
// 分成兩類：前兩類（EVM 的 nonce、TON 的 seqno）的數字是發送方自己算的，所以要有人集中發號，這個 package 就是那個人；
// 後兩類（Solana 的 blockhash、SUI 的 object version）是送出當下從鏈上讀的，同一個帳戶要同時送幾筆都行，不需要排隊。
//
// 序號跟名額（見 relayer.Limiter）長得很像，都是「動手之前要先拿到的東西」，但歸還的規則差很多：名額還回去是免費的，
// 序號還回去只在「確定那筆交易沒出門」的時候才安全。拿了序號卻沒送出去，序列上就留下一個洞，
// 而 EVM 上一個洞會把這個帳戶後面所有交易都卡在 mempool。所以這裡把送出的結果分成三種（見 Sent），
// 不知道的一律當成用掉了。
//
// 本 package 為本系列從零設計，只取公開文件裡需要的那部分。今天只有記憶體版；換成資料庫或簽名服務時介面不變。
package txseq

import (
	"context"
	"errors"
	"fmt"
)

// Reservation 是一次取號的結果：這筆交易可以站在 Account 的第 Value 格。
//
// Ordered 是 false 時 Value 沒有意義（Solana 與 SUI 那一類，見 package 註解），Sender 直接忽略它。
// 呼叫端不自己組 Reservation：它只能從 Sequencer.Reserve 拿到，Resolve 也只認目前那一個。
type Reservation struct {
	Account string
	Value   uint64
	Ordered bool
}

// String 印一個取號結果，Example 會直接貼這個格式。地址縮寫成 0x1234…abcd，跟 ledger 印帳的科目同一種縮法。
func (r Reservation) String() string {
	if !r.Ordered {
		return "no slot needed"
	}
	return fmt.Sprintf("%s #%d", shortAccount(r.Account), r.Value)
}

// shortAccount 把 0x 開頭的 40 位地址縮成 0x1234…abcd，其他形式（Solana、TON、SUI 的地址）原樣印出。
func shortAccount(a string) string {
	if len(a) != 42 || a[:2] != "0x" {
		return a
	}
	return a[:6] + "…" + a[38:]
}

// Sent 是「這筆交易到底有沒有出門」的三種答案，決定序號怎麼收尾。分三種而不是兩種，是因為送出失敗跟沒送出去不是同一件事：
// RPC 逾時的那筆可能已經被節點收下了。
type Sent string

const (
	// SentYes：節點收下了。序號用掉，計數器往前。
	SentYes Sent = "sent"
	// SentNo：確定沒出門（簽名失敗、參數組不出來、連線根本沒建立）。序號退回去給下一筆用。
	SentNo Sent = "not-sent"
	// SentUnknown：不知道。序號當成用掉了，序列上留一個洞，這個帳戶暫停發號等人來對帳。
	// 退回去重用會撞到那筆可能正躺在 mempool 裡的交易，那是最貴的錯。
	SentUnknown Sent = "unknown"
)

var (
	// ErrGap：這個帳戶的序列上有一個沒交代的洞，在洞被填掉之前不再發號。
	ErrGap = errors.New("txseq: account has an unfilled gap")
	// ErrStale：這個 Reservation 不是目前那一個（已經收尾過，或中間被 Sync 洗掉了）。
	// 跟 queue.ErrStaleReceipt、intent 的 Version 是同一招：晚到的寫入蓋不掉新的。
	ErrStale = errors.New("txseq: reservation is stale")
	// ErrBusy：這個帳戶還有一個序號沒收尾，現在不能跟鏈上對齊。
	ErrBusy = errors.New("txseq: account has an outstanding reservation")
)

// Sequencer 是發號的介面。只有兩個動作：取號、收尾。誰去問鏈、序號長什麼樣、拿不到號要不要等，都是實作的事。
type Sequencer interface {
	// Reserve 幫 account 取一個號。取不到就等，等到 ctx 結束為止；帳戶上有洞時直接回 ErrGap，不等。
	Reserve(ctx context.Context, account string) (Reservation, error)
	// Resolve 收尾。一個 Reservation 只能收尾一次，第二次是 ErrStale。
	Resolve(ctx context.Context, r Reservation, s Sent) error
}

// Unordered 是不需要發號的鏈用的 Sequencer（Solana、SUI）：Reserve 永遠立刻放行、永遠不擋，Resolve 什麼都不做。
// 它存在的理由是讓 relayer 那一側不用寫 if：不管哪條鏈，流程都是取號、送、收尾。
type Unordered struct{}

// Reserve 實作 Sequencer：回一個 Ordered=false 的空位置。
func (Unordered) Reserve(_ context.Context, account string) (Reservation, error) {
	return Reservation{Account: account}, nil
}

// Resolve 實作 Sequencer：沒有序號可以收尾。
func (Unordered) Resolve(context.Context, Reservation, Sent) error { return nil }

package relayer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txfee"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// Broadcast 是「我們對某一筆 intent 送出過的一次交易」的紀錄：站哪一格、出多少價、拿到什麼 tx hash、
// 結果是三種發送結果的哪一種。
//
// 在這之前 relayer 是沒有記憶的：intent 的 State 就是它的全部紀錄，所以一筆卡在 settling 的 intent 只能被當成
// 「不知道發生過什麼」，而不知道就只能送審。有了這份紀錄，重來的 worker 才答得出「上一次送出去的是哪一個號、出了多少價」，
// 也才有辦法在同一個號上送一筆更貴的交易把它換掉（見 internal/txfee）。
//
// 它跟 ledger 的 journal 是同一個形狀：只加不改，一筆 intent 的多次廣播是多列，不是一列被改了幾次。
// 差別在於它記的是「我們做過的嘗試」，不是「錢的移動」，所以它不進帳本、也不需要平衡。
type Broadcast struct {
	IntentID string
	Account  string
	// Nonce 是這一次站的那一格。Ordered 是 false 的鏈（Solana、SUI）沒有這個數字。
	Nonce   uint64
	Ordered bool
	// Fill 代表這一次是去補洞的，也就是一次替換。
	Fill   bool
	Fee    txfee.Fee
	TxHash string
	Sent   txseq.Sent
	At     time.Time
}

// String 用固定格式印一行，Example 會直接貼這個格式。
func (b Broadcast) String() string {
	slot := "-"
	if b.Ordered {
		slot = fmt.Sprintf("#%d", b.Nonce)
		if b.Fill {
			slot += "f"
		}
	}
	tx := b.TxHash
	if tx == "" {
		tx = "-"
	}
	return fmt.Sprintf("%-4s %-9s %-8s %s", slot, b.Sent, tx, b.Fee)
}

// Broadcasts 是廣播紀錄本。只有兩個動作：記一筆、問這筆 intent 最後一次長什麼樣。
//
// 沒有「改」也沒有「刪」：一次送出去的交易是既成事實，就算後來被替換掉了，它也發生過。
type Broadcasts interface {
	// Record 加一筆。
	Record(ctx context.Context, b Broadcast) error
	// Last 回報這筆 intent 最後一次廣播的樣子，以及一共廣播過幾次。沒送過回 ok=false。
	Last(ctx context.Context, intentID string) (b Broadcast, tries int, ok bool, err error)
}

// MemoryBroadcasts 是記憶體版的 Broadcasts。跟其他 store 一樣，換成資料庫時介面不變。
type MemoryBroadcasts struct {
	mu   sync.Mutex
	rows map[string][]Broadcast
}

// NewMemoryBroadcasts 建立一本空的紀錄本。
func NewMemoryBroadcasts() *MemoryBroadcasts {
	return &MemoryBroadcasts{rows: make(map[string][]Broadcast)}
}

// Record 實作 Broadcasts。
func (m *MemoryBroadcasts) Record(_ context.Context, b Broadcast) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[b.IntentID] = append(m.rows[b.IntentID], b)
	return nil
}

// Last 實作 Broadcasts。
func (m *MemoryBroadcasts) Last(_ context.Context, intentID string) (Broadcast, int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.rows[intentID]
	if len(rows) == 0 {
		return Broadcast{}, 0, false, nil
	}
	return rows[len(rows)-1], len(rows), true, nil
}

// All 回報一筆 intent 的所有廣播紀錄，給 Example 與之後的對帳看。回的是複本。
func (m *MemoryBroadcasts) All(intentID string) []Broadcast {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Broadcast(nil), m.rows[intentID]...)
}

// record 是 worker 每次呼叫完 Sender 之後做的那件事：把這一次的嘗試寫下來。
//
// 用 WithoutCancel 是因為紀錄比 ctx 重要：ctx 被取消的那一刻交易可能已經在路上，這時候不寫，
// 重來的 worker 就又回到「不知道上一次做了什麼」。寫失敗只能吞掉，因為交易已經送出去了，
// 這裡回錯只會讓呼叫端以為沒送。
func (w *Worker) record(ctx context.Context, it *intent.Intent, res txseq.Reservation, fee txfee.Fee, txHash string, sent txseq.Sent) {
	_ = w.broadcasts.Record(context.WithoutCancel(ctx), Broadcast{
		IntentID: it.ID, Account: res.Account, Nonce: res.Value, Ordered: res.Ordered, Fill: res.Fill,
		Fee: fee, TxHash: txHash, Sent: sent, At: w.now(),
	})
}

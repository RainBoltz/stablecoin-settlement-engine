package ledger

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// Balance 是一個科目在一種 asset 上的兩個數字。
//
// Pending 是「帳上占著、鏈上還沒確認」的部分（hold 進、post / void 出）；Posted 是「鏈上確認了」的部分（只有 post 會動它）。
// 分開算的理由：merchant 問「我收了多少」要看 Posted，對帳引擎問「還有多少在路上」要看 Pending，
// 兩個數字混在一起哪個問題都答不了。這跟 Modern Treasury 的 pending balance / posted balance 是同一個切法。
type Balance struct {
	Pending *big.Int
	Posted  *big.Int
}

// String 印成 pending X posted Y。
func (b Balance) String() string { return fmt.Sprintf("pending %s  posted %s", b.Pending, b.Posted) }

// Journal 是帳本的儲存介面。今天只有記憶體版；換成資料庫時介面不變，因為它要求的只有一件事：
// 只能 Append，而且 Append 對同一個 ID 是冪等的。沒有 Update、沒有 Delete，介面上就沒有這兩個字。
type Journal interface {
	// Append 寫入一列。回傳寫進去的那列（帶 Seq 與 Hash）；applied=false 代表同 ID 同內容的重放，什麼都沒動。
	Append(ctx context.Context, e Entry) (stored Entry, applied bool, err error)
	// Get 用 ID 找一列。
	Get(ctx context.Context, id string) (Entry, error)
	// ByRef 回傳一筆付款的所有列，照 Seq 排。對帳引擎與 GET /v1/payment_refs/{ref} 之後都從這裡拿帳。
	ByRef(ctx context.Context, ref paymentref.Ref) ([]Entry, error)
	// Balance 回傳一個科目在一種 asset 上的 pending 與 posted。從沒出現過的科目回傳兩個零，不是錯誤。
	Balance(ctx context.Context, acct Account, asset Asset) (Balance, error)
	// Scan 照 Seq 從第一列走到最後一列，fn 回錯就停。Verify 與匯出用它；資料庫版會分批讀，介面不變。
	Scan(ctx context.Context, fn func(Entry) error) error
}

// MemoryJournal 是給測試與本地開發用的 Journal。
type MemoryJournal struct {
	mu       sync.Mutex
	entries  []Entry
	byID     map[string]int  // ID → entries 的索引
	resolved map[string]bool // 已經被 post 或 void 收尾的 hold ID
}

// NewMemoryJournal 建立一本空帳。
func NewMemoryJournal() *MemoryJournal {
	return &MemoryJournal{byID: make(map[string]int), resolved: make(map[string]bool)}
}

// Append 實作 Journal。檢查順序是刻意排的：
//  1. 這一列自己合不合法（欄位、腿平不平）。
//  2. 同 ID 已存在：內容一樣就是重放，放行不動；不一樣就是 ErrConflict。
//  3. post / void：指的那筆 hold 要在、要是 hold、ref 與 asset 要對得上，而且還沒被收尾過。
//  4. 才給 Seq、接上 hash 鏈、寫入。
//
// 重放擺在 hold 檢查之前，是因為 listener 把同一個 post 送兩次時，第二次不該撞到 ErrAlreadyResolved：
// 那不是「有人想收尾第二次」，是同一次收尾送到了兩遍。
func (j *MemoryJournal) Append(_ context.Context, e Entry) (Entry, bool, error) {
	if err := e.Validate(); err != nil {
		return Entry{}, false, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if idx, ok := j.byID[e.ID]; ok {
		if j.entries[idx].Same(e) {
			return j.entries[idx].clone(), false, nil
		}
		return Entry{}, false, fmt.Errorf("%w: %s", ErrConflict, e.ID)
	}
	if e.Kind == KindPost || e.Kind == KindVoid {
		idx, ok := j.byID[e.Holds]
		if !ok || j.entries[idx].Kind != KindHold || j.entries[idx].Ref != e.Ref || j.entries[idx].Asset != e.Asset {
			return Entry{}, false, fmt.Errorf("%w: %s resolves %q", ErrNoSuchHold, e.ID, e.Holds)
		}
		if j.resolved[e.Holds] {
			return Entry{}, false, fmt.Errorf("%w: %s", ErrAlreadyResolved, e.Holds)
		}
	}

	stored := e.clone()
	stored.Seq = uint64(len(j.entries)) + 1
	if n := len(j.entries); n > 0 {
		stored.PrevHash = j.entries[n-1].Hash
	}
	stored.Hash = hashEntry(stored.PrevHash, stored)
	j.entries = append(j.entries, stored)
	j.byID[stored.ID] = len(j.entries) - 1
	if stored.Kind != KindHold {
		j.resolved[stored.Holds] = true
	}
	return stored.clone(), true, nil
}

// Get 實作 Journal。
func (j *MemoryJournal) Get(_ context.Context, id string) (Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	idx, ok := j.byID[id]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return j.entries[idx].clone(), nil
}

// ByRef 實作 Journal。
func (j *MemoryJournal) ByRef(_ context.Context, ref paymentref.Ref) ([]Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Entry
	for _, e := range j.entries {
		if e.Ref == ref {
			out = append(out, e.clone())
		}
	}
	return out, nil
}

// Balance 實作 Journal。記憶體版每次都從第一列算到最後一列：餘額是 journal 的投影，不是另一個會被改的欄位。
func (j *MemoryJournal) Balance(_ context.Context, acct Account, asset Asset) (Balance, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	b := Balances(j.entries)[BalanceKey{acct, asset}]
	if b.Pending == nil {
		b = Balance{Pending: new(big.Int), Posted: new(big.Int)}
	}
	return b, nil
}

// Scan 實作 Journal。fn 拿到的是拷貝。
func (j *MemoryJournal) Scan(_ context.Context, fn func(Entry) error) error {
	j.mu.Lock()
	snapshot := make([]Entry, len(j.entries))
	for i, e := range j.entries {
		snapshot[i] = e.clone()
	}
	j.mu.Unlock()
	for _, e := range snapshot {
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

// BalanceKey 是餘額表的索引：科目加 asset。
type BalanceKey struct {
	Account Account
	Asset   Asset
}

// Balances 是投影本體：把一串 entries（照 Seq 排、hold 一定在它的 post / void 前面）折成每個 (科目, asset) 的餘額。
// 純函式、不碰 journal，所以任何人拿匯出的 entries 都能重算，跟 Journal.Balance 的結果必須一樣（測試釘著）。
//
// 規則只有三條：hold 的腿加進 pending；post 把它那筆 hold 的腿從 pending 扣掉、自己的腿加進 posted；
// void 只把 hold 的腿從 pending 扣掉。post 的腿跟 hold 的腿可以不一樣（實收少於請款、多一條 fee），
// pending 扣的是 hold 記的、posted 加的是 post 記的，差額就自然落在 fee 那條腿上。
func Balances(entries []Entry) map[BalanceKey]Balance {
	out := make(map[BalanceKey]Balance)
	holds := make(map[string]Entry, len(entries))
	get := func(acct Account, asset Asset) Balance {
		k := BalanceKey{acct, asset}
		b, ok := out[k]
		if !ok {
			b = Balance{Pending: new(big.Int), Posted: new(big.Int)}
			out[k] = b
		}
		return b
	}
	for _, e := range entries {
		switch e.Kind {
		case KindHold:
			holds[e.ID] = e
			for _, l := range e.Legs {
				b := get(l.Account, e.Asset)
				b.Pending.Add(b.Pending, l.Amount)
			}
		case KindPost, KindVoid:
			h := holds[e.Holds]
			for _, l := range h.Legs {
				b := get(l.Account, h.Asset)
				b.Pending.Sub(b.Pending, l.Amount)
			}
			for _, l := range e.Legs {
				b := get(l.Account, e.Asset)
				b.Posted.Add(b.Posted, l.Amount)
			}
		}
	}
	return out
}

// Verify 從第一列走到最後一列重算 hash 鏈：Seq 要密集、PrevHash 要等於上一列的 Hash、Hash 要等於重算的值。
// 任何一列被改過（金額、科目、時間、甚至只是搬了位置），從那一列起就對不上，回傳 ErrChainBroken 帶著第一個壞掉的 Seq。
//
// 它能做到的是「發現」不是「阻止」：有資料庫寫入權的人還是改得動，但改完就一定會被下一次 Verify 抓到；
// 要真的阻止，最後那個 Hash 得定期存到一個寫入權不同的地方（另一個帳號的 bucket、或印出來），這是部署的事，
// 不是這個 package 的事。
func Verify(ctx context.Context, j Journal) error {
	var prev [32]byte
	var want uint64 = 1
	return j.Scan(ctx, func(e Entry) error {
		if e.Seq != want {
			return fmt.Errorf("%w: seq %d, want %d", ErrChainBroken, e.Seq, want)
		}
		if e.PrevHash != prev {
			return fmt.Errorf("%w: seq %d prev hash does not match seq %d", ErrChainBroken, e.Seq, e.Seq-1)
		}
		if got := hashEntry(prev, e); got != e.Hash {
			return fmt.Errorf("%w: seq %d hash does not match its content", ErrChainBroken, e.Seq)
		}
		prev = e.Hash
		want++
		return nil
	})
}

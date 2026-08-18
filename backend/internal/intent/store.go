package intent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

var (
	// ErrNotFound：沒有這筆 intent。
	ErrNotFound = errors.New("intent: not found")
	// ErrVersionConflict：存檔時發現別人先改過了。這不是 bug，是兩個元件同時想推同一筆 intent
	// 的正常結果；輸的一方重新讀一次再決定要不要再試。
	ErrVersionConflict = errors.New("intent: version conflict")
	// ErrRefMismatch：要存的 intent，它的 Ref 跟從它自己的條件重算出來的不一樣。
	// 這是 bug 或竄改（有人動了金額、收款人卻沒換 intent），不是競爭；一律拒絕寫入。
	ErrRefMismatch = errors.New("intent: payment ref does not match the intent terms")
)

// Store 是 intent 的儲存介面。今天只有記憶體版；換成資料庫時介面不變，
// 因為它要求的只有一件事：Save 必須是 compare-and-swap。
//
// 為什麼堅持 CAS：狀態機本身是純函式，Apply 不知道有沒有別人也在動同一筆 intent。
// 「先讀、Apply、寫回」這三步如果不是原子的，兩個 relayer worker 各自讀到 authorized、
// 各自推到 settling、各自廣播，錢就動了兩次。CAS 把這件事變成：只有一個寫得回去。
//
// 兩個讀取入口對應兩種呼叫者：鏈下的 API 與 queue 拿 id 來查；從鏈上回來的 listener 與對帳引擎
// 手上只有 ref，拿 ref 來查。資料庫版本就是 intents 表上多一個 unique index 在 ref 欄位。
type Store interface {
	// Get 回傳一份拷貝，改它不會影響存的那份。
	Get(ctx context.Context, id string) (*Intent, error)
	// GetByRef 用 PaymentRef 找回 intent，同樣回傳拷貝。這是鏈上世界回到鏈下世界的唯一入口。
	GetByRef(ctx context.Context, ref paymentref.Ref) (*Intent, error)
	// Save 只在存的那份 Version 等於 expectedVersion 時寫入；新 intent 用 expectedVersion=0。
	// 寫入前會用 intent 自己的條件重算 Ref，對不上就是 ErrRefMismatch。
	Save(ctx context.Context, it *Intent, expectedVersion uint64) error
}

// MemoryStore 是給測試與本地開發用的 Store。
type MemoryStore struct {
	mu    sync.Mutex
	m     map[string]*Intent
	byRef map[paymentref.Ref]string
}

// NewMemoryStore 建立一個空的 MemoryStore。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{m: make(map[string]*Intent), byRef: make(map[paymentref.Ref]string)}
}

// Get 實作 Store。
func (s *MemoryStore) Get(_ context.Context, id string) (*Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.m[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return it.Clone(), nil
}

// GetByRef 實作 Store。
func (s *MemoryStore) GetByRef(_ context.Context, ref paymentref.Ref) (*Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byRef[ref]
	if !ok {
		return nil, fmt.Errorf("%w: ref %s", ErrNotFound, ref)
	}
	return s.m[id].Clone(), nil
}

// Save 實作 Store。先做 CAS，再核對 ref：版本衝突是常態、先擋掉便宜；ref 對不上是異常，擋在寫入之前。
func (s *MemoryStore) Save(_ context.Context, it *Intent, expectedVersion uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, exists := s.m[it.ID]
	switch {
	case !exists && expectedVersion != 0:
		return fmt.Errorf("%w: %s does not exist, expected version %d", ErrVersionConflict, it.ID, expectedVersion)
	case exists && cur.Version != expectedVersion:
		return fmt.Errorf("%w: %s is at version %d, expected %d", ErrVersionConflict, it.ID, cur.Version, expectedVersion)
	}
	if want := paymentref.Derive(it.Terms()); it.Ref != want {
		return fmt.Errorf("%w: %s has ref %s, terms derive %s", ErrRefMismatch, it.ID, it.Ref, want)
	}
	s.m[it.ID] = it.Clone()
	s.byRef[it.Ref] = it.ID
	return nil
}

// Advance 是「讀、Apply、CAS 寫回」的標準寫法。呼叫端拿到的是寫回後的那一份。
//
// 不在這裡自動重試：ErrVersionConflict 之後該不該再試，要看是誰在試。
// relayer 讀到別人已經把它推到 settling，就該放手；API 讀到已經 canceled，就回報使用者。
// 這個判斷屬於呼叫端，狀態機不替它決定。
func Advance(ctx context.Context, s Store, id string, req Request) (*Intent, bool, error) {
	it, err := s.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	expected := it.Version
	applied, err := Apply(it, req)
	if err != nil {
		return nil, false, err
	}
	if !applied {
		return it, false, nil
	}
	if err := s.Save(ctx, it, expected); err != nil {
		return nil, false, err
	}
	return it, true, nil
}

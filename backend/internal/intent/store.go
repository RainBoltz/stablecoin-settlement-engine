package intent

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrNotFound：沒有這筆 intent。
	ErrNotFound = errors.New("intent: not found")
	// ErrVersionConflict：存檔時發現別人先改過了。這不是 bug，是兩個元件同時想推同一筆 intent
	// 的正常結果；輸的一方重新讀一次再決定要不要再試。
	ErrVersionConflict = errors.New("intent: version conflict")
)

// Store 是 intent 的儲存介面。今天只有記憶體版；換成資料庫時介面不變，
// 因為它要求的只有一件事：Save 必須是 compare-and-swap。
//
// 為什麼堅持 CAS：狀態機本身是純函式，Apply 不知道有沒有別人也在動同一筆 intent。
// 「先讀、Apply、寫回」這三步如果不是原子的，兩個 relayer worker 各自讀到 authorized、
// 各自推到 settling、各自廣播，錢就動了兩次。CAS 把這件事變成：只有一個寫得回去。
type Store interface {
	// Get 回傳一份拷貝，改它不會影響存的那份。
	Get(ctx context.Context, id string) (*Intent, error)
	// Save 只在存的那份 Version 等於 expectedVersion 時寫入；新 intent 用 expectedVersion=0。
	Save(ctx context.Context, it *Intent, expectedVersion uint64) error
}

// MemoryStore 是給測試與本地開發用的 Store。
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]*Intent
}

// NewMemoryStore 建立一個空的 MemoryStore。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{m: make(map[string]*Intent)}
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

// Save 實作 Store。
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
	s.m[it.ID] = it.Clone()
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

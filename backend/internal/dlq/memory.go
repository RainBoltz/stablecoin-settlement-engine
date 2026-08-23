package dlq

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryStore 是給測試與本地開發用的 Store。一把鎖包住整張表就夠了：Resolve 本來就得是原子的
// （「確認它還停著、標成已處置」必須是一步，兩個人同時按下 redrive 只能有一個拿到）。
//
// 一份 job 只留最新的那一趟，前面幾趟只剩 Cycles 這個數字。要留完整歷程的話那是 journal 的形狀，
// 而「這筆付款發生過什麼」本來就記在 intent 的 History 上；這裡記的是那張便條的下落，一筆就夠。
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]Record
	order   []string // 依第一次 Park 的先後排，List 照這個順序回
}

// NewMemoryStore 建立一個空的收容所。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]Record)}
}

// Park 實作 Store。Status、ParkedAt、Cycles 與兩個 Resolved 欄位由這裡填，呼叫端給的值會被蓋掉：
// 「這一筆現在是什麼狀態」只有 store 說了算，不然兩個 worker 各寫各的就對不起來了。
func (s *MemoryStore) Park(_ context.Context, r Record, now time.Time) (bool, error) {
	if err := r.Validate(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.records[r.Job.ID]
	if exists && old.Status == StatusParked {
		return false, nil
	}
	r.Status, r.ParkedAt, r.Cycles = StatusParked, now, 1
	r.ResolvedBy, r.ResolvedAt = "", time.Time{}
	if exists {
		r.Cycles = old.Cycles + 1
	} else {
		s.order = append(s.order, r.Job.ID)
	}
	s.records[r.Job.ID] = r
	return true, nil
}

// Get 實作 Store。
func (s *MemoryStore) Get(_ context.Context, jobID string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[jobID]
	if !ok {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	return r, nil
}

// List 實作 Store。
func (s *MemoryStore) List(_ context.Context, status Status) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.order))
	for _, id := range s.order {
		if r := s.records[id]; status == "" || r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

// Resolve 實作 Store。只收 redriven 與 dropped 兩個目標，而且一定要有人簽名：
// 沒有簽名的處置等於沒有人負責，那不是我們想留在稽核紀錄上的東西。
func (s *MemoryStore) Resolve(_ context.Context, jobID string, to Status, by string, now time.Time) (Record, error) {
	if to != StatusRedriven && to != StatusDropped {
		return Record{}, fmt.Errorf("%w: cannot resolve to %q", ErrInvalidRecord, to)
	}
	if by == "" {
		return Record{}, fmt.Errorf("%w: resolved by whom", ErrInvalidRecord)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[jobID]
	if !ok {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	if r.Status != StatusParked {
		return Record{}, fmt.Errorf("%w: %s is %s", ErrNotParked, jobID, r.Status)
	}
	r.Status, r.ResolvedBy, r.ResolvedAt = to, by, now
	s.records[jobID] = r
	return r, nil
}

package idempotency

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Status 是一筆紀錄的兩種狀態：handler 還在跑、或已經有答案。
type Status string

const (
	// StatusInFlight：key 已被認領，handler 正在執行（或執行到一半掛了）。
	StatusInFlight Status = "in_flight"
	// StatusCompleted：handler 跑完了，答案存在 Response 裡。
	StatusCompleted Status = "completed"
)

// Response 是要原封不動重放給客戶端的東西：狀態碼、header、body。
//
// 存整個回應而不是只存「產生的 intent id」，是因為重放必須逐 byte 一樣：
// 客戶端的 parser 第一次跟第二次看到的要是同一份東西，不然重試邏輯會被自己的差異絆倒。
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// clone 讓存進去與拿出來的都是拷貝，呼叫端改不到 store 裡那份。
func (r Response) clone() Response {
	c := Response{Status: r.Status, Header: r.Header.Clone(), Body: append([]byte(nil), r.Body...)}
	if c.Header == nil {
		c.Header = http.Header{}
	}
	return c
}

// Record 是 (scope, key) 對應的一筆紀錄。
type Record struct {
	Scope       Scope
	Key         Key
	Fingerprint Fingerprint
	Status      Status
	// Attempt 每被 Claim 一次加一。Complete 要帶同一個 Attempt 才寫得回去：
	// lease 過期後被別人接手，原本那個跑到一半的 worker 醒來也蓋不掉新的結果。跟 intent 的 Version 是同一招。
	Attempt uint64
	// ClaimedAt 是這一次 Attempt 開始的時間；LeaseUntil 是它最晚要交出答案的時間，過了就可以被接手。
	ClaimedAt  time.Time
	LeaseUntil time.Time
	// ExpiresAt 過了，這筆紀錄視同不存在：同一個 key 再來會被當成全新的請求（Stripe 的 24 小時就是這個）。
	ExpiresAt time.Time
	// Response 只有 StatusCompleted 才有。
	Response *Response
}

// Outcome 是 Claim 的四種結果。呼叫端照它決定要跑 handler、重放答案、還是拒絕。
type Outcome int

const (
	// OutcomeFresh：這個 key 沒見過（或上一筆已過期），紀錄已建立為 in_flight，去跑 handler。
	OutcomeFresh Outcome = iota
	// OutcomeReplay：同 key、同 fingerprint、已有答案，直接回 Record.Response。
	OutcomeReplay
	// OutcomeInFlight：同 key、同 fingerprint，但原請求還在跑，回 409 讓客戶端等一下再來。
	OutcomeInFlight
	// OutcomeMismatch：同 key、不同 fingerprint。不管原請求跑完沒，一律拒絕，回 422。
	OutcomeMismatch
)

func (o Outcome) String() string {
	switch o {
	case OutcomeFresh:
		return "fresh"
	case OutcomeReplay:
		return "replay"
	case OutcomeInFlight:
		return "in_flight"
	case OutcomeMismatch:
		return "mismatch"
	}
	return fmt.Sprintf("outcome(%d)", int(o))
}

// Policy 是兩個時間常數。
type Policy struct {
	// TTL 是紀錄的保存期限。Stripe 公開的是 24 小時：夠客戶端把該重試的都試完，
	// 又不用永遠留著。過了 TTL 客戶端可以拿同一個 key 做新的事，所以 key 不能當成永久身分。
	TTL time.Duration
	// Lease 是一次 Attempt 最多可以佔著 in_flight 多久。handler 正常幾十毫秒就跑完，
	// 撐到 lease 過期代表那個程序多半已經死了；讓下一次重試接手，比讓客戶端對著 409 等 24 小時好。
	Lease time.Duration
}

// DefaultPolicy：24 小時 TTL、30 秒 lease。
func DefaultPolicy() Policy {
	return Policy{TTL: 24 * time.Hour, Lease: 30 * time.Second}
}

var (
	// ErrNotFound：Complete 找不到這筆紀錄（多半是過期被清掉了）。
	ErrNotFound = errors.New("idempotency: record not found")
	// ErrStaleClaim：Complete 帶的 Attempt 不是目前那一次。你的 lease 過期、別人已經接手，你的答案作廢。
	ErrStaleClaim = errors.New("idempotency: claim is stale")
)

// Store 是紀錄的儲存介面。今天只有記憶體版；換成資料庫時介面不變。
//
// 整個機制只有一個原子點：Claim。「查有沒有、沒有就寫入 in_flight」必須是一步，
// 兩個同 key 的請求同時進來時，只能有一個拿到 OutcomeFresh。資料庫版本靠 (scope, key) 的
// unique constraint 或 SELECT ... FOR UPDATE 做同一件事。
type Store interface {
	// Claim 替 (scope, key, fp) 認領一次執行機會。回傳的 Record 是拷貝。
	Claim(ctx context.Context, scope Scope, key Key, fp Fingerprint, now time.Time, policy Policy) (Record, Outcome, error)
	// Complete 把答案寫回去。attempt 必須等於 Claim 當時拿到的 Record.Attempt。
	Complete(ctx context.Context, scope Scope, key Key, attempt uint64, resp Response, now time.Time) error
}

type recordKey struct {
	scope Scope
	key   Key
}

// MemoryStore 是給測試與本地開發用的 Store。一把鎖包住整張表就夠了，因為 Claim 本來就得是原子的。
type MemoryStore struct {
	mu sync.Mutex
	m  map[recordKey]*Record
}

// NewMemoryStore 建立一個空的 MemoryStore。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{m: make(map[recordKey]*Record)}
}

// Claim 實作 Store。判斷順序：過期視同不存在 → fingerprint 不同一律 mismatch → 有答案就重放 →
// lease 還沒過就是 in_flight → lease 過了就接手。
func (s *MemoryStore) Claim(_ context.Context, scope Scope, key Key, fp Fingerprint, now time.Time, policy Policy) (Record, Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rk := recordKey{scope, key}
	cur, exists := s.m[rk]
	if exists && !cur.ExpiresAt.After(now) {
		delete(s.m, rk)
		exists = false
	}
	if !exists {
		rec := &Record{
			Scope: scope, Key: key, Fingerprint: fp,
			Status: StatusInFlight, Attempt: 1,
			ClaimedAt: now, LeaseUntil: now.Add(policy.Lease), ExpiresAt: now.Add(policy.TTL),
		}
		s.m[rk] = rec
		return rec.clone(), OutcomeFresh, nil
	}
	if cur.Fingerprint != fp {
		return cur.clone(), OutcomeMismatch, nil
	}
	if cur.Status == StatusCompleted {
		return cur.clone(), OutcomeReplay, nil
	}
	if cur.LeaseUntil.After(now) {
		return cur.clone(), OutcomeInFlight, nil
	}
	// lease 過期：接手。Attempt 加一，讓前一個 worker 的 Complete 作廢。
	cur.Attempt++
	cur.ClaimedAt = now
	cur.LeaseUntil = now.Add(policy.Lease)
	return cur.clone(), OutcomeFresh, nil
}

// Complete 實作 Store。
func (s *MemoryStore) Complete(_ context.Context, scope Scope, key Key, attempt uint64, resp Response, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, exists := s.m[recordKey{scope, key}]
	if !exists {
		return fmt.Errorf("%w: %s/%s", ErrNotFound, scope, key)
	}
	if cur.Attempt != attempt {
		return fmt.Errorf("%w: %s/%s is at attempt %d, got %d", ErrStaleClaim, scope, key, cur.Attempt, attempt)
	}
	if cur.Status == StatusCompleted {
		// 同一個 attempt 寫兩次答案：第二次不覆蓋。答案一旦存了就是那一份。
		return nil
	}
	r := resp.clone()
	cur.Status = StatusCompleted
	cur.Response = &r
	return nil
}

// Sweep 清掉已過期的紀錄，回傳清了幾筆。記憶體版沒有背景工作，由呼叫端決定多久掃一次。
func (s *MemoryStore) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, r := range s.m {
		if !r.ExpiresAt.After(now) {
			delete(s.m, k)
			n++
		}
	}
	return n
}

// Len 回傳目前有幾筆紀錄（含已過期但還沒 Sweep 的），測試用。
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func (r *Record) clone() Record {
	c := *r
	if r.Response != nil {
		resp := r.Response.clone()
		c.Response = &resp
	}
	return c
}

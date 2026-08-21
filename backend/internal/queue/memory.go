package queue

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryQueue 是給測試與本地開發用的 Queue。一把鎖包住整張表就夠了：Lease 本來就得是原子的
// （「找一份可見的、標成被領走」必須是一步，兩個 worker 同時來只能有一個拿到）。
type MemoryQueue struct {
	mu    sync.Mutex
	jobs  map[string]*slot
	order []string // 依 Enqueue 先後排，Lease 從最早的可見 job 開始領
}

// slot 是一份 job 在 queue 裡的狀態。attempt 是它被交付過幾次；notBefore 之前不交付（Nack 的延遲）；
// leaseUntil 之前對別人隱形。
type slot struct {
	job        Job
	attempt    uint64
	notBefore  time.Time
	leaseUntil time.Time
}

// NewMemoryQueue 建立一條空的 queue。
func NewMemoryQueue() *MemoryQueue {
	return &MemoryQueue{jobs: make(map[string]*slot)}
}

// Enqueue 實作 Queue。
func (q *MemoryQueue) Enqueue(_ context.Context, job Job, now time.Time) (bool, error) {
	if err := job.Validate(); err != nil {
		return false, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.jobs[job.ID]; exists {
		return false, nil
	}
	q.jobs[job.ID] = &slot{job: job, notBefore: now}
	q.order = append(q.order, job.ID)
	return true, nil
}

// Lease 實作 Queue。可見的定義：notBefore 到了、而且沒有人正拿著有效的 lease。
// lease 過期但沒 Ack 的 job 在這裡會被再領一次，attempt 加一，這就是 at-least-once 的來源。
func (q *MemoryQueue) Lease(_ context.Context, now time.Time, lease time.Duration) (Delivery, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range q.order {
		s := q.jobs[id]
		if s.notBefore.After(now) || s.leaseUntil.After(now) {
			continue
		}
		s.attempt++
		s.leaseUntil = now.Add(lease)
		return Delivery{Job: s.job, Attempt: s.attempt, Receipt: receipt(id, s.attempt), LeaseUntil: s.leaseUntil}, true, nil
	}
	return Delivery{}, false, nil
}

// Ack 實作 Queue。憑證對得上才刪；lease 過期後被別人領走，attempt 已經加一，舊憑證就對不上。
func (q *MemoryQueue) Ack(_ context.Context, d Delivery) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	s, err := q.current(d)
	if err != nil {
		return err
	}
	delete(q.jobs, s.job.ID)
	for i, id := range q.order {
		if id == s.job.ID {
			q.order = append(q.order[:i], q.order[i+1:]...)
			break
		}
	}
	return nil
}

// Nack 實作 Queue。放掉 lease、延後 notBefore；attempt 不歸零，之後 worker 看得到這是第幾次。
func (q *MemoryQueue) Nack(_ context.Context, d Delivery, retryAfter time.Duration, now time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	s, err := q.current(d)
	if err != nil {
		return err
	}
	s.leaseUntil = time.Time{}
	s.notBefore = now.Add(retryAfter)
	return nil
}

// Len 實作 Queue。
func (q *MemoryQueue) Len(_ context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs), nil
}

// current 找出憑證指的那份 job，並確認憑證是目前這一次交付的。呼叫端要先拿鎖。
func (q *MemoryQueue) current(d Delivery) (*slot, error) {
	s, ok := q.jobs[d.Job.ID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, d.Job.ID)
	}
	if d.Receipt != receipt(s.job.ID, s.attempt) {
		return nil, fmt.Errorf("%w: %s is at attempt %d, receipt is for %s", ErrStaleReceipt, s.job.ID, s.attempt, d.Receipt)
	}
	return s, nil
}

// receipt 是「job ID 加 attempt」。它不需要難猜（這不是安全機制），只需要每一次交付都不一樣。
func receipt(id string, attempt uint64) Receipt {
	return Receipt(fmt.Sprintf("%s#%d", id, attempt))
}

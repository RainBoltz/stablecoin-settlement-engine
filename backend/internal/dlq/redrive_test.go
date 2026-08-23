package dlq

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
)

// brokenQueue 是一條 Enqueue 一定失敗的 queue，用來釘 Redrive 的兩步順序。
type brokenQueue struct{ queue.Queue }

func (brokenQueue) Enqueue(context.Context, queue.Job, time.Time) (bool, error) {
	return false, errors.New("queue: unavailable")
}

// TestRedrive_RequeuesTheJobAndSignsTheRecord：redrive 的正常路。job 原封不動回到 queue，
// 紀錄上留下誰按的；那份 job 對 worker 來說就是一份新的工作，attempt 從 1 重新算。
func TestRedrive_RequeuesTheJobAndSignsTheRecord(t *testing.T) {
	ctx := context.Background()
	s, q := NewMemoryStore(), queue.NewMemoryQueue()
	mustPark(t, s, parked("pi_0001", "created"), t0)

	got, err := Redrive(ctx, s, q, "pi_0001/settle", "ops", t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("redrive: %v", err)
	}
	if got.Status != StatusRedriven || got.ResolvedBy != "ops" || !got.ResolvedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("record: %+v", got)
	}
	d, ok, err := q.Lease(ctx, t0.Add(time.Hour), time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if d.Job != settle("pi_0001") || d.Attempt != 1 {
		t.Fatalf("the job should come back untouched and freshly counted: %+v", d)
	}
}

// TestRedrive_TwiceIsRefused：第二次按下去要被擋掉，不然一個 job 可以被無限放回去，
// 每一次都吃掉一輪 worker。擋它的是 Resolve 那個 CAS。
func TestRedrive_TwiceIsRefused(t *testing.T) {
	ctx := context.Background()
	s, q := NewMemoryStore(), queue.NewMemoryQueue()
	mustPark(t, s, parked("pi_0001", "created"), t0)
	if _, err := Redrive(ctx, s, q, "pi_0001/settle", "ops", t0); err != nil {
		t.Fatalf("first redrive: %v", err)
	}
	if _, err := Redrive(ctx, s, q, "pi_0001/settle", "ops", t0); !errors.Is(err, ErrNotParked) {
		t.Fatalf("second redrive: want ErrNotParked, got %v", err)
	}
	if n, _ := q.Len(ctx); n != 1 {
		t.Fatalf("want one job on the queue, got %d", n)
	}
}

// TestRedrive_DroppedRecordNeverReachesTheQueue：有人已經判定這份 job 沒用了，
// 那它就不該因為第二個人按錯而回到 queue 上。這條釘的是 Enqueue 之前那道檢查。
func TestRedrive_DroppedRecordNeverReachesTheQueue(t *testing.T) {
	ctx := context.Background()
	s, q := NewMemoryStore(), queue.NewMemoryQueue()
	mustPark(t, s, parked("pi_0001", "created"), t0)
	if _, err := Drop(ctx, s, "pi_0001/settle", "ops", t0); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := Redrive(ctx, s, q, "pi_0001/settle", "someone-else", t0); !errors.Is(err, ErrNotParked) {
		t.Fatalf("want ErrNotParked, got %v", err)
	}
	if n, _ := q.Len(ctx); n != 0 {
		t.Fatalf("queue should stay empty, got %d", n)
	}
}

// TestRedrive_LeavesTheRecordParkedWhenTheQueueIsDown：兩步的順序是先放回、再標記，
// 所以放不回去的時候紀錄要留在 parked，人才能再按一次。反過來寫的話這份工作就沒有人記得了。
func TestRedrive_LeavesTheRecordParkedWhenTheQueueIsDown(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	mustPark(t, s, parked("pi_0001", "created"), t0)

	if _, err := Redrive(ctx, s, brokenQueue{}, "pi_0001/settle", "ops", t0); err == nil {
		t.Fatal("want the enqueue error to surface")
	}
	got, _ := s.Get(ctx, "pi_0001/settle")
	if got.Status != StatusParked || got.ResolvedBy != "" {
		t.Fatalf("record should still be parked: %+v", got)
	}
	// 人再按一次，這次 queue 好了，結果跟第一次就成功一樣。
	if _, err := Redrive(ctx, s, queue.NewMemoryQueue(), "pi_0001/settle", "ops", t0); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// TestDrop_KeepsTheJobOffTheQueue：丟掉的是便條，不是付款。queue 上什麼都不會多，
// 那筆 intent 也沒有人碰（這個 package 根本讀不到它）。
func TestDrop_KeepsTheJobOffTheQueue(t *testing.T) {
	ctx := context.Background()
	s, q := NewMemoryStore(), queue.NewMemoryQueue()
	mustPark(t, s, parked("pi_0001", "needs_review"), t0)

	got, err := Drop(ctx, s, "pi_0001/settle", "ops", t0)
	if err != nil || got.Status != StatusDropped || got.ResolvedBy != "ops" {
		t.Fatalf("drop: %+v err=%v", got, err)
	}
	if n, _ := q.Len(ctx); n != 0 {
		t.Fatalf("queue should stay empty, got %d", n)
	}
}

// TestRedrive_UnknownJob：沒停過的 job 沒有東西可以放回去。
func TestRedrive_UnknownJob(t *testing.T) {
	_, err := Redrive(context.Background(), NewMemoryStore(), queue.NewMemoryQueue(), "pi_9999/settle", "ops", t0)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

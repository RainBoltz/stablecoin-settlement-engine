package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

var (
	t0   = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	refA = paymentref.Derive(paymentref.Terms{IntentID: "pi_0001", Chain: "evm:31337",
		Token: "0x5FbDB2315678afecb367f032d93F642f64180aa3", Payer: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
		Merchant: "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC", Amount: "100000000"})
)

func settle(id string) Job {
	return Job{ID: id + "/settle", Kind: KindSettle, IntentID: id, Ref: refA}
}

func mustEnqueue(t *testing.T, q Queue, j Job, now time.Time) {
	t.Helper()
	applied, err := q.Enqueue(context.Background(), j, now)
	if err != nil || !applied {
		t.Fatalf("enqueue %s: applied=%v err=%v", j.ID, applied, err)
	}
}

func mustLease(t *testing.T, q Queue, now time.Time, lease time.Duration) Delivery {
	t.Helper()
	d, ok, err := q.Lease(context.Background(), now, lease)
	if err != nil || !ok {
		t.Fatalf("lease at %s: ok=%v err=%v", now.Format(time.TimeOnly), ok, err)
	}
	return d
}

// TestJob_Validate：缺 ID、缺 intent id、缺 ref、種類不對，都不收。job 是指標，指標不完整就沒有意義。
func TestJob_Validate(t *testing.T) {
	good := settle("pi_0001")
	if err := good.Validate(); err != nil {
		t.Fatalf("good job: %v", err)
	}
	for name, mutate := range map[string]func(*Job){
		"no id":     func(j *Job) { j.ID = "" },
		"no intent": func(j *Job) { j.IntentID = "" },
		"no ref":    func(j *Job) { j.Ref = paymentref.Ref{} },
		"bad kind":  func(j *Job) { j.Kind = "refund" },
	} {
		j := good
		mutate(&j)
		if err := j.Validate(); !errors.Is(err, ErrInvalidJob) {
			t.Errorf("%s: want ErrInvalidJob, got %v", name, err)
		}
	}
}

// TestMemoryQueue_EnqueueIsIdempotentWhilePending：同 ID 的 job 還在 queue 裡（排隊中或被領走）時再 Enqueue 是 no-op。
// API 重送、sweeper 重掃，都可能對同一筆 intent 再丟一次 job；queue 裡最多排一份。
func TestMemoryQueue_EnqueueIsIdempotentWhilePending(t *testing.T) {
	ctx := context.Background()
	q := NewMemoryQueue()
	mustEnqueue(t, q, settle("pi_0001"), t0)
	applied, err := q.Enqueue(ctx, settle("pi_0001"), t0.Add(time.Second))
	if err != nil || applied {
		t.Fatalf("second enqueue: applied=%v err=%v", applied, err)
	}
	mustLease(t, q, t0, 30*time.Second)
	applied, err = q.Enqueue(ctx, settle("pi_0001"), t0.Add(2*time.Second))
	if err != nil || applied {
		t.Fatalf("enqueue while leased: applied=%v err=%v", applied, err)
	}
	if n, _ := q.Len(ctx); n != 1 {
		t.Fatalf("len: want 1, got %d", n)
	}
}

// TestMemoryQueue_LeaseHidesUntilExpiry：領走的 job 在 lease 期間對別人隱形；期限一過就再度可見、attempt 加一。
// 這就是「worker 死了，工作不會跟著消失」的機制。
func TestMemoryQueue_LeaseHidesUntilExpiry(t *testing.T) {
	ctx := context.Background()
	q := NewMemoryQueue()
	mustEnqueue(t, q, settle("pi_0001"), t0)

	first := mustLease(t, q, t0, 30*time.Second)
	if first.Attempt != 1 || first.LeaseUntil != t0.Add(30*time.Second) {
		t.Fatalf("first delivery: %+v", first)
	}
	if _, ok, _ := q.Lease(ctx, t0.Add(29*time.Second), 30*time.Second); ok {
		t.Fatal("job should be invisible while leased")
	}
	second := mustLease(t, q, t0.Add(30*time.Second), 30*time.Second)
	if second.Attempt != 2 || second.Receipt == first.Receipt {
		t.Fatalf("second delivery: %+v", second)
	}
}

// TestMemoryQueue_AckWithStaleReceiptIsRejected：lease 過期後 job 被別人領走，原本那個 worker 醒來想 Ack，
// 憑證已經作廢。不然它會把別人正在做的工作刪掉，那份工作就真的丟了。
func TestMemoryQueue_AckWithStaleReceiptIsRejected(t *testing.T) {
	ctx := context.Background()
	q := NewMemoryQueue()
	mustEnqueue(t, q, settle("pi_0001"), t0)
	first := mustLease(t, q, t0, 30*time.Second)
	second := mustLease(t, q, t0.Add(time.Minute), 30*time.Second)

	if err := q.Ack(ctx, first); !errors.Is(err, ErrStaleReceipt) {
		t.Fatalf("stale ack: want ErrStaleReceipt, got %v", err)
	}
	if err := q.Nack(ctx, first, time.Second, t0.Add(time.Minute)); !errors.Is(err, ErrStaleReceipt) {
		t.Fatalf("stale nack: want ErrStaleReceipt, got %v", err)
	}
	if err := q.Ack(ctx, second); err != nil {
		t.Fatalf("current ack: %v", err)
	}
	if err := q.Ack(ctx, second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ack twice: want ErrNotFound, got %v", err)
	}
}

// TestMemoryQueue_NackDelaysRedelivery：Nack 之後 job 不是馬上回來，要等 retryAfter；attempt 計次保留，
// 之後 worker 看得出這份工作已經試過幾次。
func TestMemoryQueue_NackDelaysRedelivery(t *testing.T) {
	ctx := context.Background()
	q := NewMemoryQueue()
	mustEnqueue(t, q, settle("pi_0001"), t0)
	d := mustLease(t, q, t0, 30*time.Second)
	if err := q.Nack(ctx, d, 5*time.Second, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := q.Lease(ctx, t0.Add(5*time.Second), 30*time.Second); ok {
		t.Fatal("job should stay hidden until retryAfter has passed")
	}
	again := mustLease(t, q, t0.Add(6*time.Second), 30*time.Second)
	if again.Attempt != 2 {
		t.Fatalf("attempt after nack: want 2, got %d", again.Attempt)
	}
}

// TestMemoryQueue_AckForgetsTheJob：Ack 之後同 ID 再 Enqueue 是新的一份工作，attempt 從 1 重來。
// 去重的範圍只有「還在 queue 裡」；reorg 之後要 relayer 重送，就是這樣再排一次。安全不靠這裡，靠 worker 冪等。
func TestMemoryQueue_AckForgetsTheJob(t *testing.T) {
	ctx := context.Background()
	q := NewMemoryQueue()
	mustEnqueue(t, q, settle("pi_0001"), t0)
	d := mustLease(t, q, t0, 30*time.Second)
	if err := q.Ack(ctx, d); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.Len(ctx); n != 0 {
		t.Fatalf("len after ack: want 0, got %d", n)
	}
	mustEnqueue(t, q, settle("pi_0001"), t0.Add(time.Minute))
	again := mustLease(t, q, t0.Add(time.Minute), 30*time.Second)
	if again.Attempt != 1 {
		t.Fatalf("attempt of re-enqueued job: want 1, got %d", again.Attempt)
	}
}

// TestMemoryQueue_LeaseIsFIFOAmongVisible：先進先出，但只在「現在可見的」之間排：被 Nack 延後的排在後面，
// 不會擋住後來的 job。
func TestMemoryQueue_LeaseIsFIFOAmongVisible(t *testing.T) {
	ctx := context.Background()
	q := NewMemoryQueue()
	for _, id := range []string{"pi_0001", "pi_0002", "pi_0003"} {
		mustEnqueue(t, q, settle(id), t0)
	}
	first := mustLease(t, q, t0, 30*time.Second)
	if first.Job.IntentID != "pi_0001" {
		t.Fatalf("first lease: got %s", first.Job.IntentID)
	}
	if err := q.Nack(ctx, first, time.Minute, t0); err != nil {
		t.Fatal(err)
	}
	if d := mustLease(t, q, t0, 30*time.Second); d.Job.IntentID != "pi_0002" {
		t.Fatalf("after nack: got %s, want pi_0002", d.Job.IntentID)
	}
	if d := mustLease(t, q, t0, 30*time.Second); d.Job.IntentID != "pi_0003" {
		t.Fatalf("third lease: got %s, want pi_0003", d.Job.IntentID)
	}
	if _, ok, _ := q.Lease(ctx, t0, 30*time.Second); ok {
		t.Fatal("pi_0001 should still be hidden")
	}
}

// TestMemoryQueue_ConcurrentLeasesExactlyOne：五十個 worker 同時來領同一份 job，只有一個拿得到。
// 這是 queue 唯一必須原子的地方；拿不到的照常拿到 ok=false，不是錯誤。
func TestMemoryQueue_ConcurrentLeasesExactlyOne(t *testing.T) {
	ctx := context.Background()
	q := NewMemoryQueue()
	mustEnqueue(t, q, settle("pi_0001"), t0)

	const workers = 50
	var wg sync.WaitGroup
	got := make(chan Delivery, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if d, ok, err := q.Lease(ctx, t0, 30*time.Second); err != nil {
				t.Error(err)
			} else if ok {
				got <- d
			}
		}()
	}
	close(start)
	wg.Wait()
	close(got)
	n := 0
	for range got {
		n++
	}
	if n != 1 {
		t.Fatalf("leases handed out: want 1, got %d", n)
	}
}

package dlq

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
)

var (
	t0   = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	refA = paymentref.Derive(paymentref.Terms{IntentID: "pi_0001", Chain: "evm:31337",
		Token: "0x5FbDB2315678afecb367f032d93F642f64180aa3", Payer: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
		Merchant: "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC", Amount: "100000000"})
)

func settle(id string) queue.Job {
	return queue.Job{ID: id + "/settle", Kind: queue.KindSettle, IntentID: id, Ref: refA}
}

func parked(id, state string) Record {
	return Record{Job: settle(id), Attempts: 3, Reason: "no luck after 3 deliveries", IntentState: state}
}

func mustPark(t *testing.T, s Store, r Record, now time.Time) Record {
	t.Helper()
	applied, err := s.Park(context.Background(), r, now)
	if err != nil || !applied {
		t.Fatalf("park %s: applied=%v err=%v", r.Job.ID, applied, err)
	}
	got, err := s.Get(context.Background(), r.Job.ID)
	if err != nil {
		t.Fatalf("get %s: %v", r.Job.ID, err)
	}
	return got
}

// TestRecord_Validate：缺 job 欄位不收，缺理由也不收。一份沒有理由的放棄，人打開來也不知道該做什麼。
func TestRecord_Validate(t *testing.T) {
	good := parked("pi_0001", "needs_review")
	if err := good.Validate(); err != nil {
		t.Fatalf("good record: %v", err)
	}
	for name, mutate := range map[string]func(*Record){
		"no job id": func(r *Record) { r.Job.ID = "" },
		"no ref":    func(r *Record) { r.Job.Ref = paymentref.Ref{} },
		"no reason": func(r *Record) { r.Reason = "" },
	} {
		r := good
		mutate(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: want ErrInvalidRecord, got %v", name, err)
		}
	}
}

// TestRecord_String：文章與運維畫面直接貼這個格式，欄位數不能隨著狀態變。還沒有人處置的那一欄印 -。
func TestRecord_String(t *testing.T) {
	s := NewMemoryStore()
	r := mustPark(t, s, parked("pi_0001", "needs_review"), t0)
	line := r.String()
	if !strings.HasPrefix(line, "pi_0001/settle   #3  parked   needs_review -    ") {
		t.Fatalf("parked line: %q", line)
	}
	done, err := s.Resolve(context.Background(), "pi_0001/settle", StatusRedriven, "ops", t0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.HasPrefix(done.String(), "pi_0001/settle   #3  redriven needs_review ops  ") {
		t.Fatalf("resolved line: %q", done.String())
	}
}

// TestMemoryStore_ParkIsIdempotentWhileParked：去重的範圍只有「還停著」，跟 queue.Enqueue 的規矩一樣。
// 同一份 job 被重新交付、又被判死一次，不該在收容所裡長出第二列。
func TestMemoryStore_ParkIsIdempotentWhileParked(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	first := mustPark(t, s, parked("pi_0001", "created"), t0)

	again := parked("pi_0001", "settling")
	again.Reason = "a different reason"
	applied, err := s.Park(ctx, again, t0.Add(time.Minute))
	if err != nil || applied {
		t.Fatalf("second park: applied=%v err=%v", applied, err)
	}
	got, _ := s.Get(ctx, "pi_0001/settle")
	if got.Reason != first.Reason || got.IntentState != "created" || got.Cycles != 1 {
		t.Fatalf("the first parking should win: %+v", got)
	}
	all, _ := s.List(ctx, "")
	if len(all) != 1 {
		t.Fatalf("want one record, got %d", len(all))
	}
}

// TestMemoryStore_ParkAfterAResolveStartsANewCycle：放回去又回來的東西要看得出來它是第二趟，
// 那是「別再按 redrive 了」的訊號。
func TestMemoryStore_ParkAfterAResolveStartsANewCycle(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	mustPark(t, s, parked("pi_0001", "created"), t0)
	if _, err := s.Resolve(ctx, "pi_0001/settle", StatusRedriven, "ops", t0); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second := mustPark(t, s, parked("pi_0001", "settling"), t0.Add(time.Hour))
	if second.Cycles != 2 || second.Status != StatusParked || second.ResolvedBy != "" {
		t.Fatalf("second cycle: %+v", second)
	}
	if second.IntentState != "settling" || !second.ParkedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("the new parking should carry the new facts: %+v", second)
	}
}

// TestMemoryStore_ResolveOnlyOnce：Resolve 是這個 package 唯一的原子點。第二個按下去的人要知道自己晚了，
// 不能靜靜地成功。
func TestMemoryStore_ResolveOnlyOnce(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	mustPark(t, s, parked("pi_0001", "created"), t0)
	if _, err := s.Resolve(ctx, "pi_0001/settle", StatusRedriven, "ops", t0); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	_, err := s.Resolve(ctx, "pi_0001/settle", StatusDropped, "someone-else", t0)
	if !errors.Is(err, ErrNotParked) {
		t.Fatalf("want ErrNotParked, got %v", err)
	}
	got, _ := s.Get(ctx, "pi_0001/settle")
	if got.Status != StatusRedriven || got.ResolvedBy != "ops" {
		t.Fatalf("the first resolve should stand: %+v", got)
	}
}

// TestMemoryStore_ResolveRejectsBadTargets：只能改成 redriven 或 dropped，而且一定要有人簽名。
// 沒有簽名的處置等於沒有人負責。
func TestMemoryStore_ResolveRejectsBadTargets(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	mustPark(t, s, parked("pi_0001", "created"), t0)
	for name, call := range map[string]func() error{
		"back to parked": func() error {
			_, err := s.Resolve(ctx, "pi_0001/settle", StatusParked, "ops", t0)
			return err
		},
		"unknown status": func() error {
			_, err := s.Resolve(ctx, "pi_0001/settle", "shredded", "ops", t0)
			return err
		},
		"nobody signed": func() error {
			_, err := s.Resolve(ctx, "pi_0001/settle", StatusDropped, "", t0)
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: want ErrInvalidRecord, got %v", name, err)
		}
	}
	if _, err := s.Resolve(ctx, "pi_9999/settle", StatusDropped, "ops", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown job: want ErrNotFound, got %v", err)
	}
}

// TestMemoryStore_ListFiltersAndKeepsParkOrder：運維畫面要的是「還停著的有哪些」，順序是進來的先後。
func TestMemoryStore_ListFiltersAndKeepsParkOrder(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	for _, id := range []string{"pi_0003", "pi_0001", "pi_0002"} {
		mustPark(t, s, parked(id, "created"), t0)
	}
	if _, err := s.Resolve(ctx, "pi_0001/settle", StatusDropped, "ops", t0); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	still, _ := s.List(ctx, StatusParked)
	if len(still) != 2 || still[0].Job.ID != "pi_0003/settle" || still[1].Job.ID != "pi_0002/settle" {
		t.Fatalf("parked list: %v", still)
	}
	gone, _ := s.List(ctx, StatusDropped)
	if len(gone) != 1 || gone[0].Job.ID != "pi_0001/settle" {
		t.Fatalf("dropped list: %v", gone)
	}
	if all, _ := s.List(ctx, ""); len(all) != 3 {
		t.Fatalf("empty status should list everything, got %d", len(all))
	}
}

// TestMemoryStore_GetUnknownJob：問一份沒停過的 job 要拿到 ErrNotFound，不是一列空白紀錄。
func TestMemoryStore_GetUnknownJob(t *testing.T) {
	if _, err := NewMemoryStore().Get(context.Background(), "pi_9999/settle"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestMemoryStore_ConcurrentResolveHasOneWinner：釘的是「兩個人同時按下去只有一個算數」，
// 這是 redrive 敢直接放回 queue 的前提。
func TestMemoryStore_ConcurrentResolveHasOneWinner(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	mustPark(t, s, parked("pi_0001", "created"), t0)

	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Resolve(ctx, "pi_0001/settle", StatusRedriven, "ops", t0); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("want exactly one winner, got %d", won)
	}
}

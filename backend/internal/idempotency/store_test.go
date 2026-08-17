package idempotency

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

var (
	t0     = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx    = context.Background()
	scopeA = Scope("merchant-a")
	scopeB = Scope("merchant-b")
	fpA    = FingerprintOf("POST", "/v1/payment_intents", []byte(`{"amount":"100"}`))
	fpB    = FingerprintOf("POST", "/v1/payment_intents", []byte(`{"amount":"999"}`))
	okResp = Response{Status: 201, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"id":"pi_1"}`)}
)

func testPolicy() Policy { return Policy{TTL: 24 * time.Hour, Lease: 30 * time.Second} }

func mustClaim(t *testing.T, s Store, scope Scope, key Key, fp Fingerprint, now time.Time) (Record, Outcome) {
	t.Helper()
	rec, out, err := s.Claim(ctx, scope, key, fp, now, testPolicy())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return rec, out
}

// TestMemoryStore_FirstClaimIsFresh：沒見過的 key 拿到 fresh，紀錄是 in_flight、attempt 1、lease 與 TTL 都設好。
func TestMemoryStore_FirstClaimIsFresh(t *testing.T) {
	s := NewMemoryStore()
	rec, out := mustClaim(t, s, scopeA, "k1", fpA, t0)
	if out != OutcomeFresh {
		t.Fatalf("outcome = %s, want fresh", out)
	}
	if rec.Status != StatusInFlight || rec.Attempt != 1 || rec.Response != nil {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.LeaseUntil != t0.Add(30*time.Second) || rec.ExpiresAt != t0.Add(24*time.Hour) {
		t.Fatalf("lease/ttl wrong: %+v", rec)
	}
}

// TestMemoryStore_ReplayAfterComplete：同 key 同 fingerprint、已有答案，拿到 replay 與那份答案；
// 拿到的是拷貝，改它不會動到 store。
func TestMemoryStore_ReplayAfterComplete(t *testing.T) {
	s := NewMemoryStore()
	rec, _ := mustClaim(t, s, scopeA, "k1", fpA, t0)
	if err := s.Complete(ctx, scopeA, "k1", rec.Attempt, okResp, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, out := mustClaim(t, s, scopeA, "k1", fpA, t0.Add(time.Minute))
	if out != OutcomeReplay || got.Response == nil || got.Response.Status != 201 || string(got.Response.Body) != `{"id":"pi_1"}` {
		t.Fatalf("outcome=%s record=%+v", out, got)
	}
	got.Response.Body[0] = 'X'
	got.Response.Header.Set("Content-Type", "text/plain")
	again, _ := mustClaim(t, s, scopeA, "k1", fpA, t0.Add(time.Minute))
	if string(again.Response.Body) != `{"id":"pi_1"}` || again.Response.Header.Get("Content-Type") != "application/json" {
		t.Fatal("store leaked its internal response")
	}
}

// TestMemoryStore_InFlightUntilCompleted：原請求還在跑，第二個同 key 請求拿到 in_flight，不會被當成新的。
func TestMemoryStore_InFlightUntilCompleted(t *testing.T) {
	s := NewMemoryStore()
	mustClaim(t, s, scopeA, "k1", fpA, t0)
	_, out := mustClaim(t, s, scopeA, "k1", fpA, t0.Add(time.Second))
	if out != OutcomeInFlight {
		t.Fatalf("outcome = %s, want in_flight", out)
	}
	if s.Len() != 1 {
		t.Fatalf("len = %d", s.Len())
	}
}

// TestMemoryStore_MismatchBeatsEverything：同 key 不同 fingerprint，不管原請求在跑還是跑完，一律 mismatch。
// 這是客戶端拿舊訂單的 key 付新訂單，要大聲拒絕。
func TestMemoryStore_MismatchBeatsEverything(t *testing.T) {
	s := NewMemoryStore()
	rec, _ := mustClaim(t, s, scopeA, "k1", fpA, t0)
	if _, out := mustClaim(t, s, scopeA, "k1", fpB, t0); out != OutcomeMismatch {
		t.Fatalf("in-flight mismatch: outcome = %s", out)
	}
	_ = s.Complete(ctx, scopeA, "k1", rec.Attempt, okResp, t0)
	if _, out := mustClaim(t, s, scopeA, "k1", fpB, t0); out != OutcomeMismatch {
		t.Fatalf("completed mismatch: outcome = %s", out)
	}
	// mismatch 不會弄壞原本的紀錄：正確的 fingerprint 還是能重放
	if _, out := mustClaim(t, s, scopeA, "k1", fpA, t0); out != OutcomeReplay {
		t.Fatalf("after mismatch, correct fingerprint: outcome = %s", out)
	}
}

// TestMemoryStore_ScopeIsolatesKeys：兩個 merchant 用同一個 key 字串互不影響。
// 沒有 scope 的話，任何人都能用你的 key 讀到你的答案、或用同 key 不同 body 把你的 key 卡成 422。
func TestMemoryStore_ScopeIsolatesKeys(t *testing.T) {
	s := NewMemoryStore()
	recA, outA := mustClaim(t, s, scopeA, "order-1", fpA, t0)
	_, outB := mustClaim(t, s, scopeB, "order-1", fpB, t0)
	if outA != OutcomeFresh || outB != OutcomeFresh {
		t.Fatalf("both scopes should be fresh: %s %s", outA, outB)
	}
	_ = s.Complete(ctx, scopeA, "order-1", recA.Attempt, okResp, t0)
	if _, out := mustClaim(t, s, scopeB, "order-1", fpB, t0); out != OutcomeInFlight {
		t.Fatalf("scope B unaffected by scope A completing: %s", out)
	}
}

// TestMemoryStore_ExpiredRecordIsForgotten：過了 TTL 同一個 key 是全新的請求，連 fingerprint 不同也不算 mismatch。
// 這就是為什麼 key 不能當永久身分：客戶端可以在 24 小時後拿同一個 key 做別的事。
func TestMemoryStore_ExpiredRecordIsForgotten(t *testing.T) {
	s := NewMemoryStore()
	rec, _ := mustClaim(t, s, scopeA, "k1", fpA, t0)
	_ = s.Complete(ctx, scopeA, "k1", rec.Attempt, okResp, t0)

	if _, out := mustClaim(t, s, scopeA, "k1", fpB, t0.Add(24*time.Hour-time.Second)); out != OutcomeMismatch {
		t.Fatalf("just before ttl: %s", out)
	}
	got, out := mustClaim(t, s, scopeA, "k1", fpB, t0.Add(24*time.Hour))
	if out != OutcomeFresh || got.Attempt != 1 || got.Fingerprint != fpB {
		t.Fatalf("at ttl the key should be brand new: outcome=%s rec=%+v", out, got)
	}
	// Sweep 只清過期的
	mustClaim(t, s, scopeA, "k2", fpA, t0.Add(24*time.Hour))
	if n := s.Sweep(t0.Add(48 * time.Hour)); n != 2 || s.Len() != 0 {
		t.Fatalf("sweep removed %d, len %d", n, s.Len())
	}
}

// TestMemoryStore_LeaseExpiryAllowsTakeover：原 worker 撐過 lease 沒交答案（多半是掛了），下一次重試可以接手；
// 舊 worker 之後醒來想 Complete，attempt 已經不對，拿到 ErrStaleClaim，蓋不掉新的結果。
func TestMemoryStore_LeaseExpiryAllowsTakeover(t *testing.T) {
	s := NewMemoryStore()
	first, _ := mustClaim(t, s, scopeA, "k1", fpA, t0)

	if _, out := mustClaim(t, s, scopeA, "k1", fpA, t0.Add(29*time.Second)); out != OutcomeInFlight {
		t.Fatalf("within lease: %s", out)
	}
	second, out := mustClaim(t, s, scopeA, "k1", fpA, t0.Add(30*time.Second))
	if out != OutcomeFresh || second.Attempt != 2 || second.LeaseUntil != t0.Add(60*time.Second) {
		t.Fatalf("at lease expiry the retry should take over: outcome=%s rec=%+v", out, second)
	}
	if err := s.Complete(ctx, scopeA, "k1", first.Attempt, Response{Status: 500}, t0.Add(31*time.Second)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale worker must not write: %v", err)
	}
	if err := s.Complete(ctx, scopeA, "k1", second.Attempt, okResp, t0.Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, out := mustClaim(t, s, scopeA, "k1", fpA, t0.Add(32*time.Second))
	if out != OutcomeReplay || got.Response.Status != 201 {
		t.Fatalf("the takeover's answer should be the one replayed: %s %+v", out, got.Response)
	}
	// 同一個 attempt 寫第二次不覆蓋
	_ = s.Complete(ctx, scopeA, "k1", second.Attempt, Response{Status: 500}, t0.Add(33*time.Second))
	got, _ = mustClaim(t, s, scopeA, "k1", fpA, t0.Add(34*time.Second))
	if got.Response.Status != 201 {
		t.Fatal("a completed answer must not be overwritten")
	}
	if err := s.Complete(ctx, scopeA, "nope", 1, okResp, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestMemoryStore_ConcurrentClaimsExactlyOneFresh：一百個同 key 請求同時到，只有一個拿到 fresh。
// 這是「客戶端 timeout 後狂重送、或 load balancer 重放」在 store 這一層的長相。
func TestMemoryStore_ConcurrentClaimsExactlyOneFresh(t *testing.T) {
	s := NewMemoryStore()
	const n = 100
	var wg sync.WaitGroup
	outcomes := make([]Outcome, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, out, err := s.Claim(ctx, scopeA, "k1", fpA, t0, testPolicy())
			if err != nil {
				t.Error(err)
			}
			outcomes[i] = out
		}(i)
	}
	wg.Wait()
	fresh, inflight := 0, 0
	for _, o := range outcomes {
		switch o {
		case OutcomeFresh:
			fresh++
		case OutcomeInFlight:
			inflight++
		default:
			t.Errorf("unexpected outcome %s", o)
		}
	}
	if fresh != 1 || inflight != n-1 {
		t.Fatalf("fresh=%d in_flight=%d", fresh, inflight)
	}
}

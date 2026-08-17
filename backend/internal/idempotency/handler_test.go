package idempotency

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingHandler 每被叫到一次就加一，回 201 與一個帶流水號的 body，這樣重放與重跑一眼分得出來。
type countingHandler struct {
	calls  atomic.Int64
	status int
	// gate 非 nil 時，每次呼叫先經過它（帶這是第幾次），測試用它讓某一次卡住，製造 in-flight。
	gate func(call int64)
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := h.calls.Add(1)
	if h.gate != nil {
		h.gate(n)
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", "/v1/things/"+string(rune('0'+n)))
	status := h.status
	if status == 0 {
		status = http.StatusCreated
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"call":` + string(rune('0'+n)) + `,"echo":` + string(body) + `}`))
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestHandler(next http.Handler) (http.Handler, *clock, *MemoryStore) {
	c := &clock{t: t0}
	s := NewMemoryStore()
	return Handler(Options{Store: s, Now: c.now}, next), c, s
}

func post(h http.Handler, key, scope, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/things", strings.NewReader(body))
	if key != "" {
		req.Header.Set(HeaderKey, key)
	}
	if scope != "" {
		req.Header.Set("Authorization", "Bearer "+scope)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func errCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var e ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatalf("body is not an error json: %s", rr.Body.String())
	}
	return e.Error
}

// TestHandler_ReplayReturnsTheSameAnswer：同 key 同 body 送三次，handler 只跑一次，
// 三次的狀態碼、body、Location 都一樣，第二次起多一個 Idempotent-Replayed: true。
func TestHandler_ReplayReturnsTheSameAnswer(t *testing.T) {
	next := &countingHandler{}
	h, _, _ := newTestHandler(next)

	first := post(h, "k1", "m1", `{"amount":"100"}`)
	if first.Code != 201 || first.Header().Get(HeaderReplayed) != "" {
		t.Fatalf("first: code=%d replayed=%q", first.Code, first.Header().Get(HeaderReplayed))
	}
	for i := 0; i < 2; i++ {
		again := post(h, "k1", "m1", `{"amount":"100"}`)
		if again.Code != 201 || again.Body.String() != first.Body.String() {
			t.Fatalf("replay %d differs: %d %s", i, again.Code, again.Body.String())
		}
		if again.Header().Get(HeaderReplayed) != "true" || again.Header().Get("Location") != first.Header().Get("Location") {
			t.Fatalf("replay %d headers: %v", i, again.Header())
		}
	}
	if next.calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", next.calls.Load())
	}
}

// TestHandler_MissingOrInvalidKeyIs400：POST 沒帶 key 就是 400；key 語法不對也是 400；都不會跑 handler。
func TestHandler_MissingOrInvalidKeyIs400(t *testing.T) {
	next := &countingHandler{}
	h, _, _ := newTestHandler(next)
	if rr := post(h, "", "m1", `{}`); rr.Code != 400 || errCode(t, rr) != "idempotency_key_required" {
		t.Fatalf("missing key: %d %s", rr.Code, rr.Body.String())
	}
	if rr := post(h, "has space", "m1", `{}`); rr.Code != 400 || errCode(t, rr) != "idempotency_key_invalid" {
		t.Fatalf("invalid key: %d %s", rr.Code, rr.Body.String())
	}
	if next.calls.Load() != 0 {
		t.Fatal("handler must not run without a valid key")
	}
}

// TestHandler_NoScopeIs401：認不出主人的 key 不收。沒有 scope，任何人都能用同一個 key 讀到別人的答案。
func TestHandler_NoScopeIs401(t *testing.T) {
	next := &countingHandler{}
	h, _, _ := newTestHandler(next)
	if rr := post(h, "k1", "", `{}`); rr.Code != 401 || errCode(t, rr) != "unauthorized" {
		t.Fatalf("no auth: %d %s", rr.Code, rr.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/things", strings.NewReader(`{}`))
	req.Header.Set(HeaderKey, "k1")
	req.Header.Set("Authorization", "Basic abc")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("non-bearer auth: %d", rr.Code)
	}
	if next.calls.Load() != 0 {
		t.Fatal("handler must not run without a scope")
	}
}

// TestHandler_ScopeIsolatesKeys：兩個 merchant 用同一個 key，各跑一次、各拿各的答案。
func TestHandler_ScopeIsolatesKeys(t *testing.T) {
	next := &countingHandler{}
	h, _, _ := newTestHandler(next)
	a := post(h, "order-1", "m1", `{"amount":"100"}`)
	b := post(h, "order-1", "m2", `{"amount":"100"}`)
	if a.Code != 201 || b.Code != 201 || a.Body.String() == b.Body.String() {
		t.Fatalf("a=%s b=%s", a.Body.String(), b.Body.String())
	}
	if b.Header().Get(HeaderReplayed) != "" {
		t.Fatal("scope B must not see scope A's answer")
	}
	if next.calls.Load() != 2 {
		t.Fatalf("handler ran %d times, want 2", next.calls.Load())
	}
}

// TestHandler_MismatchIs422：同 key 不同 body 回 422，handler 不跑；原本的答案還在，正確的 body 仍可重放。
func TestHandler_MismatchIs422(t *testing.T) {
	next := &countingHandler{}
	h, _, _ := newTestHandler(next)
	post(h, "k1", "m1", `{"amount":"100"}`)
	rr := post(h, "k1", "m1", `{"amount":"999"}`)
	if rr.Code != 422 || errCode(t, rr) != "idempotency_key_reused" {
		t.Fatalf("mismatch: %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get(HeaderReplayed) != "" {
		t.Fatal("a 422 is not a replay")
	}
	again := post(h, "k1", "m1", `{"amount":"100"}`)
	if again.Code != 201 || again.Header().Get(HeaderReplayed) != "true" {
		t.Fatalf("original still replayable: %d %v", again.Code, again.Header())
	}
	if next.calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", next.calls.Load())
	}
}

// TestHandler_InFlightIs409：第一個請求還在 handler 裡，第二個同 key 請求拿 409 與 Retry-After；
// 第一個做完後，第三個請求拿到重放。handler 從頭到尾只跑一次。
func TestHandler_InFlightIs409(t *testing.T) {
	block, entered := make(chan struct{}), make(chan struct{})
	next := &countingHandler{gate: func(call int64) {
		if call == 1 {
			close(entered)
			<-block
		}
	}}
	h, _, _ := newTestHandler(next)

	firstDone := make(chan *httptest.ResponseRecorder)
	go func() { firstDone <- post(h, "k1", "m1", `{"amount":"100"}`) }()
	<-entered
	second := post(h, "k1", "m1", `{"amount":"100"}`)
	if second.Code != 409 || errCode(t, second) != "idempotency_key_in_flight" || second.Header().Get("Retry-After") == "" {
		t.Fatalf("in flight: %d %v %s", second.Code, second.Header(), second.Body.String())
	}
	close(block)
	first := <-firstDone
	third := post(h, "k1", "m1", `{"amount":"100"}`)
	if first.Code != 201 || third.Code != 201 || third.Body.String() != first.Body.String() || third.Header().Get(HeaderReplayed) != "true" {
		t.Fatalf("first=%d third=%d %s", first.Code, third.Code, third.Body.String())
	}
	if next.calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", next.calls.Load())
	}
}

// TestHandler_HandlerErrorsAreStoredToo：handler 回 500 也存、也重放。500 代表「不知道有沒有做」，
// 讓客戶端每次重試都看到同一個 500，比讓它換 key 重來（可能做第二次）安全。跟 Stripe 的公開行為一致。
func TestHandler_HandlerErrorsAreStoredToo(t *testing.T) {
	next := &countingHandler{status: 500}
	h, _, _ := newTestHandler(next)
	first := post(h, "k1", "m1", `{}`)
	again := post(h, "k1", "m1", `{}`)
	if first.Code != 500 || again.Code != 500 || again.Header().Get(HeaderReplayed) != "true" || again.Body.String() != first.Body.String() {
		t.Fatalf("first=%d again=%d replayed=%q", first.Code, again.Code, again.Header().Get(HeaderReplayed))
	}
	if next.calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", next.calls.Load())
	}
}

// TestHandler_ExpiredKeyRunsAgain：24 小時後同一個 key 是新的一次，handler 再跑一次、拿到新答案。
func TestHandler_ExpiredKeyRunsAgain(t *testing.T) {
	next := &countingHandler{}
	h, c, _ := newTestHandler(next)
	first := post(h, "k1", "m1", `{}`)
	c.advance(24 * time.Hour)
	second := post(h, "k1", "m1", `{}`)
	if second.Header().Get(HeaderReplayed) != "" || second.Body.String() == first.Body.String() {
		t.Fatalf("after ttl the key must be fresh: %v %s", second.Header(), second.Body.String())
	}
	if next.calls.Load() != 2 {
		t.Fatalf("handler ran %d times, want 2", next.calls.Load())
	}
}

// TestHandler_LeaseTakeoverStoresTheSecondAnswer：第一次執行卡住超過 lease，重試接手並存下自己的答案；
// 卡住的那一次醒來後，它的答案寫不回去，那個客戶端拿到 409（一個 key 只有一個答案會離開伺服器）。
// 之後重放的是接手那一次的答案。
func TestHandler_LeaseTakeoverStoresTheSecondAnswer(t *testing.T) {
	block, entered := make(chan struct{}), make(chan struct{})
	next := &countingHandler{gate: func(call int64) {
		if call == 1 { // 只有第一次卡住；接手的那一次直接跑完
			close(entered)
			<-block
		}
	}}
	h, c, _ := newTestHandler(next)

	firstDone := make(chan *httptest.ResponseRecorder)
	go func() { firstDone <- post(h, "k1", "m1", `{}`) }()
	<-entered
	c.advance(31 * time.Second)
	second := post(h, "k1", "m1", `{}`)
	if second.Code != 201 || second.Header().Get(HeaderReplayed) != "" {
		t.Fatalf("takeover should run the handler: %d %v", second.Code, second.Header())
	}
	close(block)
	first := <-firstDone
	if first.Code != 409 || errCode(t, first) != "idempotency_attempt_superseded" {
		t.Fatalf("the stale attempt must not hand out its answer: %d %s", first.Code, first.Body.String())
	}
	third := post(h, "k1", "m1", `{}`)
	if third.Code != 201 || third.Body.String() != second.Body.String() || third.Header().Get(HeaderReplayed) != "true" {
		t.Fatalf("replay should be the takeover's answer: second=%s third=%d %s",
			second.Body.String(), third.Code, third.Body.String())
	}
	if next.calls.Load() != 2 {
		t.Fatalf("handler ran %d times, want 2", next.calls.Load())
	}
}

// TestHandler_GetPassesThrough：GET 不需要 key、不需要 scope，直接到 handler。
func TestHandler_GetPassesThrough(t *testing.T) {
	next := &countingHandler{}
	h, _, _ := newTestHandler(next)
	req := httptest.NewRequest(http.MethodGet, "/v1/things/1", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 201 || next.calls.Load() != 1 {
		t.Fatalf("get: %d calls=%d", rr.Code, next.calls.Load())
	}
}

// TestHandler_BodyIsStillReadableByNext：去重層讀完 body 算 fingerprint 之後，handler 還是讀得到同一份 body。
func TestHandler_BodyIsStillReadableByNext(t *testing.T) {
	next := &countingHandler{}
	h, _, _ := newTestHandler(next)
	rr := post(h, "k1", "m1", `{"amount":"100"}`)
	if !strings.Contains(rr.Body.String(), `"echo":{"amount":"100"}`) {
		t.Fatalf("handler did not see the body: %s", rr.Body.String())
	}
	big := strings.Repeat("x", MaxBodyBytes+1)
	if rr := post(h, "k2", "m1", big); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", rr.Code)
	}
}

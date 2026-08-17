package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/idempotency"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const goodBody = `{"chain":"evm:31337","token":"0x5FbDB2315678afecb367f032d93F642f64180aa3",` +
	`"payer":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8","merchant":"0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",` +
	`"amount":"100000000","expires_in_seconds":900}`

type fixture struct {
	h       http.Handler
	intents *intent.MemoryStore
	idem    *idempotency.MemoryStore
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{intents: intent.NewMemoryStore(), idem: idempotency.NewMemoryStore()}
	f.h = New(f.intents, f.idem, WithClock(func() time.Time { return t0 }))
	return f
}

func (f *fixture) post(key, scope, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents", strings.NewReader(body))
	if key != "" {
		req.Header.Set(idempotency.HeaderKey, key)
	}
	if scope != "" {
		req.Header.Set("Authorization", "Bearer "+scope)
	}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	return rr
}

func (f *fixture) get(id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/payment_intents/"+id, nil)
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	return rr
}

func decodeIntent(t *testing.T, rr *httptest.ResponseRecorder) IntentResponse {
	t.Helper()
	var out IntentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("not an intent json (%d): %s", rr.Code, rr.Body.String())
	}
	return out
}

func decodeError(t *testing.T, rr *httptest.ResponseRecorder) idempotency.ErrorBody {
	t.Helper()
	var out idempotency.ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("not an error json (%d): %s", rr.Code, rr.Body.String())
	}
	return out
}

// countIntents 數 store 裡有幾筆不同的 intent：MemoryStore 沒有 List，用回應裡看到的 id 去 Get。
func countIntents(t *testing.T, s *intent.MemoryStore, ids []string) int {
	t.Helper()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		if _, err := s.Get(context.Background(), id); err == nil {
			seen[id] = true
		}
	}
	return len(seen)
}

// TestCreateIntent_HappyPath：201、Location、body 是 created v1、store 裡真的有這筆。
func TestCreateIntent_HappyPath(t *testing.T) {
	f := newFixture(t)
	rr := f.post("order-1001", "merchant-demo", goodBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("code = %d body = %s", rr.Code, rr.Body.String())
	}
	got := decodeIntent(t, rr)
	if !strings.HasPrefix(got.ID, "pi_") || len(got.ID) != 3+24 || got.State != "created" || got.Version != 1 || got.Amount != "100000000" {
		t.Fatalf("unexpected intent: %+v", got)
	}
	if got.ExpiresAt != t0.Add(15*time.Minute) || got.CreatedAt != t0 {
		t.Fatalf("timestamps: %+v", got)
	}
	if rr.Header().Get("Location") != "/v1/payment_intents/"+got.ID {
		t.Fatalf("location = %q", rr.Header().Get("Location"))
	}
	stored, err := f.intents.Get(context.Background(), got.ID)
	if err != nil || stored.State != intent.StateCreated {
		t.Fatalf("not in store: %v %+v", err, stored)
	}
	if g := f.get(got.ID); g.Code != 200 || decodeIntent(t, g).ID != got.ID {
		t.Fatalf("get: %d %s", g.Code, g.Body.String())
	}
}

// TestCreateIntent_RetryStormCreatesOneIntent：同一個 key 送三次，三次都是 201 且 body 逐 byte 一樣，
// 第二次起帶 Idempotent-Replayed，store 裡只有一筆。這就是「API 收到同一筆請求三次，只長出一筆 intent」。
func TestCreateIntent_RetryStormCreatesOneIntent(t *testing.T) {
	f := newFixture(t)
	var ids []string
	var bodies []string
	for i := 0; i < 3; i++ {
		rr := f.post("order-1001", "merchant-demo", goodBody)
		if rr.Code != 201 {
			t.Fatalf("attempt %d: %d %s", i, rr.Code, rr.Body.String())
		}
		replayed := rr.Header().Get(idempotency.HeaderReplayed) == "true"
		if replayed != (i > 0) {
			t.Fatalf("attempt %d: replayed=%v", i, replayed)
		}
		ids = append(ids, decodeIntent(t, rr).ID)
		bodies = append(bodies, rr.Body.String())
	}
	if ids[0] != ids[1] || ids[1] != ids[2] || bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("responses differ: %v", bodies)
	}
	if countIntents(t, f.intents, ids) != 1 || f.idem.Len() != 1 {
		t.Fatalf("expected exactly one intent and one record")
	}
}

// TestCreateIntent_SameKeyDifferentAmountIs422：客戶端拿舊訂單的 key 付新金額，422，沒有新 intent。
func TestCreateIntent_SameKeyDifferentAmountIs422(t *testing.T) {
	f := newFixture(t)
	first := decodeIntent(t, f.post("order-1001", "merchant-demo", goodBody))
	rr := f.post("order-1001", "merchant-demo", strings.Replace(goodBody, `"100000000"`, `"999000000"`, 1))
	if rr.Code != 422 || decodeError(t, rr).Error != "idempotency_key_reused" {
		t.Fatalf("code = %d body = %s", rr.Code, rr.Body.String())
	}
	if f.get(first.ID).Code != 200 {
		t.Fatal("original intent should still exist")
	}
}

// TestCreateIntent_ConcurrentRetriesCreateOneIntent：五十個同 key 請求同時到，只長出一筆 intent。
// 每個請求拿到 201（第一個、或重放）或 409（撞上還在跑的那個），沒有其他結果。
func TestCreateIntent_ConcurrentRetriesCreateOneIntent(t *testing.T) {
	f := newFixture(t)
	const n = 50
	var wg sync.WaitGroup
	codes := make([]int, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := f.post("order-1001", "merchant-demo", goodBody)
			codes[i] = rr.Code
			if rr.Code == 201 {
				var out IntentResponse
				_ = json.Unmarshal(rr.Body.Bytes(), &out)
				ids[i] = out.ID
			}
		}(i)
	}
	wg.Wait()
	created, conflicts := 0, 0
	seen := map[string]bool{}
	for i, c := range codes {
		switch c {
		case 201:
			created++
			seen[ids[i]] = true
		case 409:
			conflicts++
		default:
			t.Errorf("request %d: unexpected code %d", i, c)
		}
	}
	if created < 1 || created+conflicts != n || len(seen) != 1 {
		t.Fatalf("created=%d conflicts=%d distinct ids=%d", created, conflicts, len(seen))
	}
	if f.idem.Len() != 1 {
		t.Fatalf("records = %d", f.idem.Len())
	}
}

// TestCreateIntent_ValidationErrorIsReplayed：金額為 0 拿 400；同 key 再送一次拿同一個 400 加 Idempotent-Replayed。
// 答案就是答案，錯的答案也重放；客戶端修好 body 之後要換一個新 key。
func TestCreateIntent_ValidationErrorIsReplayed(t *testing.T) {
	f := newFixture(t)
	body := strings.Replace(goodBody, `"100000000"`, `"0"`, 1)
	first := f.post("order-1002", "merchant-demo", body)
	again := f.post("order-1002", "merchant-demo", body)
	if first.Code != 400 || again.Code != 400 || decodeError(t, first).Error != "invalid_request" {
		t.Fatalf("first=%d again=%d %s", first.Code, again.Code, first.Body.String())
	}
	if again.Header().Get(idempotency.HeaderReplayed) != "true" || again.Body.String() != first.Body.String() {
		t.Fatalf("400 should be replayed verbatim: %v %s", again.Header(), again.Body.String())
	}
	// 修好 body 但沿用同一個 key：422，不是 201
	if rr := f.post("order-1002", "merchant-demo", goodBody); rr.Code != 422 {
		t.Fatalf("fixed body with the same key: %d", rr.Code)
	}
	if rr := f.post("order-1003", "merchant-demo", goodBody); rr.Code != 201 {
		t.Fatalf("fixed body with a new key: %d", rr.Code)
	}
}

// TestCreateIntent_RejectsBadInput：不是 JSON、多出來的欄位、金額不是十進位整數字串，都是 400。
func TestCreateIntent_RejectsBadInput(t *testing.T) {
	f := newFixture(t)
	cases := map[string]struct {
		body string
		code string
	}{
		"not json":       {`{`, "invalid_json"},
		"unknown field":  {strings.Replace(goodBody, `"chain"`, `"chainz"`, 1), "invalid_json"},
		"amount number":  {strings.Replace(goodBody, `"100000000"`, `100000000`, 1), "invalid_json"},
		"amount decimal": {strings.Replace(goodBody, `"100000000"`, `"100.5"`, 1), "invalid_request"},
		"amount hex":     {strings.Replace(goodBody, `"100000000"`, `"0x64"`, 1), "invalid_request"},
		"missing payer":  {strings.Replace(goodBody, `"payer":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8"`, `"payer":""`, 1), "invalid_request"},
	}
	i := 0
	for name, c := range cases {
		i++
		rr := f.post(fmt.Sprintf("bad-%d", i), "merchant-demo", c.body)
		if rr.Code != 400 || decodeError(t, rr).Error != c.code {
			t.Errorf("%s: code=%d body=%s", name, rr.Code, rr.Body.String())
		}
	}
}

// TestCreateIntent_RequiresKeyAndScope：沒 key 400、沒憑證 401，兩者都不會建 intent。
func TestCreateIntent_RequiresKeyAndScope(t *testing.T) {
	f := newFixture(t)
	if rr := f.post("", "merchant-demo", goodBody); rr.Code != 400 {
		t.Fatalf("no key: %d", rr.Code)
	}
	if rr := f.post("order-1", "", goodBody); rr.Code != 401 {
		t.Fatalf("no scope: %d", rr.Code)
	}
	if f.idem.Len() != 0 {
		t.Fatal("nothing should have been recorded")
	}
}

// TestGetIntent_NotFound：查不到是 404，GET 不需要 key。
func TestGetIntent_NotFound(t *testing.T) {
	f := newFixture(t)
	if rr := f.get("pi_nope"); rr.Code != 404 || decodeError(t, rr).Error != "not_found" {
		t.Fatalf("get missing: %d %s", rr.Code, rr.Body.String())
	}
}

// TestNewIntentID_Shape：pi_ 加 24 個 hex，而且連續產生不重複。
func TestNewIntentID_Shape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewIntentID()
		if !strings.HasPrefix(id, "pi_") || len(id) != 27 || seen[id] {
			t.Fatalf("bad id %q", id)
		}
		seen[id] = true
	}
}

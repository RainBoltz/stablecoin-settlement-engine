package idempotency

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HeaderKey 是客戶端放 key 的 header，名稱沿用 IETF 草案與 Stripe。
const HeaderKey = "Idempotency-Key"

// HeaderReplayed 在重放的回應上設為 true，客戶端才分得出「這是第一次的答案」還是「剛剛才做的」。
// 名稱沿用 Stripe 的 Idempotent-Replayed。
const HeaderReplayed = "Idempotent-Replayed"

// MaxBodyBytes 是會被讀進記憶體算 fingerprint 的 body 上限。去重層要看完整 body 才能算摘要，
// 所以這裡順便當第一道大小限制。1 MiB 對一筆付款請求綽綽有餘。
const MaxBodyBytes = 1 << 20

// Options 是 Handler 的設定。
type Options struct {
	Store  Store
	Policy Policy
	// Now 由外面注入，測試才能把時鐘往前撥（讓 lease 與 TTL 過期）。零值用 time.Now。
	Now func() time.Time
	// Scope 從 request 算出 key 的主人。回 false 代表認不出來，整個請求以 401 拒絕：
	// 沒有主人的 key 不能收，不然任何人都能用同一個 key 讀到別人的答案。
	Scope func(*http.Request) (Scope, bool)
}

// ScopeFromBearer 拿 Authorization: Bearer <token> 的 token 字串當 scope。
//
// 今天沒有真正的驗證，token 是什麼就是什麼；重點是「scope 從憑證來，不從 body 來」這個形狀。
// 之後接上驗證時，這個函式換成「查憑證、回 merchant id」，去重層其他地方不用動。
func ScopeFromBearer(r *http.Request) (Scope, bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(auth[len(prefix):])
	if tok == "" {
		return "", false
	}
	return Scope(tok), true
}

// Handler 把 next 包成「同一個 (scope, key) 只執行一次」的版本。
//
// 只攔會產生副作用的方法（POST、PUT、PATCH、DELETE 之外的其實只有 POST 與 PATCH 會經過這裡，
// 但寫成「非安全方法都攔」比較不容易漏）。GET 與 HEAD 直接放行：它們本來就是冪等的，
// 帶 key 也沒有意義（Stripe 對 GET 的 key 也是忽略）。
//
// 執行順序與每一步的回應：
//  1. 認不出 scope → 401，不記錄。
//  2. 沒帶 key → 400，不記錄。key 語法不對 → 400，不記錄。
//  3. 讀 body、算 fingerprint、Claim：
//     mismatch → 422、in_flight → 409（帶 Retry-After）、replay → 原答案 + Idempotent-Replayed: true。
//     這三種也都不記錄：Stripe 的原則是「endpoint 沒開始執行就不存結果」，這樣客戶端修正後重送同一個 key 還有機會。
//  4. fresh → 跑 next，把它寫的狀態碼、header、body 整份錄下來，Complete，再原樣回給客戶端。
//     不論 next 回 2xx、4xx 還是 5xx 都存：存的是「答案」，不是「成功的答案」。
//     一個 500 代表「不知道有沒有做」，重放同一個 500 會逼客戶端注意到這件事，
//     比讓它換個 key 重來（可能付第二次）安全。這一點也是照 Stripe 的公開行為。
//  5. Complete 失敗且原因是 ErrStaleClaim（撐過 lease、被接手了）→ 這份答案不回給客戶端，改回 409；
//     一個 key 只有一個答案會離開伺服器。
func Handler(opts Options, next http.Handler) http.Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Scope == nil {
		opts.Scope = ScopeFromBearer
	}
	if opts.Policy == (Policy{}) {
		opts.Policy = DefaultPolicy()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		scope, ok := opts.Scope(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authorization: Bearer <token> is required")
			return
		}
		raw := r.Header.Get(HeaderKey)
		if raw == "" {
			writeError(w, http.StatusBadRequest, "idempotency_key_required", HeaderKey+" header is required for "+r.Method)
			return
		}
		key := Key(raw)
		if err := key.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "idempotency_key_invalid", err.Error())
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "body_unreadable", err.Error())
			return
		}
		if len(body) > MaxBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("max %d bytes", MaxBodyBytes))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		now := opts.Now()
		fp := FingerprintOf(r.Method, r.URL.Path, body)
		rec, outcome, err := opts.Store.Claim(r.Context(), scope, key, fp, now, opts.Policy)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "idempotency_store_unavailable", err.Error())
			return
		}
		switch outcome {
		case OutcomeMismatch:
			writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused",
				fmt.Sprintf("%s %q was already used for a different request (fingerprint %s, got %s)",
					HeaderKey, key, rec.Fingerprint, fp))
			return
		case OutcomeInFlight:
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(rec, now)))
			writeError(w, http.StatusConflict, "idempotency_key_in_flight",
				fmt.Sprintf("a request with %s %q is still being processed", HeaderKey, key))
			return
		case OutcomeReplay:
			replay(w, *rec.Response)
			return
		}

		rec2 := &recorder{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec2, r)
		resp := Response{Status: rec2.status, Header: rec2.header, Body: rec2.body.Bytes()}
		if err := opts.Store.Complete(r.Context(), scope, key, rec.Attempt, resp, opts.Now()); err != nil {
			if errors.Is(err, ErrStaleClaim) {
				// 我們撐過了 lease、已經被別人接手，這份答案作廢。不回給客戶端：一個 key 只能有一個答案離開伺服器，
				// 不然這個客戶端會拿著一個永遠不會被重放的 intent id。它重試就會拿到接手那一次的答案。
				writeError(w, http.StatusConflict, "idempotency_attempt_superseded",
					fmt.Sprintf("this attempt for %s %q exceeded its lease and was taken over; retry to get the recorded answer", HeaderKey, key))
				return
			}
			// 其他寫回失敗（記憶體版不會發生）：答案照給，只是不會被重放。留一行 log。
			log.Printf("idempotency: complete %s/%s attempt %d: %v", scope, key, rec.Attempt, err)
		}
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.Status)
		_, _ = w.Write(resp.Body)
	})
}

// retryAfterSeconds 算 409 要建議客戶端等幾秒：lease 剩多久就等多久，最少 1 秒。
func retryAfterSeconds(rec Record, now time.Time) int {
	secs := int(rec.LeaseUntil.Sub(now).Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

func replay(w http.ResponseWriter, resp Response) {
	copyHeader(w.Header(), resp.Header)
	w.Header().Set(HeaderReplayed, "true")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		dst[k] = append([]string(nil), vs...)
	}
}

// recorder 把 next 寫的東西全部先收進記憶體。不能邊寫邊回給客戶端：答案要先存下來才算數，
// 而且 next 中途出錯時我們要拿到完整的狀態碼與 body 才能決定怎麼記。
type recorder struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
}

func (r *recorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(p)
}

// ErrorBody 是這個 package 自己回的錯誤格式：一個機器可比對的 error 碼，加一句給人看的 detail。
type ErrorBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorBody{Error: code, Detail: detail})
}

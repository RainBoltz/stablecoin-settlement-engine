// Package api 是對外的 Payment API。今天只有兩條路：建立一筆 Payment Intent、查一筆 Payment Intent。
//
// 建立這條路包在 idempotency.Handler 後面：同一個 merchant 用同一個 Idempotency-Key 送幾次，
// 都只會長出一筆 intent、拿到同一份回應。API 本身不知道自己被去重了，它每次被叫到都老實地建一筆；
// 「只叫到一次」是外面那層保證的。這樣切的好處是之後每一個會產生副作用的 endpoint 都能用同一層，
// 而 intent 的建立邏輯也不用管重試。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/idempotency"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
)

// Server 把兩個 store 與一個時鐘綁在一起。
type Server struct {
	intents intent.Store
	idem    idempotency.Store
	now     func() time.Time
	newID   func() string
	policy  idempotency.Policy
}

// Option 調整 Server 的預設值，測試用。
type Option func(*Server)

// WithClock 換掉時鐘。
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithIDGenerator 換掉 intent id 的產生方式，Example 才印得出固定的 id。
func WithIDGenerator(f func() string) Option { return func(s *Server) { s.newID = f } }

// WithPolicy 換掉去重層的 TTL 與 lease。
func WithPolicy(p idempotency.Policy) Option { return func(s *Server) { s.policy = p } }

// New 建立路由。回傳 http.Handler 而不是 *Server，呼叫端只需要把它掛到 http.Server 上。
func New(intents intent.Store, idem idempotency.Store, opts ...Option) http.Handler {
	s := &Server{intents: intents, idem: idem, now: time.Now, newID: NewIntentID, policy: idempotency.DefaultPolicy()}
	for _, o := range opts {
		o(s)
	}
	mux := http.NewServeMux()
	create := idempotency.Handler(idempotency.Options{Store: idem, Policy: s.policy, Now: s.now}, http.HandlerFunc(s.createIntent))
	mux.Handle("POST /v1/payment_intents", create)
	mux.HandleFunc("GET /v1/payment_intents/{id}", s.getIntent)
	return mux
}

// NewIntentID 產生 pi_ 加 24 個 hex 字元（96 bits 亂數）。
//
// intent id 由伺服器產生、跟客戶端的 Idempotency-Key 無關：key 是客戶端取的、只在 scope 內唯一、
// 24 小時後可以回收再用；id 要全系統唯一、永遠不變。兩者的生命週期不同，不能拿一個推另一個。
func NewIntentID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("api: crypto/rand unavailable: " + err.Error())
	}
	return "pi_" + hex.EncodeToString(b[:])
}

// CreateIntentRequest 是 POST /v1/payment_intents 的 body。
//
// Amount 是字串不是數字：JSON 的數字在多數 parser 手上是 IEEE 754 double，2^53 以上就不精確
// （RFC 8259 §6 直接提醒了這件事），而 18 位小數的 token 一筆一百萬就超過了。金額一律走字串。
type CreateIntentRequest struct {
	Chain    string `json:"chain"`
	Token    string `json:"token"`
	Payer    string `json:"payer"`
	Merchant string `json:"merchant"`
	Amount   string `json:"amount"`
	// ExpiresInSeconds 是簽名迴圈的期限，從現在起算。0 代表不設限。
	ExpiresInSeconds int64 `json:"expires_in_seconds,omitempty"`
}

// IntentResponse 是對外的 intent 形狀。欄位比 intent.Intent 少：History、UpdatedAt 這些是內部的事。
type IntentResponse struct {
	ID        string    `json:"id"`
	Chain     string    `json:"chain"`
	Token     string    `json:"token"`
	Payer     string    `json:"payer"`
	Merchant  string    `json:"merchant"`
	Amount    string    `json:"amount"`
	State     string    `json:"state"`
	Version   uint64    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

func toResponse(it *intent.Intent) IntentResponse {
	return IntentResponse{
		ID: it.ID, Chain: it.Chain, Token: it.Token, Payer: it.Payer, Merchant: it.Merchant,
		Amount: it.Amount.String(), State: string(it.State), Version: it.Version,
		CreatedAt: it.CreatedAt, ExpiresAt: it.ExpiresAt,
	}
}

// createIntent 是「同一個請求只會被叫到一次」的那個 handler。它自己不做去重。
func (s *Server) createIntent(w http.ResponseWriter, r *http.Request) {
	var req CreateIntentRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	amount, ok := new(big.Int).SetString(req.Amount, 10)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "amount must be a base-10 integer string")
		return
	}
	now := s.now()
	spec := intent.Spec{
		ID: s.newID(), Chain: req.Chain, Token: req.Token, Payer: req.Payer, Merchant: req.Merchant, Amount: amount,
	}
	if req.ExpiresInSeconds > 0 {
		spec.ExpiresAt = now.Add(time.Duration(req.ExpiresInSeconds) * time.Second)
	}
	it, err := intent.New(spec, now)
	if err != nil {
		if errors.Is(err, intent.ErrInvalidSpec) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// expectedVersion=0：這是新的一筆。id 是亂數，撞到現有的 id 機率可以忽略，但撞到就是 500，不是默默覆蓋。
	if err := s.intents.Save(r.Context(), it, 0); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Location", "/v1/payment_intents/"+it.ID)
	writeJSON(w, http.StatusCreated, toResponse(it))
}

func (s *Server) getIntent(w http.ResponseWriter, r *http.Request) {
	it, err := s.intents.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, intent.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toResponse(it))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, idempotency.ErrorBody{Error: code, Detail: detail})
}

// String 讓 IntentResponse 在 Example 裡印成一行。
func (r IntentResponse) String() string {
	return fmt.Sprintf("%s %s v%d %s %s", r.ID, r.State, r.Version, r.Amount, r.Merchant)
}

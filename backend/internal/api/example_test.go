package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/api"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/idempotency"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
)

// Example_retryStorm 是「同一筆請求送三次」的長相：第一次建立、第二次第三次重放同一份答案；
// 接著同 key 換金額被 422 擋下、換 key 才是新的一筆。id 固定成 pi_0001、pi_0002 是為了印得出來。
func Example_retryStorm() {
	seq := 0
	h := api.New(intent.NewMemoryStore(), idempotency.NewMemoryStore(),
		api.WithClock(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }),
		api.WithIDGenerator(func() string { seq++; return fmt.Sprintf("pi_%04d", seq) }),
	)
	send := func(key, amount string) {
		body := `{"chain":"evm:31337","token":"0x5FbDB2315678afecb367f032d93F642f64180aa3",` +
			`"payer":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8","merchant":"0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",` +
			`"amount":"` + amount + `","expires_in_seconds":900}`
		req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer merchant-demo")
		req.Header.Set(idempotency.HeaderKey, key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var out struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		line := fmt.Sprintf("%-11s amount=%-9s -> %d", key, amount, rr.Code)
		if out.ID != "" {
			line += fmt.Sprintf("  id=%s state=%s", out.ID, out.State)
		} else {
			line += "  error=" + out.Error
		}
		if rr.Header().Get(idempotency.HeaderReplayed) == "true" {
			line += "  Idempotent-Replayed: true"
		}
		fmt.Println(line)
	}

	send("order-1001", "100000000")
	send("order-1001", "100000000")
	send("order-1001", "100000000")
	send("order-1001", "999000000")
	send("order-1002", "999000000")

	// Output:
	// order-1001  amount=100000000 -> 201  id=pi_0001 state=created
	// order-1001  amount=100000000 -> 201  id=pi_0001 state=created  Idempotent-Replayed: true
	// order-1001  amount=100000000 -> 201  id=pi_0001 state=created  Idempotent-Replayed: true
	// order-1001  amount=999000000 -> 422  error=idempotency_key_reused
	// order-1002  amount=999000000 -> 201  id=pi_0002 state=created
}

// Example_traceByRef 是「從鏈上撈到一個 ref，反查回這筆錢的一生」的長相。
// 先建一筆 intent（拿到 ref），再替 relayer 與 listener 把它推到 settled（中間經歷一次 reorg，兩個 tx hash），
// 最後只拿 ref 去問 API：回來的是 intent id、目前狀態，跟 History 裡的每一步、每個 tx hash。
func Example_traceByRef() {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	intents := intent.NewMemoryStore()
	h := api.New(intents, idempotency.NewMemoryStore(),
		api.WithClock(func() time.Time { return now }),
		api.WithIDGenerator(func() string { return "pi_0001" }),
	)

	body := `{"chain":"evm:31337","token":"0x5FbDB2315678afecb367f032d93F642f64180aa3",` +
		`"payer":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8","merchant":"0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",` +
		`"amount":"100000000","expires_in_seconds":900}`
	req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer merchant-demo")
	req.Header.Set(idempotency.HeaderKey, "order-1001")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var created api.IntentResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	fmt.Printf("POST -> %d  id=%s\n            ref=%s\n", rr.Code, created.ID, created.Ref)

	// 今天還沒有 relayer 與 listener，用 Advance 替它們把 intent 推到底：兩個 tx hash，只有一筆錢動了。
	steps := []intent.Request{
		{To: intent.StateAuthorized, By: intent.ActorAPI, At: now.Add(1 * time.Minute)},
		{To: intent.StateSettling, By: intent.ActorRelayer, At: now.Add(2 * time.Minute)},
		{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: "0xaa", At: now.Add(3 * time.Minute)},
		{To: intent.StateSettling, By: intent.ActorListener, Reason: "reorg at block 12", At: now.Add(4 * time.Minute)},
		{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: "0xbb", At: now.Add(5 * time.Minute)},
		{To: intent.StateSettled, By: intent.ActorListener, TxHash: "0xbb", At: now.Add(6 * time.Minute)},
	}
	for _, st := range steps {
		if _, _, err := intent.Advance(ctx, intents, created.ID, st); err != nil {
			panic(err)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/payment_refs/"+created.Ref, nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var trace api.TraceResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &trace)
	fmt.Printf("GET  -> %d  %s\n", rr.Code, trace.Intent)
	for _, t := range trace.History {
		fmt.Println("  " + t.String())
	}

	// Output:
	// POST -> 201  id=pi_0001
	//             ref=0xb02f8d2972380c471030066cf638083d0d6e1674d250a38f2347c28fc5783c47
	// GET  -> 200  pi_0001 settled v7 100000000 0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC
	//   created      -> authorized   by api
	//   authorized   -> settling     by relayer
	//   settling     -> confirming   by relayer   tx 0xaa
	//   confirming   -> settling     by listener  (reorg at block 12)
	//   settling     -> confirming   by relayer   tx 0xbb
	//   confirming   -> settled      by listener  tx 0xbb
}

package api_test

import (
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

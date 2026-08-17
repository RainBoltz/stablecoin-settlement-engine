// Command api 起一個只用記憶體的 Payment API，給本地開發與 curl 用。
//
// 沒有資料庫、沒有鏈：程序一停，intent 與去重紀錄都會消失。它存在的理由是讓「同一個請求送三次」
// 這件事可以用 curl 親眼看到，而不是只在測試裡發生。
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/api"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/idempotency"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
)

func main() {
	addr := flag.String("addr", envOr("API_ADDR", "127.0.0.1:8080"), "listen address")
	flag.Parse()

	h := api.New(intent.NewMemoryStore(), idempotency.NewMemoryStore())
	srv := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(h),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.SetFlags(0)
	log.Printf("[api] listening on http://%s (memory stores; state is lost on exit)", *addr)
	log.Fatal(srv.ListenAndServe())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// logRequests 一行一個請求：方法、路徑、狀態碼、有沒有帶 key、是不是重放。
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		key := r.Header.Get(idempotency.HeaderKey)
		if key == "" {
			key = "-"
		}
		replayed := ""
		if sw.Header().Get(idempotency.HeaderReplayed) == "true" {
			replayed = " replayed"
		}
		log.Printf("[api] %s %s -> %d  key=%s%s", r.Method, r.URL.Path, sw.status, key, replayed)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

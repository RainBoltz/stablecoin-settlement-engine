package idempotency

import (
	"errors"
	"strings"
	"testing"
)

// TestKey_Validate：空、太長、含空白或非 ASCII 都拒絕；255 個可見 ASCII 剛好收。
// 防的是「同一個 key 經過 proxy 或 SDK 之後變成另一個字串」，到時候去重層會把一次請求認成兩次。
func TestKey_Validate(t *testing.T) {
	good := []Key{"a", "order-1001", "8e03978e-40d5-43e8-bc93-6894a57f9324", Key(strings.Repeat("k", MaxKeyLen))}
	for _, k := range good {
		if err := k.Validate(); err != nil {
			t.Errorf("%q should be valid: %v", k, err)
		}
	}
	bad := map[string]Key{
		"empty":     "",
		"too long":  Key(strings.Repeat("k", MaxKeyLen+1)),
		"space":     "order 1001",
		"tab":       "order\t1001",
		"non-ascii": "訂單-1001",
		"newline":   "order\n",
	}
	for name, k := range bad {
		if err := k.Validate(); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: want ErrInvalidKey, got %v", name, err)
		}
	}
}

// TestFingerprint_CoversMethodPathAndRawBody：三樣任何一樣不同就是不同的請求，
// 而且 body 是原始 bytes 比對，JSON 欄位順序不同也算不同（寧可誤殺）。
func TestFingerprint_CoversMethodPathAndRawBody(t *testing.T) {
	base := FingerprintOf("POST", "/v1/payment_intents", []byte(`{"amount":"1"}`))
	same := FingerprintOf("POST", "/v1/payment_intents", []byte(`{"amount":"1"}`))
	if base != same {
		t.Fatal("same inputs must give the same fingerprint")
	}
	diffs := map[string]Fingerprint{
		"method":     FingerprintOf("PATCH", "/v1/payment_intents", []byte(`{"amount":"1"}`)),
		"path":       FingerprintOf("POST", "/v1/refunds", []byte(`{"amount":"1"}`)),
		"body":       FingerprintOf("POST", "/v1/payment_intents", []byte(`{"amount":"2"}`)),
		"whitespace": FingerprintOf("POST", "/v1/payment_intents", []byte(`{"amount": "1"}`)),
	}
	for name, fp := range diffs {
		if fp == base {
			t.Errorf("%s differs but fingerprint collided", name)
		}
	}
	// 分隔符號存在：method="POSTx" path="" 不能等於 method="POST" path="x"
	if FingerprintOf("POSTx", "", nil) == FingerprintOf("POST", "x", nil) {
		t.Fatal("fields must be delimited")
	}
	if len(base.String()) != 16 {
		t.Fatalf("String() should be 8 bytes of hex, got %q", base.String())
	}
}

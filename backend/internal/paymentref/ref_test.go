package paymentref

import (
	"errors"
	"strings"
	"testing"
)

// canonical 是整個系列反覆出現的那筆付款：devnet 的 payer 付 100 USDC 給 merchant。
var canonical = Terms{
	IntentID: "pi_0001",
	Chain:    "evm:31337",
	Token:    "0x5FbDB2315678afecb367f032d93F642f64180aa3",
	Payer:    "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
	Merchant: "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
	Amount:   "100000000",
}

// canonicalRef 是 canonical 算出來的 ref，寫死在這裡當 golden：任何人動到 DomainV1、欄位順序或編碼，
// 這條測試會先叫。改編碼就是改所有已經上鏈的 ref 的意義，必須是刻意的、有 diff 的。
const canonicalRef = "0xb02f8d2972380c471030066cf638083d0d6e1674d250a38f2347c28fc5783c47"

// TestDerive_PinnedVector：canonical 的 ref 逐字等於 golden。
func TestDerive_PinnedVector(t *testing.T) {
	if got := Derive(canonical).String(); got != canonicalRef {
		t.Fatalf("ref changed:\n got %s\nwant %s", got, canonicalRef)
	}
}

// TestDerive_IsDeterministic：同一組條件算幾次都一樣，而且不是零值。
func TestDerive_IsDeterministic(t *testing.T) {
	a, b := Derive(canonical), Derive(canonical)
	if a != b || a.IsZero() {
		t.Fatalf("a=%s b=%s", a, b)
	}
}

// TestDerive_EveryFieldMatters：六個欄位任一個動一下（連只改大小寫都算），ref 整個變。
// 這就是 commitment 的意思：資料庫那一列少一個零、換一個收款人，鏈上那 32 bytes 就對不回來。
func TestDerive_EveryFieldMatters(t *testing.T) {
	base := Derive(canonical)
	mutations := map[string]func(*Terms){
		"intent id": func(t *Terms) { t.IntentID = "pi_0002" },
		"chain":     func(t *Terms) { t.Chain = "evm:1" },
		"token":     func(t *Terms) { t.Token = "0xdAC17F958D2ee523a2206206994597C13D831ec7" },
		"payer":     func(t *Terms) { t.Payer = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC" },
		"merchant":  func(t *Terms) { t.Merchant = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8" },
		"amount":    func(t *Terms) { t.Amount = "100000001" },
		"case only": func(t *Terms) { t.Token = strings.ToLower(t.Token) },
	}
	for name, mutate := range mutations {
		tt := canonical
		mutate(&tt)
		if Derive(tt) == base {
			t.Errorf("%s: ref did not change", name)
		}
	}
}

// TestDerive_FieldBoundariesAreUnambiguous：長度前綴讓 ("ab","c") 與 ("a","bc") 算出不同的 ref。
// 用分隔符拼字串的做法會在這裡撞車，之後就有人能用一組不同的條件湊出同一個 ref。
func TestDerive_FieldBoundariesAreUnambiguous(t *testing.T) {
	x := Derive(Terms{IntentID: "ab", Chain: "c"})
	y := Derive(Terms{IntentID: "a", Chain: "bc"})
	z := Derive(Terms{IntentID: "abc", Chain: ""})
	if x == y || y == z || x == z {
		t.Fatalf("boundary collision: %s %s %s", x, y, z)
	}
}

// TestRef_StringParseRoundTrip：印出去再讀回來是同一個 ref；大寫 hex 也讀得回來，印出來一律小寫。
func TestRef_StringParseRoundTrip(t *testing.T) {
	r := Derive(canonical)
	s := r.String()
	if len(s) != 66 || !strings.HasPrefix(s, "0x") || s != strings.ToLower(s) {
		t.Fatalf("string form: %q", s)
	}
	back, err := Parse(s)
	if err != nil || back != r {
		t.Fatalf("round trip: %v %s", err, back)
	}
	upper, err := Parse("0x" + strings.ToUpper(s[2:]))
	if err != nil || upper != r {
		t.Fatalf("upper-case hex should parse: %v", err)
	}
}

// TestParse_RejectsMalformed：沒有 0x、長度不對、不是 hex，一律 ErrInvalidRef。追蹤鍵只有一種寫法。
func TestParse_RejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"no prefix":  strings.TrimPrefix(canonicalRef, "0x"),
		"too short":  canonicalRef[:64],
		"too long":   canonicalRef + "00",
		"not hex":    "0x" + strings.Repeat("zz", Size),
		"empty":      "",
		"pi_ string": "pi_0001",
	}
	for name, in := range cases {
		if _, err := Parse(in); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("%s: want ErrInvalidRef, got %v", name, err)
		}
	}
}

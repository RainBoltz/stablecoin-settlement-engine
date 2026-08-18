package ledger

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	usdc     = "0x5FbDB2315678afecb367f032d93F642f64180aa3"
	usdt     = "0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512"
	payer    = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	merchant = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
)

var (
	assetUSDC = Asset{Chain: "evm:31337", Token: usdc}
	assetUSDT = Asset{Chain: "evm:31337", Token: usdt}
	// refA 就是整個系列反覆出現的那筆：pi_0001 付 100 USDC，跟 paymentref 的 canonical 同一個 ref。
	refA = paymentref.Derive(paymentref.Terms{IntentID: "pi_0001", Chain: "evm:31337", Token: usdc, Payer: payer, Merchant: merchant, Amount: "100000000"})
	refB = paymentref.Derive(paymentref.Terms{IntentID: "pi_0002", Chain: "evm:31337", Token: usdt, Payer: payer, Merchant: merchant, Amount: "100000000"})
)

// twoLegs 是最常見的形狀：payer 出、merchant 進、同一個數字。
func twoLegs(amount int64) []Leg {
	return []Leg{
		{Account: PayerAccount(payer), Amount: big.NewInt(-amount)},
		{Account: MerchantAccount(merchant), Amount: big.NewInt(amount)},
	}
}

// hold 建一筆 hold；測試裡的 pi_0001 都用 refA 與 USDC。
func hold(id string, ref paymentref.Ref, asset Asset, amount int64) Entry {
	return Entry{ID: id + "/hold", Ref: ref, Kind: KindHold, Asset: asset, Legs: twoLegs(amount), By: "relayer", At: t0.Add(time.Minute)}
}

func post(id string, ref paymentref.Ref, asset Asset, legs []Leg) Entry {
	return Entry{ID: id + "/post", Ref: ref, Kind: KindPost, Holds: id + "/hold", Asset: asset, Legs: legs, By: "listener", At: t0.Add(5 * time.Minute), TxHash: "0xbb"}
}

func void(id string, ref paymentref.Ref, asset Asset) Entry {
	return Entry{ID: id + "/void", Ref: ref, Kind: KindVoid, Holds: id + "/hold", Asset: asset, By: "relayer", At: t0.Add(5 * time.Minute), Memo: "blacklisted"}
}

// TestEntry_LegsMustSumToZero：複式記帳唯一的硬規則。少一個零、多一個零、只有一條腿，都拒收，
// 因為那就是「錢憑空出現／消失」在資料上的長相。
func TestEntry_LegsMustSumToZero(t *testing.T) {
	cases := map[string]struct {
		legs []Leg
		want error
	}{
		"payer pays 100, merchant gets 99": {
			legs: []Leg{{PayerAccount(payer), big.NewInt(-100)}, {MerchantAccount(merchant), big.NewInt(99)}},
			want: ErrUnbalanced,
		},
		"merchant gets 100 out of thin air": {
			legs: []Leg{{MerchantAccount(merchant), big.NewInt(100)}},
			want: ErrInvalidEntry,
		},
		"both legs positive": {
			legs: []Leg{{PayerAccount(payer), big.NewInt(100)}, {MerchantAccount(merchant), big.NewInt(100)}},
			want: ErrUnbalanced,
		},
		"zero leg": {
			legs: []Leg{{PayerAccount(payer), big.NewInt(0)}, {MerchantAccount(merchant), big.NewInt(0)}},
			want: ErrInvalidEntry,
		},
		"same account twice": {
			legs: []Leg{{PayerAccount(payer), big.NewInt(-100)}, {PayerAccount(payer), big.NewInt(100)}},
			want: ErrInvalidEntry,
		},
	}
	for name, c := range cases {
		e := hold("pi_0001", refA, assetUSDC, 100)
		e.Legs = c.legs
		if err := e.Validate(); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", name, c.want, err)
		}
	}
	// 三條腿也可以，只要加總是零：這就是轉帳稅在帳上的長相。
	e := hold("pi_0002", refB, assetUSDT, 100)
	e.Legs = []Leg{
		{PayerAccount(payer), big.NewInt(-100_000_000)},
		{MerchantAccount(merchant), big.NewInt(99_900_000)},
		{FeeAccount(usdt), big.NewInt(100_000)},
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("three balanced legs should be fine: %v", err)
	}
}

// TestEntry_ShapePerKind：hold 不能指別的 hold、post 與 void 一定要指、void 沒有腿；欄位缺一不可。
func TestEntry_ShapePerKind(t *testing.T) {
	ok := hold("pi_0001", refA, assetUSDC, 100)
	if err := ok.Validate(); err != nil {
		t.Fatalf("baseline hold should validate: %v", err)
	}
	mutations := map[string]func(*Entry){
		"hold with holds":   func(e *Entry) { e.Holds = "x/hold" },
		"post without hold": func(e *Entry) { e.Kind = KindPost },
		"void with legs":    func(e *Entry) { e.Kind = KindVoid; e.Holds = "x/hold" },
		"unknown kind":      func(e *Entry) { e.Kind = "adjust" },
		"no id":             func(e *Entry) { e.ID = "" },
		"no ref":            func(e *Entry) { e.Ref = paymentref.Ref{} },
		"no asset":          func(e *Entry) { e.Asset = Asset{} },
		"no by":             func(e *Entry) { e.By = "" },
		"no at":             func(e *Entry) { e.At = time.Time{} },
	}
	for name, mutate := range mutations {
		e := hold("pi_0001", refA, assetUSDC, 100)
		mutate(&e)
		if err := e.Validate(); !errors.Is(err, ErrInvalidEntry) {
			t.Errorf("%s: want ErrInvalidEntry, got %v", name, err)
		}
	}
	v := void("pi_0001", refA, assetUSDC)
	if err := v.Validate(); err != nil {
		t.Fatalf("void without legs should validate: %v", err)
	}
}

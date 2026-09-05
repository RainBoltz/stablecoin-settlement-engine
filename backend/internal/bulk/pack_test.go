package bulk_test

import (
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// payouts 造一份 n 筆的名單；every 大於 0 時每逢第 every 筆標記「要先開帳戶」（從 1 數）。
func payouts(n, every int) []bulk.Payout {
	items := make([]bulk.Payout, 0, n)
	for i := 1; i <= n; i++ {
		merchant := fmt.Sprintf("mch-%05d", i)
		items = append(items, bulk.Payout{
			Ref: paymentref.Derive(paymentref.Terms{
				IntentID: fmt.Sprintf("pi_%05d", i),
				Chain:    "payout-run/test",
				Token:    "USDC",
				Payer:    "platform",
				Merchant: merchant,
				Amount:   "100000000",
			}),
			Merchant:        merchant,
			Amount:          big.NewInt(100_000_000),
			NewTokenAccount: every > 0 && i%every == 0,
		})
	}
	return items
}

// 防的情境：300 筆在 EVM 上本來就裝得進一筆交易，切批的存在不該把它拆開。
func TestPack_EvmFitsAWholePayoutRunInOneTransaction(t *testing.T) {
	plan, err := bulk.Pack(payouts(300, 0), bulk.Defaults()["evm"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(plan.Batches) != 1 {
		t.Fatalf("evm batches = %d, want 1", len(plan.Batches))
	}
	if len(plan.Prepare) != 0 || plan.Levels != 0 {
		t.Fatalf("evm plan grew a prepare phase or a tree: %+v", plan)
	}
	if got := plan.Batches[0].Used[0].Used; got != 16_003_310 {
		t.Fatalf("gas for 300 items = %d, want 16,003,310", got)
	}
}

// 防的情境：同一份名單搬到 Solana。批要切在 8 的倍數邊界上，
// 不是「塞得下就多塞一項」：邊界歪一格，那一批就共用不了一份區塊證明。
func TestPack_SolanaSplitsTheRunIntoAlignedBlocks(t *testing.T) {
	plan, err := bulk.Pack(payouts(300, 0), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(plan.Batches) != 38 {
		t.Fatalf("solana batches = %d, want 38", len(plan.Batches))
	}
	for i, b := range plan.Batches {
		want := 8
		if i == len(plan.Batches)-1 {
			want = 4
		}
		if len(b.Items) != want {
			t.Fatalf("batch %d has %d items, want %d", b.Index, len(b.Items), want)
		}
	}
	if plan.Levels != 9 || plan.ProofHashes != 6 {
		t.Fatalf("tree = depth %d proof %d, want depth 9 proof 6", plan.Levels, plan.ProofHashes)
	}
}

// 防的情境：切批把名單弄丟一筆、或把順序弄亂。名單的順序就是葉子的順序，
// payer 簽的 root 蓋住這個順序，重排等於換了一棵樹。
func TestPack_KeepsTheRunInOrderAndLosesNothing(t *testing.T) {
	items := payouts(300, 25)
	for _, chain := range []string{"evm", "solana"} {
		plan, err := bulk.Pack(items, bulk.Defaults()[chain])
		if err != nil {
			t.Fatalf("Pack(%s): %v", chain, err)
		}
		var flat []bulk.Payout
		for _, b := range plan.Batches {
			flat = append(flat, b.Items...)
		}
		if len(flat) != len(items) {
			t.Fatalf("%s: %d items after packing, want %d", chain, len(flat), len(items))
		}
		for i := range items {
			if flat[i].Ref != items[i].Ref {
				t.Fatalf("%s: item %d is out of order", chain, i)
			}
		}
	}
}

// 防的情境：要開帳戶的那幾項回去讓某一批變貴。開帳戶整個住在 prepare batch 裡，
// 付款批的價錢只跟它裝了幾項有關，跟誰要開帳戶無關。
func TestPack_ANewTokenAccountMovesTheWorkToPrepare(t *testing.T) {
	l := bulk.Defaults()["solana"]
	plain, err := bulk.Pack(payouts(300, 0), l)
	if err != nil {
		t.Fatalf("Pack plain: %v", err)
	}
	flagged, err := bulk.Pack(payouts(300, 25), l)
	if err != nil {
		t.Fatalf("Pack flagged: %v", err)
	}
	if len(flagged.Batches) != len(plain.Batches) {
		t.Fatalf("flagging accounts changed the batch count: %d vs %d", len(flagged.Batches), len(plain.Batches))
	}
	for i := range plain.Batches {
		for j := range plain.Batches[i].Used {
			if plain.Batches[i].Used[j] != flagged.Batches[i].Used[j] {
				t.Fatalf("batch %d got more expensive because of a new account", i+1)
			}
		}
	}
	if len(flagged.Prepare) != 1 {
		t.Fatalf("prepare batches = %d, want 1", len(flagged.Prepare))
	}
	prep := flagged.Prepare[0]
	if !prep.Prep || len(prep.Items) != 12 || prep.NewAccounts != 12 {
		t.Fatalf("prepare batch = %+v, want 12 accounts", prep)
	}
	for _, it := range prep.Items {
		if !it.NewTokenAccount {
			t.Fatal("a merchant that already has an account got into the prepare batch")
		}
	}
}

// 防的情境：rent 要在送出去之前備好。名單上有幾個 merchant 要開帳戶，
// 錢包就要先有幾份 rent，一份都不能少。
func TestPack_ReportsTheRentTheSenderHasToFundFirst(t *testing.T) {
	plan, err := bulk.Pack(payouts(300, 25), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if plan.NewAccounts != 12 {
		t.Fatalf("new accounts = %d, want 12", plan.NewAccounts)
	}
	if plan.Rent != 12*2_039_280 {
		t.Fatalf("rent = %d, want %d", plan.Rent, 12*2_039_280)
	}
}

// 防的情境：任何一批（prepare batch 也算）超過任何一條上限。
// 上限之間沒有互相折抵這回事，一條超了整批就送不出去。
func TestPack_EveryBatchStaysUnderEveryCap(t *testing.T) {
	for _, chain := range []string{"evm", "solana"} {
		plan, err := bulk.Pack(payouts(300, 25), bulk.Defaults()[chain])
		if err != nil {
			t.Fatalf("Pack(%s): %v", chain, err)
		}
		for _, b := range append(plan.Prepare, plan.Batches...) {
			for _, u := range b.Used {
				if u.Used > u.Cap {
					t.Fatalf("%s batch %d uses %d/%d %s", chain, b.Index, u.Used, u.Cap, u.Unit)
				}
			}
		}
	}
}

// 防的情境：兩條上限誰先咬人要弄清楚。Solana 一批 8 項是 bytes 決定的，
// 64 個帳戶的額度連三成都用不到；哪天有人想去調緊 accounts，這條測試會先講話。
func TestPack_BytesIsTheCapThatBindsOnSolana(t *testing.T) {
	plan, err := bulk.Pack(payouts(300, 0), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	full := plan.Batches[0]
	var bytesU, acctU bulk.Usage
	for _, u := range full.Used {
		switch u.Unit {
		case "bytes":
			bytesU = u
		case "accounts":
			acctU = u
		}
	}
	if bytesU.Used != 1_056 {
		t.Fatalf("bytes for a full batch = %d, want 1,056", bytesU.Used)
	}
	if acctU.Used != 13 {
		t.Fatalf("accounts for a full batch = %d, want 13", acctU.Used)
	}
	if float64(bytesU.Used)/float64(bytesU.Cap) <= float64(acctU.Used)/float64(acctU.Cap) {
		t.Fatal("bytes is no longer the cap that binds")
	}
}

// 防的情境：證明的長度跟著整份名單長，不跟著一批長。名單越大樹越高，
// 每一批就要多帶幾個雜湊；大到全滿的一批塞不下證明的那一天，這個切法就到頂了。
func TestPack_TheProofGrowsWithTheRunNotTheBatch(t *testing.T) {
	l := bulk.Defaults()["solana"]

	small, err := bulk.Pack(payouts(300, 0), l)
	if err != nil {
		t.Fatalf("Pack(300): %v", err)
	}
	big16k, err := bulk.Pack(payouts(16_384, 0), l)
	if err != nil {
		t.Fatalf("Pack(16,384): %v", err)
	}
	if small.ProofHashes != 6 || big16k.ProofHashes != 11 {
		t.Fatalf("proof hashes = %d and %d, want 6 and 11", small.ProofHashes, big16k.ProofHashes)
	}
	if got := big16k.Batches[0].Used[0].Used; got != 1_216 {
		t.Fatalf("bytes for a full batch of a 16,384 run = %d, want 1,216", got)
	}

	// 16,385 筆會把樹墊到 32,768 片、證明 12 層，滿批 1,248 bytes 超過 1,232：
	// 一輪撥款在這個切法下有自己的天花板，超過就要拆成兩輪（或把 Align 降到 4）。
	if _, err := bulk.Pack(payouts(16_385, 0), l); !errors.Is(err, bulk.ErrBlockTooLarge) {
		t.Fatalf("err = %v, want ErrBlockTooLarge", err)
	}
}

// 防的情境：名單是空的。
func TestPack_RejectsAnEmptyRun(t *testing.T) {
	if _, err := bulk.Pack(nil, bulk.Defaults()["evm"]); !errors.Is(err, bulk.ErrEmptyRun) {
		t.Fatalf("err = %v, want ErrEmptyRun", err)
	}
}

// 防的情境：拿 Defaults() 查一條沒有實作的鏈，拿到零值就往下走。
func TestPack_RejectsAChainWithNoLimits(t *testing.T) {
	if _, err := bulk.Pack(payouts(1, 0), bulk.Defaults()["aptos"]); !errors.Is(err, bulk.ErrNoRules) {
		t.Fatalf("err = %v, want ErrNoRules", err)
	}
}

// 防的情境：設定寫錯，一項自己就塞不進一筆交易。再切細也救不了，要直接回報。
func TestPack_RejectsAnItemThatCannotFitAlone(t *testing.T) {
	tight := bulk.Limits{
		Chain: "tight",
		Rules: []bulk.Rule{{Unit: "bytes", Cap: 100, Base: 50, Item: 60}},
	}
	if _, err := bulk.Pack(payouts(1, 0), tight); !errors.Is(err, bulk.ErrItemTooLarge) {
		t.Fatalf("err = %v, want ErrItemTooLarge", err)
	}
}

// 防的情境：切批只是讀名單，不該動到呼叫端的 slice。
func TestPack_DoesNotTouchTheCallersSlice(t *testing.T) {
	items := payouts(20, 5)
	before := make([]bulk.Payout, len(items))
	copy(before, items)
	if _, err := bulk.Pack(items, bulk.Defaults()["solana"]); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	for i := range items {
		if items[i].Ref != before[i].Ref || items[i].NewTokenAccount != before[i].NewTokenAccount {
			t.Fatalf("Pack modified the caller's slice at %d", i)
		}
	}
}

// 防的情境：對齊批要是還跟名單共用底層陣列，之後動一批的 Items 會悄悄改到另一批，
// 或改到呼叫端手上那份名單，而兩邊都看不出來哪裡出了錯。批次本來就該各自獨立。
func TestPack_BatchesOwnTheirItems(t *testing.T) {
	items := payouts(20, 0)
	plan, err := bulk.Pack(items, bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(plan.Batches) < 2 {
		t.Fatalf("got %d batches, want at least 2", len(plan.Batches))
	}
	before := items[8].Merchant
	plan.Batches[0].Items[0].Merchant = "mutated"
	if items[0].Merchant == "mutated" {
		t.Fatalf("mutating a batch's Items changed the caller's slice")
	}
	plan.Batches[1].Items[0].Merchant = "mutated"
	if items[8].Merchant != before {
		t.Fatalf("mutating a batch's Items changed the caller's slice")
	}
}

// 防的情境：一筆也是一輪。單獨一筆在 Solana 上還是一棵（墊滿的）樹加一批，
// 只是區塊自己就是整棵樹，證明剛好零個雜湊。
func TestPack_ASingleItemIsStillOneBatch(t *testing.T) {
	plan, err := bulk.Pack(payouts(1, 0), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(plan.Batches) != 1 || len(plan.Batches[0].Items) != 1 {
		t.Fatalf("plan = %+v, want one batch of one item", plan)
	}
	if plan.Levels != 3 || plan.ProofHashes != 0 {
		t.Fatalf("tree = depth %d proof %d, want depth 3 proof 0", plan.Levels, plan.ProofHashes)
	}
}

// 防的情境：prepare batch 有自己的一組規則，跟付款批各算各的。
// 30 個帳戶照 bytes 的算法一筆交易裝 13 個，要切成三批，最後一批 4 個。
func TestPack_PrepareBatchesRespectTheirOwnRules(t *testing.T) {
	plan, err := bulk.Pack(payouts(300, 10), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if plan.NewAccounts != 30 {
		t.Fatalf("new accounts = %d, want 30", plan.NewAccounts)
	}
	if len(plan.Prepare) != 3 {
		t.Fatalf("prepare batches = %d, want 3", len(plan.Prepare))
	}
	sizes := []int{13, 13, 4}
	for i, b := range plan.Prepare {
		if len(b.Items) != sizes[i] {
			t.Fatalf("prepare batch %d has %d accounts, want %d", b.Index, len(b.Items), sizes[i])
		}
	}
}

package bulk_test

import (
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
)

// items 造一份 n 筆的名單，第 every 筆標成「這個 merchant 還沒有 token 帳戶」。every 是 0 就全部都有帳戶。
func items(n, every int) []bulk.Payout {
	out := make([]bulk.Payout, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, bulk.Payout{
			Merchant:        fmt.Sprintf("mch-%03d", i),
			Amount:          big.NewInt(100_000_000),
			NewTokenAccount: every > 0 && i%every == 0,
		})
	}
	return out
}

// 一份三百筆的撥款名單在 EVM 上還離 target block size 很遠，所以組批這件事在 EVM 上
// 幾乎不會成為限制。這條測試釘的是那個「幾乎」：真的塞得下才敢在文章裡這樣講。
func TestPack_EvmFitsAWholePayoutRunInOneTransaction(t *testing.T) {
	plan, err := bulk.Pack(items(300, 0), bulk.Defaults()["evm"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(plan.Batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(plan.Batches))
	}
	if got := len(plan.Batches[0].Items); got != 300 {
		t.Fatalf("items in the batch = %d, want 300", got)
	}
}

// 同一份名單、同一份合約設計，換到 Solana 上要送二十五筆交易。這條測試是整個 package 的理由：
// 「一批多大」不是我們決定的，是鏈決定的，而兩條鏈的答案差了一個數量級。
func TestPack_SolanaSplitsTheSameRunIntoManyTransactions(t *testing.T) {
	plan, err := bulk.Pack(items(300, 25), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(plan.Batches) != 25 {
		t.Fatalf("batches = %d, want 25", len(plan.Batches))
	}
	for _, b := range plan.Batches {
		if len(b.Items) != 12 {
			t.Fatalf("batch #%d has %d items, want 12", b.Index, len(b.Items))
		}
	}
}

// 撥款名單的第幾行要對得回第幾批的第幾項，不然出事時沒有人有辦法把 event 對回原本那份檔案。
// 這條測試釘的是「不重排、不去重、不丟」。
func TestPack_KeepsTheRunInOrderAndLosesNothing(t *testing.T) {
	run := items(300, 25)
	plan, err := bulk.Pack(run, bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	var flat []bulk.Payout
	for _, b := range plan.Batches {
		flat = append(flat, b.Items...)
	}
	if len(flat) != len(run) {
		t.Fatalf("packed %d payouts, want %d", len(flat), len(run))
	}
	for i := range run {
		if flat[i].Merchant != run[i].Merchant {
			t.Fatalf("payout %d is %q, want %q", i, flat[i].Merchant, run[i].Merchant)
		}
	}
}

// 要先開帳戶的那一項比別項貴，所以它會把同一批的其他項擠出去。不把這件事算進去的話，
// 組出來的批次會在鏈上因為交易太長而整批送不出去，而且是送到一半才發現。
func TestPack_ANewTokenAccountMakesThatItemMoreExpensive(t *testing.T) {
	plain, err := bulk.Pack(items(12, 0), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(plain.Batches) != 1 {
		t.Fatalf("twelve plain payouts became %d batches, want 1", len(plain.Batches))
	}
	// 同樣十二筆，其中兩筆要先開帳戶，就裝不進同一筆交易了。
	mixed, err := bulk.Pack(items(12, 6), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(mixed.Batches) != 2 {
		t.Fatalf("twelve payouts with two new accounts became %d batches, want 2", len(mixed.Batches))
	}
}

// rent 不是手續費，是鎖在新帳戶裡的錢。送這批之前發送錢包沒有備夠，
// 會在鏈上開帳戶那一步失敗，而失敗的是整筆交易。所以這個數字要在組批的時候就算出來。
func TestPack_ReportsTheRentTheSenderHasToFundFirst(t *testing.T) {
	plan, err := bulk.Pack(items(300, 25), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if plan.NewAccounts != 12 {
		t.Fatalf("new accounts = %d, want 12", plan.NewAccounts)
	}
	if plan.Rent != 12*2_039_280 {
		t.Fatalf("rent = %d, want %d", plan.Rent, 12*2_039_280)
	}
	if plan.RentUnit != "lamports" {
		t.Fatalf("rent unit = %q, want lamports", plan.RentUnit)
	}
}

// 這條測試對兩條鏈跑同一件事：不管切成幾批，每一批在每一種資源上都要留在上限以內。
// 少檢查一條規則的下場是那一批到了鏈上才被拒絕，而 relayer 只會看到一個很難讀的錯誤。
func TestPack_EveryBatchStaysUnderEveryCap(t *testing.T) {
	for _, chain := range []string{"evm", "solana"} {
		limits := bulk.Defaults()[chain]
		plan, err := bulk.Pack(items(300, 25), limits)
		if err != nil {
			t.Fatalf("%s: Pack: %v", chain, err)
		}
		for _, b := range plan.Batches {
			if len(b.Used) != len(limits.Rules) {
				t.Fatalf("%s batch #%d reports %d units, want %d", chain, b.Index, len(b.Used), len(limits.Rules))
			}
			for _, u := range b.Used {
				if u.Used > u.Cap {
					t.Fatalf("%s batch #%d uses %d %s, cap is %d", chain, b.Index, u.Used, u.Unit, u.Cap)
				}
			}
		}
	}
}

// Solana 有兩個各自獨立的上限，而先撞上的是交易長度，不是那個比較有名的 64 個帳戶。
// 這條測試釘住這個結論：哪一條先撞上會決定我們該去省什麼，省錯地方是白做工。
func TestPack_BytesIsTheCapThatBindsOnSolana(t *testing.T) {
	plan, err := bulk.Pack(items(300, 0), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	b := plan.Batches[0]
	var bytesUsage, accountsUsage bulk.Usage
	for _, u := range b.Used {
		switch u.Unit {
		case "bytes":
			bytesUsage = u
		case "accounts":
			accountsUsage = u
		}
	}
	if bytesUsage.Cap-bytesUsage.Used >= 73 {
		t.Fatalf("bytes still has room for another payout: %d/%d", bytesUsage.Used, bytesUsage.Cap)
	}
	if accountsUsage.Used*2 > accountsUsage.Cap {
		t.Fatalf("accounts is tighter than expected: %d/%d", accountsUsage.Used, accountsUsage.Cap)
	}
}

// 空批次在合約那一側會被擋下來，鏈下沒有理由先組一份出來再送過去被拒絕。
func TestPack_RejectsAnEmptyRun(t *testing.T) {
	if _, err := bulk.Pack(nil, bulk.Defaults()["evm"]); !errors.Is(err, bulk.ErrEmptyRun) {
		t.Fatalf("err = %v, want ErrEmptyRun", err)
	}
}

// Defaults() 查一條還沒實作的鏈會拿到零值。不擋下來的話整份名單會被裝進同一批，
// 而且看起來一切正常，直到那筆交易被鏈拒絕。
func TestPack_RejectsAChainWithNoLimits(t *testing.T) {
	_, err := bulk.Pack(items(3, 0), bulk.Defaults()["ton"])
	if !errors.Is(err, bulk.ErrNoRules) {
		t.Fatalf("err = %v, want ErrNoRules", err)
	}
}

// 一項自己一個人都塞不下的話，切得再細也沒有用，所以這裡不回一份切不完的計畫，直接報錯。
func TestPack_RejectsAnItemThatCannotFitAlone(t *testing.T) {
	tight := bulk.Limits{
		Chain: "tight",
		Rules: []bulk.Rule{{Unit: "bytes", Cap: 320, Base: 311, Item: 73, Source: "test"}},
	}
	_, err := bulk.Pack(items(1, 0), tight)
	if !errors.Is(err, bulk.ErrItemTooLarge) {
		t.Fatalf("err = %v, want ErrItemTooLarge", err)
	}
}

// 呼叫端手上那份名單多半還要拿去記帳與比對，Pack 不能動它。
func TestPack_DoesNotTouchTheCallersSlice(t *testing.T) {
	run := items(30, 5)
	before := make([]bulk.Payout, len(run))
	copy(before, run)
	if _, err := bulk.Pack(run, bulk.Defaults()["solana"]); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	for i := range run {
		if run[i] != before[i] {
			t.Fatalf("payout %d changed: %+v -> %+v", i, before[i], run[i])
		}
	}
}

// 一筆付款的名單照樣是一份計畫，跟合約那一側「一項的批次跟單筆結清行為一樣」是同一個原則。
func TestPack_ASingleItemIsStillOneBatch(t *testing.T) {
	plan, err := bulk.Pack(items(1, 0), bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(plan.Batches) != 1 || len(plan.Batches[0].Items) != 1 {
		t.Fatalf("plan = %v, want one batch of one", plan)
	}
	if plan.Batches[0].Index != 1 {
		t.Fatalf("batch index = %d, want 1", plan.Batches[0].Index)
	}
}

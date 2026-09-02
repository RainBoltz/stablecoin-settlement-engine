package chain_test

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

const (
	evmToken = "0x1000000000000000000000000000000000000001"
	evmPayer = "0x2000000000000000000000000000000000000002"
)

// evmPayout 造一筆 EVM 名單上的付款：ref 是從真的 Terms 推導的，不是亂數，
// 所以 golden calldata 每次都長一樣。
func evmPayout(i int, merchant string, amount int64) bulk.Payout {
	return bulk.Payout{
		Ref: paymentref.Derive(paymentref.Terms{
			IntentID: fmt.Sprintf("pi_24%02d", i),
			Chain:    "evm:31337",
			Token:    evmToken,
			Payer:    evmPayer,
			Merchant: merchant,
			Amount:   strconv.FormatInt(amount, 10),
		}),
		Merchant: merchant,
		Amount:   big.NewInt(amount),
	}
}

// goldenSettleBatch 是同一批兩筆付款用 Foundry 編出來的 calldata：
//
//	cast calldata "settleBatch(address,address,(address,uint256,bytes32)[])" \
//	  0x1000000000000000000000000000000000000001 0x2000000000000000000000000000000000000002 \
//	  "[(0x3000000000000000000000000000000000000003,100000000,0x08ae…2723),(0x4000000000000000000000000000000000000004,250000000,0xac12…6f66)]"
//
// 兩個 ref 是上面 evmPayout 對 pi_2401 與 pi_2402 算出來的值。跑出來不一樣是編碼器錯了，
// 去修編碼器，不要改這一串。
const goldenSettleBatch = "0xd0e1d648" +
	"0000000000000000000000001000000000000000000000000000000000000001" +
	"0000000000000000000000002000000000000000000000000000000000000002" +
	"0000000000000000000000000000000000000000000000000000000000000060" +
	"0000000000000000000000000000000000000000000000000000000000000002" +
	"0000000000000000000000003000000000000000000000000000000000000003" +
	"0000000000000000000000000000000000000000000000000000000005f5e100" +
	"08ae0bc180a552184ae2543d61802f536005d4ae402ac62f496190852c822723" +
	"0000000000000000000000004000000000000000000000000000000000000004" +
	"000000000000000000000000000000000000000000000000000000000ee6b280" +
	"ac128e684fd68e4f896b0f974dfdeca90abfeeda800a224ee5435faed2c06f66"

// TestSettleBatchCalldata_MatchesCastExactly 拿 Foundry 的 cast 當對照組：
// 同一批付款，我們的編碼器要跟它逐 byte 相同。ABI 只有一種正確答案。
func TestSettleBatchCalldata_MatchesCastExactly(t *testing.T) {
	items := []bulk.Payout{
		evmPayout(1, "0x3000000000000000000000000000000000000003", 100_000_000),
		evmPayout(2, "0x4000000000000000000000000000000000000004", 250_000_000),
	}
	out, err := chain.NewEVM().SettleBatchCalldata(evmToken, evmPayer, items)
	if err != nil {
		t.Fatalf("SettleBatchCalldata: %v", err)
	}
	if got := "0x" + hex.EncodeToString(out); got != goldenSettleBatch {
		t.Fatalf("calldata drifted from cast:\n got %s\nwant %s", got, goldenSettleBatch)
	}
}

// TestSettleBatchCalldata_StartsWithThePinnedSelector 釘住選擇子是寫死的常數：
// keccak256 不在標準函式庫裡，算得出它的那一天就是這個 repo 吃下第一個外部依賴的那一天。
func TestSettleBatchCalldata_StartsWithThePinnedSelector(t *testing.T) {
	out, err := chain.NewEVM().SettleBatchCalldata(evmToken, evmPayer,
		[]bulk.Payout{evmPayout(1, "0x3000000000000000000000000000000000000003", 1)})
	if err != nil {
		t.Fatalf("SettleBatchCalldata: %v", err)
	}
	if got := hex.EncodeToString(out[:4]); got != "d0e1d648" {
		t.Fatalf("selector = 0x%s, want 0xd0e1d648 (cast sig)", got)
	}
	if want := 4 + 96 + 32 + 96; len(out) != want {
		t.Fatalf("one payout encodes to %d bytes, want %d", len(out), want)
	}
}

// TestSettleBatchCalldata_RejectsABadAddress 釘住地址只有一種寫法：名單上那種鏈中立的
// merchant 名（mch-003）到了要編碼的這一刻就不夠用了，錯誤要說出是第幾筆。
func TestSettleBatchCalldata_RejectsABadAddress(t *testing.T) {
	items := []bulk.Payout{
		evmPayout(1, "0x3000000000000000000000000000000000000003", 1),
		evmPayout(2, "mch-003", 1),
	}
	_, err := chain.NewEVM().SettleBatchCalldata(evmToken, evmPayer, items)
	if !errors.Is(err, chain.ErrBadAddress) {
		t.Fatalf("err = %v, want ErrBadAddress", err)
	}
	if !strings.Contains(err.Error(), "payout 1") {
		t.Fatalf("the error should name the payout: %v", err)
	}
	if _, err := chain.NewEVM().SettleBatchCalldata("USDC", evmPayer, items[:1]); !errors.Is(err, chain.ErrBadAddress) {
		t.Fatalf("token err = %v, want ErrBadAddress", err)
	}
}

// TestSettleBatchCalldata_RejectsWhatTheContractWouldReject 把合約 _reserve 的三條 require
// 搬到組交易之前：空批、零 ref、非正金額，組出來也只是一筆必定 revert 的交易。
func TestSettleBatchCalldata_RejectsWhatTheContractWouldReject(t *testing.T) {
	e := chain.NewEVM()
	if _, err := e.SettleBatchCalldata(evmToken, evmPayer, nil); !errors.Is(err, chain.ErrEmptyBatch) {
		t.Fatalf("empty batch err = %v, want ErrEmptyBatch", err)
	}
	zero := evmPayout(1, "0x3000000000000000000000000000000000000003", 1)
	zero.Ref = paymentref.Ref{}
	if _, err := e.SettleBatchCalldata(evmToken, evmPayer, []bulk.Payout{zero}); !errors.Is(err, chain.ErrZeroRef) {
		t.Fatalf("zero ref err = %v, want ErrZeroRef", err)
	}
	for _, amt := range []*big.Int{nil, big.NewInt(0), big.NewInt(-5)} {
		bad := evmPayout(1, "0x3000000000000000000000000000000000000003", 1)
		bad.Amount = amt
		if _, err := e.SettleBatchCalldata(evmToken, evmPayer, []bulk.Payout{bad}); !errors.Is(err, chain.ErrBadAmount) {
			t.Fatalf("amount %v err = %v, want ErrBadAmount", amt, err)
		}
	}
}

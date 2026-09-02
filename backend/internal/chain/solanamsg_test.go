package chain_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
)

// solanaSignedBytes 是一份訊息簽好之後的交易大小：簽名數的 compact-u16（一個 byte）
// 加一個 64 bytes 的簽名，再加訊息本身。bulk 的 bytes 規則算的是這個總數。
func solanaSignedBytes(msg []byte) uint64 {
	return uint64(1 + 64 + len(msg))
}

// key 造一個定值的假帳戶位址：sha256 過的標籤。真的位址要從鏈上查，形狀是一樣的 32 bytes。
func key(label string) chain.Pubkey {
	return chain.Pubkey(sha256.Sum256([]byte(label)))
}

// solanaFixture 是六個固定帳戶加一個 blockhash，全部由標籤決定，跑幾次都一樣。
func solanaFixture() (chain.SolanaAccounts, [32]byte) {
	acc := chain.SolanaAccounts{
		FeePayer:          key("fee-payer"),
		PayerTokenAccount: key("payer-usdc-account"),
		Authority:         key("authority-pda"),
		Program:           key("settlement-program"),
		TokenProgram:      key("spl-token-program"),
		Mint:              key("usdc-mint"),
	}
	return acc, sha256.Sum256([]byte("recent-blockhash"))
}

// solanaBatch 造 n 筆付款與它們各自的 token 帳戶，merchant 全部不同。
func solanaBatch(n int) ([]bulk.Payout, []chain.Pubkey) {
	items := make([]bulk.Payout, 0, n)
	accounts := make([]chain.Pubkey, 0, n)
	for i := 1; i <= n; i++ {
		merchant := fmt.Sprintf("0x%036x%04x", 9, i)
		items = append(items, evmPayout(i, merchant, 100_000_000))
		accounts = append(accounts, key("token-account:"+merchant))
	}
	return items, accounts
}

// bytesRule 把 bulk 對 solana 的 bytes 規則挑出來，測試拿它當對照組。
func bytesRule(t *testing.T) bulk.Rule {
	t.Helper()
	for _, r := range bulk.Defaults()["solana"].Rules {
		if r.Unit == "bytes" {
			return r
		}
	}
	t.Fatalf("bulk no longer has a bytes rule for solana")
	return bulk.Rule{}
}

// TestBatchMessage_MatchesTheBytesRuleInBulk 是這半個 adapter 存在的對照組：bulk 的
// Base 311 與 Item 73 本來是照 wire format 手算的估計，這裡拿真的序列化結果對答案。
// 三項以上要逐 byte 相等；一、兩項時指令資料還不到 128 bytes，compact-u16 的長度前綴只占
// 一個 byte，真值比估計少一。估計只准高、不准低，這一個 byte 就是全部的誤差。
func TestBatchMessage_MatchesTheBytesRuleInBulk(t *testing.T) {
	rule := bytesRule(t)
	acc, blockhash := solanaFixture()
	for n := 1; n <= 12; n++ {
		items, accounts := solanaBatch(n)
		msg, err := chain.NewSolana().BatchMessage(acc, blockhash, items, accounts)
		if err != nil {
			t.Fatalf("BatchMessage(%d): %v", n, err)
		}
		got := solanaSignedBytes(msg)
		want := rule.Base + uint64(n)*rule.Item
		if got > want {
			t.Fatalf("%d payouts serialize to %d bytes, above the %d bulk promised", n, got, want)
		}
		if n >= 3 && got != want {
			t.Fatalf("%d payouts serialize to %d bytes, want exactly %d", n, got, want)
		}
		if n < 3 && got != want-1 {
			t.Fatalf("%d payouts serialize to %d bytes, want %d (one byte under the estimate)", n, got, want-1)
		}
	}
}

// TestBatchMessage_ThirteenPayoutsOverflowTheWire 釘住 12 是物理上限：
// 第 13 筆會把交易推過 1,232 bytes，這正是 Pack 在第 12 筆收批的原因。
func TestBatchMessage_ThirteenPayoutsOverflowTheWire(t *testing.T) {
	rule := bytesRule(t)
	acc, blockhash := solanaFixture()
	items, accounts := solanaBatch(13)
	msg, err := chain.NewSolana().BatchMessage(acc, blockhash, items, accounts)
	if err != nil {
		t.Fatalf("BatchMessage: %v", err)
	}
	if got := solanaSignedBytes(msg); got <= rule.Cap {
		t.Fatalf("13 payouts serialize to %d bytes, expected more than the %d cap", got, rule.Cap)
	}
}

// TestBatchMessage_UsesCompactLengths 用三筆的訊息釘住兩個 compact-u16 的落點：
// 帳戶清單長度九（一個 byte），指令資料長度 129（兩個 byte：0x81 0x01）。
func TestBatchMessage_UsesCompactLengths(t *testing.T) {
	acc, blockhash := solanaFixture()
	items, accounts := solanaBatch(3)
	msg, err := chain.NewSolana().BatchMessage(acc, blockhash, items, accounts)
	if err != nil {
		t.Fatalf("BatchMessage: %v", err)
	}
	if msg[3] != 9 {
		t.Fatalf("account list length byte = %d, want 9", msg[3])
	}
	dlen := 3 + 1 + 9*32 + 32 + 1 + 1 + 1 + 7
	if msg[dlen] != 0x81 || msg[dlen+1] != 0x01 {
		t.Fatalf("data length bytes = %x %x, want 81 01 (129 in compact-u16)", msg[dlen], msg[dlen+1])
	}
}

// TestBatchMessage_TheBlockhashIsTheOnlyDifference 釘住投遞資料住在訊息裡這件事本身：
// 同一批付款換一個 blockhash，變的只有那 32 個 bytes，而那 32 個 bytes 在簽名蓋住的
// 範圍裡，所以換信封等於換交易，這條鏈上沒有替換這回事。
func TestBatchMessage_TheBlockhashIsTheOnlyDifference(t *testing.T) {
	acc, _ := solanaFixture()
	items, accounts := solanaBatch(4)
	one, err := chain.NewSolana().BatchMessage(acc, sha256.Sum256([]byte("blockhash-a")), items, accounts)
	if err != nil {
		t.Fatalf("BatchMessage: %v", err)
	}
	two, err := chain.NewSolana().BatchMessage(acc, sha256.Sum256([]byte("blockhash-b")), items, accounts)
	if err != nil {
		t.Fatalf("BatchMessage: %v", err)
	}
	if len(one) != len(two) {
		t.Fatalf("lengths differ: %d vs %d", len(one), len(two))
	}
	off := 3 + 1 + 10*32
	for i := range one {
		inHash := i >= off && i < off+32
		if (one[i] != two[i]) != inHash {
			t.Fatalf("byte %d: only the 32 blockhash bytes at %d..%d may differ", i, off, off+31)
		}
	}
}

// TestBatchMessage_DeduplicatesARepeatedTokenAccount 釘住同一個 merchant 出現兩次時
// 帳戶只列一次：兩筆付款指向同一個索引，訊息比估計短，短的方向是安全的那一邊。
func TestBatchMessage_DeduplicatesARepeatedTokenAccount(t *testing.T) {
	acc, blockhash := solanaFixture()
	items, accounts := solanaBatch(2)
	accounts[1] = accounts[0]
	msg, err := chain.NewSolana().BatchMessage(acc, blockhash, items, accounts)
	if err != nil {
		t.Fatalf("BatchMessage: %v", err)
	}
	distinct, distinctAccounts := solanaBatch(2)
	ref, err := chain.NewSolana().BatchMessage(acc, blockhash, distinct, distinctAccounts)
	if err != nil {
		t.Fatalf("BatchMessage: %v", err)
	}
	if len(msg) != len(ref)-32 {
		t.Fatalf("deduplicated message is %d bytes, want %d (one 32-byte key less)", len(msg), len(ref)-32)
	}
	for _, it := range items {
		if bytes.Count(msg, it.Ref[:]) != 1 {
			t.Fatalf("a ref went missing in the deduplicated message")
		}
	}
}

// TestBatchMessage_RejectsAnAmountOver64Bits 釘住兩條鏈對同一個金額欄位的分歧：
// uint256 裝得下的數字，SPL 的 u64 未必裝得下，裝不下要拒絕，不悄悄截斷。
func TestBatchMessage_RejectsAnAmountOver64Bits(t *testing.T) {
	acc, blockhash := solanaFixture()
	items, accounts := solanaBatch(1)
	items[0].Amount = new(big.Int).Lsh(big.NewInt(1), 64)
	_, err := chain.NewSolana().BatchMessage(acc, blockhash, items, accounts)
	if !errors.Is(err, chain.ErrAmountOverflow) {
		t.Fatalf("err = %v, want ErrAmountOverflow", err)
	}
}

// TestBatchMessage_RefusesTheCreateAccountLeg 釘住還沒蓋的東西不准假裝蓋好了：
// 要開帳戶的付款組不進今天的訊息，bulk 那 42 bytes 在能對答案之前繼續是估計。
func TestBatchMessage_RefusesTheCreateAccountLeg(t *testing.T) {
	acc, blockhash := solanaFixture()
	items, accounts := solanaBatch(2)
	items[1].NewTokenAccount = true
	_, err := chain.NewSolana().BatchMessage(acc, blockhash, items, accounts)
	if !errors.Is(err, chain.ErrNeedsNewAccount) {
		t.Fatalf("err = %v, want ErrNeedsNewAccount", err)
	}
}

// TestBatchMessage_RejectsHalfWiredInputs 釘住空批、零 ref、對不齊的帳戶清單與零值帳戶：
// 每一種都是「還沒準備好」，組出去只會在更遠的地方炸。
func TestBatchMessage_RejectsHalfWiredInputs(t *testing.T) {
	s := chain.NewSolana()
	acc, blockhash := solanaFixture()
	if _, err := s.BatchMessage(acc, blockhash, nil, nil); !errors.Is(err, chain.ErrEmptyBatch) {
		t.Fatalf("empty err = %v, want ErrEmptyBatch", err)
	}
	items, accounts := solanaBatch(2)
	if _, err := s.BatchMessage(acc, blockhash, items, accounts[:1]); !errors.Is(err, chain.ErrAccountsMismatch) {
		t.Fatalf("mismatch err = %v, want ErrAccountsMismatch", err)
	}
	items[0].Ref = [32]byte{}
	if _, err := s.BatchMessage(acc, blockhash, items, accounts); !errors.Is(err, chain.ErrZeroRef) {
		t.Fatalf("zero ref err = %v, want ErrZeroRef", err)
	}
	items, accounts = solanaBatch(2)
	accounts[1] = chain.Pubkey{}
	if _, err := s.BatchMessage(acc, blockhash, items, accounts); !errors.Is(err, chain.ErrZeroAccount) {
		t.Fatalf("zero account err = %v, want ErrZeroAccount", err)
	}
	acc.Mint = chain.Pubkey{}
	items, accounts = solanaBatch(1)
	if _, err := s.BatchMessage(acc, blockhash, items, accounts); !errors.Is(err, chain.ErrZeroAccount) {
		t.Fatalf("zero mint err = %v, want ErrZeroAccount", err)
	}
}

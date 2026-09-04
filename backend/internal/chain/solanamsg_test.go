package chain_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/merkle"
)

// solanaSignedBytes 是一份訊息簽好之後的交易大小：簽名數的 compact-u16（一個 byte）
// 加一個 64 bytes 的簽名（relayer 是唯一的簽名者），再加訊息本身。
// bulk 的 bytes 規則算的是這個總數。
func solanaSignedBytes(msg []byte) uint64 {
	return uint64(1 + 64 + len(msg))
}

// key 造一個定值的假帳戶位址：sha256 過的標籤。真的位址要從鏈上查，形狀是一樣的 32 bytes。
func key(label string) chain.Pubkey {
	return chain.Pubkey(sha256.Sum256([]byte(label)))
}

// runAccounts 是一筆 pay_batch 的五個固定帳戶，全部由標籤決定，跑幾次都一樣。
func runAccounts() chain.SolanaAccounts {
	return chain.SolanaAccounts{
		FeePayer:     key("relayer-fee-payer"),
		Run:          key("run-pda"),
		Vault:        key("vault-token-account"),
		TokenProgram: key("spl-token-program"),
		Program:      key("payout-run-program"),
	}
}

// solanaRun 造 n 筆付款與它們各自的 token 帳戶，merchant 全部不同。
func solanaRun(n int) ([]bulk.Payout, []chain.Pubkey) {
	items := make([]bulk.Payout, 0, n)
	accounts := make([]chain.Pubkey, 0, n)
	for i := 1; i <= n; i++ {
		merchant := fmt.Sprintf("0x%036x%04x", 9, i)
		items = append(items, evmPayout(i, merchant, 100_000_000))
		accounts = append(accounts, key("token-account:"+merchant))
	}
	return items, accounts
}

// payDataLen 是一份 pay_batch 訊息指令資料的長度：discriminator 8、區塊編號 2、項數 1、
// 每項 40、證明一層 32。它決定 compact-u16 的長度前綴占一個還是兩個 byte。
func payDataLen(count, proofHashes int) int {
	return 11 + 40*count + 32*proofHashes
}

// TestPayBatchMessage_MatchesTheBytesRuleInBulk 是這半個 adapter 存在的對照組：bulk 對
// pay_batch 的 Base 280、Item 73、PerLevel 32 本來是照 wire format 手算的估計，這裡拿
// 真的序列化結果對答案，而且不是對公式，是對 bulk.Pack 切同一份名單記下的每一批用量。
// 指令資料滿 128 bytes 時要逐 byte 相等；不滿時 compact-u16 的長度前綴只占一個 byte，
// 真值比估計少一。估計只准高、不准低，這一個 byte 就是全部的誤差。
func TestPayBatchMessage_MatchesTheBytesRuleInBulk(t *testing.T) {
	acc := runAccounts()
	blockhash := sha256.Sum256([]byte("recent-blockhash"))
	for n := 1; n <= 12; n++ {
		items, accounts := solanaRun(n)
		run, err := chain.NewSolana().NewRun(items, accounts)
		if err != nil {
			t.Fatalf("NewRun(%d): %v", n, err)
		}
		plan, err := bulk.Pack(items, bulk.Defaults()["solana"])
		if err != nil {
			t.Fatalf("Pack(%d): %v", n, err)
		}
		if run.Blocks() != len(plan.Batches) {
			t.Fatalf("%d payouts: the run has %d blocks, the plan has %d batches", n, run.Blocks(), len(plan.Batches))
		}
		for _, b := range plan.Batches {
			msg, err := run.PayBatchMessage(acc, blockhash, b.Index-1)
			if err != nil {
				t.Fatalf("PayBatchMessage(%d, block %d): %v", n, b.Index-1, err)
			}
			got, want := solanaSignedBytes(msg), b.Used[0].Used
			if got > want {
				t.Fatalf("%d payouts, batch #%d serializes to %d bytes, above the %d bulk promised", n, b.Index, got, want)
			}
			if payDataLen(len(b.Items), plan.ProofHashes) >= 128 {
				if got != want {
					t.Fatalf("%d payouts, batch #%d serializes to %d bytes, want exactly %d", n, b.Index, got, want)
				}
			} else if got != want-1 {
				t.Fatalf("%d payouts, batch #%d serializes to %d bytes, want %d (one byte under the estimate)", n, b.Index, got, want-1)
			}
		}
	}
}

// TestPayBatchMessage_A300RunLandsOnThePinnedGeometry 釘住文章裡那一輪的每一個數字：
// 300 筆是 38 個區塊、樹深 9、證明 6 個雜湊，滿批 1,056、最後那批 4 筆 764，
// 每一批都跟 bulk 記的用量逐 byte 相等（證明 192 bytes 讓指令資料永遠滿 128）。
func TestPayBatchMessage_A300RunLandsOnThePinnedGeometry(t *testing.T) {
	items, accounts := solanaRun(300)
	run, err := chain.NewSolana().NewRun(items, accounts)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if run.Blocks() != 38 || run.Depth() != 9 {
		t.Fatalf("run = %d blocks depth %d, want 38 blocks depth 9", run.Blocks(), run.Depth())
	}
	plan, err := bulk.Pack(items, bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	acc := runAccounts()
	blockhash := sha256.Sum256([]byte("recent-blockhash"))
	for _, b := range plan.Batches {
		msg, err := run.PayBatchMessage(acc, blockhash, b.Index-1)
		if err != nil {
			t.Fatalf("PayBatchMessage(block %d): %v", b.Index-1, err)
		}
		if got := solanaSignedBytes(msg); got != b.Used[0].Used {
			t.Fatalf("batch #%d serializes to %d bytes, bulk recorded %d", b.Index, got, b.Used[0].Used)
		}
	}
	full, _ := run.PayBatchMessage(acc, blockhash, 0)
	last, _ := run.PayBatchMessage(acc, blockhash, 37)
	if solanaSignedBytes(full) != 1056 || solanaSignedBytes(last) != 764 {
		t.Fatalf("full/last batch = %d/%d bytes, want 1,056/764",
			solanaSignedBytes(full), solanaSignedBytes(last))
	}
}

// 防的情境：跨語言重寫葉子的編碼。merkle 的 golden 釘的是樹的算法，這裡釘的是
// 「一筆付款怎麼變成一片葉子」：編號 u16 LE、token 帳戶 32、金額 u64 LE、ref 32。
// Rust 那一側（contracts/solana/payout-run/tests）用同一個固定輸入釘同一個值，
// 兩邊只要有一個 byte 的編碼歧義，root 就對不上，payer 簽的承諾就付不出去。
func TestNewRun_GoldenRootAcrossImplementations(t *testing.T) {
	items := make([]bulk.Payout, 12)
	accounts := make([]chain.Pubkey, 12)
	for i := range items {
		var ref [32]byte
		for j := range ref {
			ref[j] = byte(0x10 + i)
		}
		items[i] = bulk.Payout{Ref: ref, Merchant: "golden", Amount: big.NewInt(int64(1_000_000 * (i + 1)))}
		for j := range accounts[i] {
			accounts[i][j] = byte(i + 1)
		}
	}
	run, err := chain.NewSolana().NewRun(items, accounts)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	const want = "4e6fa44a1987a3ef74cc6cc2befc3bf6ae7b3f0ec5477bde1b94633974f13f0c"
	root := run.Root()
	if got := fmt.Sprintf("%x", root); got != want {
		t.Fatalf("root = %s, want %s", got, want)
	}
}

// TestPayBatchMessage_ProvesItsOwnBlock 把訊息當成程式收到的東西驗一遍：從尾端把證明
// 切出來，葉子照文件寫的編碼在測試裡重算（不碰 builder 的任何內部），再用
// merkle.VerifyBlock 走回 root。20 筆的 run 有三個區塊，最後一個只有 4 筆真葉子，
// 墊的 PadLeaf 也要跟程式墊的一樣才走得回去。
func TestPayBatchMessage_ProvesItsOwnBlock(t *testing.T) {
	items, accounts := solanaRun(20)
	run, err := chain.NewSolana().NewRun(items, accounts)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	acc := runAccounts()
	blockhash := sha256.Sum256([]byte("recent-blockhash"))
	proofLen := (run.Depth() - 3) * 32
	for block := 0; block < run.Blocks(); block++ {
		msg, err := run.PayBatchMessage(acc, blockhash, block)
		if err != nil {
			t.Fatalf("PayBatchMessage(%d): %v", block, err)
		}
		proofBytes := msg[len(msg)-proofLen:]
		proof := make([][merkle.Size]byte, 0, run.Depth()-3)
		for i := 0; i < len(proofBytes); i += 32 {
			var h [merkle.Size]byte
			copy(h[:], proofBytes[i:i+32])
			proof = append(proof, h)
		}
		leaves := make([][merkle.Size]byte, 8)
		for i := 0; i < 8; i++ {
			g := block*8 + i
			if g >= len(items) {
				leaves[i] = merkle.PadLeaf
				continue
			}
			var data [74]byte
			binary.LittleEndian.PutUint16(data[:2], uint16(g))
			copy(data[2:34], accounts[g][:])
			binary.LittleEndian.PutUint64(data[34:42], items[g].Amount.Uint64())
			copy(data[42:], items[g].Ref[:])
			leaves[i] = merkle.Leaf(data[:])
		}
		if !merkle.VerifyBlock(run.Root(), block, leaves, proof) {
			t.Fatalf("block %d: the proof in the message does not reach the signed root", block)
		}
	}
}

// TestPayBatchMessage_TheBlockhashIsTheOnlyDifference 釘住兩份授權物的分工：blockhash 是
// 被簽掉的一部分，換一個就是另一筆交易，但簽這份訊息的是 relayer，重簽不花 payer 任何
// 東西；payer 簽的 root 這裡一個 byte 都沒動。
func TestPayBatchMessage_TheBlockhashIsTheOnlyDifference(t *testing.T) {
	items, accounts := solanaRun(8)
	run, err := chain.NewSolana().NewRun(items, accounts)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	acc := runAccounts()
	one, err := run.PayBatchMessage(acc, sha256.Sum256([]byte("blockhash-a")), 0)
	if err != nil {
		t.Fatalf("PayBatchMessage: %v", err)
	}
	two, err := run.PayBatchMessage(acc, sha256.Sum256([]byte("blockhash-b")), 0)
	if err != nil {
		t.Fatalf("PayBatchMessage: %v", err)
	}
	if len(one) != len(two) {
		t.Fatalf("lengths differ: %d vs %d", len(one), len(two))
	}
	off := 3 + 1 + 13*32
	for i := range one {
		inHash := i >= off && i < off+32
		if (one[i] != two[i]) != inHash {
			t.Fatalf("byte %d: only the 32 blockhash bytes at %d..%d may differ", i, off, off+31)
		}
	}
}

// TestPayBatchMessage_DeduplicatesARepeatedTokenAccount 釘住同一個 token 帳戶在一個區塊裡
// 出現兩次時帳戶只列一次：兩筆付款指向同一個索引，訊息比估計短，短的方向是安全的那一邊。
// 葉子不去重：編號在葉子裡，同一個帳戶的兩片葉子還是兩片。
func TestPayBatchMessage_DeduplicatesARepeatedTokenAccount(t *testing.T) {
	acc := runAccounts()
	blockhash := sha256.Sum256([]byte("recent-blockhash"))
	items, accounts := solanaRun(2)
	accounts[1] = accounts[0]
	dedup, err := chain.NewSolana().NewRun(items, accounts)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	msg, err := dedup.PayBatchMessage(acc, blockhash, 0)
	if err != nil {
		t.Fatalf("PayBatchMessage: %v", err)
	}
	distinctItems, distinctAccounts := solanaRun(2)
	distinct, err := chain.NewSolana().NewRun(distinctItems, distinctAccounts)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	ref, err := distinct.PayBatchMessage(acc, blockhash, 0)
	if err != nil {
		t.Fatalf("PayBatchMessage: %v", err)
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

// TestNewRun_RejectsWhatTheProgramWouldReject 把程式與合約會拒絕的東西搬到建樹之前：
// 空名單、對不齊的帳戶清單、壞金額、超過 u64 的金額、零 ref、零帳戶、塞不進 u16 的名單。
// 每一種都是「還沒準備好」，簽了 root 才發現等於整輪重簽。
func TestNewRun_RejectsWhatTheProgramWouldReject(t *testing.T) {
	s := chain.NewSolana()
	if _, err := s.NewRun(nil, nil); !errors.Is(err, chain.ErrEmptyRun) {
		t.Fatalf("empty err = %v, want ErrEmptyRun", err)
	}
	items, accounts := solanaRun(2)
	if _, err := s.NewRun(items, accounts[:1]); !errors.Is(err, chain.ErrAccountsMismatch) {
		t.Fatalf("mismatch err = %v, want ErrAccountsMismatch", err)
	}
	for _, amt := range []*big.Int{nil, big.NewInt(0), big.NewInt(-5)} {
		items, accounts = solanaRun(1)
		items[0].Amount = amt
		if _, err := s.NewRun(items, accounts); !errors.Is(err, chain.ErrBadAmount) {
			t.Fatalf("amount %v err = %v, want ErrBadAmount", amt, err)
		}
	}
	items, accounts = solanaRun(1)
	items[0].Amount = new(big.Int).Lsh(big.NewInt(1), 64)
	if _, err := s.NewRun(items, accounts); !errors.Is(err, chain.ErrAmountOverflow) {
		t.Fatalf("overflow err = %v, want ErrAmountOverflow", err)
	}
	items, accounts = solanaRun(2)
	items[1].Ref = [32]byte{}
	if _, err := s.NewRun(items, accounts); !errors.Is(err, chain.ErrZeroRef) {
		t.Fatalf("zero ref err = %v, want ErrZeroRef", err)
	}
	items, accounts = solanaRun(2)
	accounts[1] = chain.Pubkey{}
	if _, err := s.NewRun(items, accounts); !errors.Is(err, chain.ErrZeroAccount) {
		t.Fatalf("zero account err = %v, want ErrZeroAccount", err)
	}
	one, oneAccount := solanaRun(1)
	huge := make([]bulk.Payout, 1<<16+1)
	hugeAccounts := make([]chain.Pubkey, 1<<16+1)
	for i := range huge {
		huge[i], hugeAccounts[i] = one[0], oneAccount[0]
	}
	if _, err := s.NewRun(huge, hugeAccounts); !errors.Is(err, chain.ErrRunTooLarge) {
		t.Fatalf("too large err = %v, want ErrRunTooLarge", err)
	}
}

// TestPayBatchMessage_RejectsHalfWiredCalls 釘住組訊息那一刻的兩種「還沒準備好」：
// 沒填的固定帳戶，與這輪根本沒有的區塊編號。
func TestPayBatchMessage_RejectsHalfWiredCalls(t *testing.T) {
	items, accounts := solanaRun(4)
	run, err := chain.NewSolana().NewRun(items, accounts)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	blockhash := sha256.Sum256([]byte("recent-blockhash"))
	acc := runAccounts()
	acc.Program = chain.Pubkey{}
	if _, err := run.PayBatchMessage(acc, blockhash, 0); !errors.Is(err, chain.ErrZeroAccount) {
		t.Fatalf("zero program err = %v, want ErrZeroAccount", err)
	}
	for _, block := range []int{-1, run.Blocks(), run.Blocks() + 1} {
		if _, err := run.PayBatchMessage(runAccounts(), blockhash, block); !errors.Is(err, chain.ErrNoSuchBlock) {
			t.Fatalf("block %d: err = %v, want ErrNoSuchBlock", block, err)
		}
	}
}

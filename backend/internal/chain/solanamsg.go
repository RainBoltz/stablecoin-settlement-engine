package chain

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
)

// Pubkey 是 Solana 的 32 bytes 帳戶位址。這裡只認原始 bytes：base58 是給人看的表示法，
// 標準函式庫沒有、也不需要有，intent 與名單上存什麼字串是上層的事。
type Pubkey [32]byte

// SolanaAccounts 是一筆 batch 結算訊息裡固定的六個帳戶，跟 bulk 對固定開銷的拆法同一組：
// 付手續費的錢包、payer 的 token 帳戶、代簽的 authority（我們程式的 PDA）、我們的程式、
// SPL Token 程式、mint。
type SolanaAccounts struct {
	FeePayer          Pubkey
	PayerTokenAccount Pubkey
	Authority         Pubkey
	Program           Pubkey
	TokenProgram      Pubkey
	Mint              Pubkey
}

var (
	// ErrAccountsMismatch：token 帳戶的清單跟名單對不上。Solana 的 builder 不自己推導
	// 每個 merchant 的 token 帳戶（那要先去鏈上查），呼叫端查完照名單的順序交進來。
	ErrAccountsMismatch = errors.New("chain: token accounts do not line up with the payouts")
	// ErrAmountOverflow：金額塞不進 u64。SPL Token 的帳戶餘額本身就是 u64
	//（Account 的 amount 欄位，https://github.com/solana-program/token/blob/main/interface/src/state.rs），
	// 所以任何轉帳金額也只有 u64；superset 的金額在 EVM 上編得進 uint256，在這裡就是編不進去。
	ErrAmountOverflow = errors.New("chain: the amount does not fit the u64 an SPL transfer carries")
	// ErrNeedsNewAccount：名單上有 merchant 還沒有地方收這顆 token。開帳戶那段指令會動到
	// 別的程式、固定帳戶的形狀會跟著變，這裡還組不出來，之後再補；bulk 對它的 42 bytes
	// 是估算，在能對答案之前也繼續是估算。
	ErrNeedsNewAccount = errors.New("chain: a payout still needs its token account created")
	// ErrZeroAccount：有一個帳戶位址是零值。零值的 Pubkey 跟零值的 ref 一樣代表「還沒填」。
	ErrZeroAccount = errors.New("chain: an account key is zero")
)

// batchDiscriminator 是指令資料的前 8 個 bytes：sha256("global:settle_batch") 的前 8 個，
// 照 Anchor 對 instruction discriminator 的慣例（https://www.anchor-lang.com/docs/basics/idl），
// 用意跟 paymentref 的 DomainV1 一樣：別的程式對同一段資料不會算出同一個開頭。
func batchDiscriminator() []byte {
	d := sha256.Sum256([]byte("global:settle_batch"))
	return d[:8]
}

// batchDataVersion 是 discriminator 後面的一個版本 byte：之後要換每一項的編法，換版本號就好，
// 舊訊息不會被誤讀成新格式。
const batchDataVersion = 1

// BatchMessage 把一批付款組成一份 Solana 的訊息（message），也就是簽名蓋下去的那段 bytes。
//
// 跟 EVM 那一半最大的不同有兩個。第一，投遞的資料在訊息裡面：recent blockhash 是被簽掉的
// 一部分，所以「同一筆付款換個信封」在這條鏈上不存在，換了 blockhash 就是另一份簽名、
// 另一筆交易。第二，這筆交易會碰到的每一個帳戶都得先列出來，所以名單有多長、訊息就有多長，
// bulk 的 bytes 規則算的就是這份輸出。
//
// 版面照 wire format（https://solana.com/docs/core/transactions/transaction-structure）：
// 表頭三個 byte、帳戶清單（compact-u16 長度＋每個 32 bytes）、blockhash、指令清單。
// 帳戶的順序是規格定的：要簽名的在前、可寫的在前、唯讀的在後，所以是
// fee payer、payer 的 token 帳戶、每個 merchant 的 token 帳戶（可寫），
// 然後 authority、program、token program、mint（唯讀，表頭最後一個數字的那四個）。
func (s *Solana) BatchMessage(acc SolanaAccounts, blockhash [32]byte, items []bulk.Payout, tokenAccounts []Pubkey) ([]byte, error) {
	if len(items) == 0 {
		return nil, ErrEmptyBatch
	}
	if len(tokenAccounts) != len(items) {
		return nil, fmt.Errorf("%w: %d payouts, %d token accounts", ErrAccountsMismatch, len(items), len(tokenAccounts))
	}
	fixed := []Pubkey{acc.FeePayer, acc.PayerTokenAccount, acc.Authority, acc.Program, acc.TokenProgram, acc.Mint}
	for _, k := range fixed {
		if k == (Pubkey{}) {
			return nil, fmt.Errorf("%w: one of the six fixed accounts", ErrZeroAccount)
		}
	}

	// 帳戶清單：同一個 merchant 在一批裡出現兩次，帳戶只列一次，兩項付款指向同一個位置。
	// 訊息因此比 bulk 估的短，方向是安全的那一邊（bulk 的規則寧可高估、不低估）。
	keys := []Pubkey{acc.FeePayer, acc.PayerTokenAccount}
	seen := make(map[Pubkey]int)
	itemIndex := make([]int, len(items))
	for i, it := range items {
		if it.NewTokenAccount {
			return nil, fmt.Errorf("%w: payout %d", ErrNeedsNewAccount, i)
		}
		if it.Amount == nil || it.Amount.Sign() <= 0 {
			return nil, fmt.Errorf("%w: payout %d", ErrBadAmount, i)
		}
		if !it.Amount.IsUint64() {
			return nil, fmt.Errorf("%w: payout %d is %s", ErrAmountOverflow, i, it.Amount)
		}
		if it.Ref.IsZero() {
			return nil, fmt.Errorf("%w: payout %d", ErrZeroRef, i)
		}
		ta := tokenAccounts[i]
		if ta == (Pubkey{}) {
			return nil, fmt.Errorf("%w: token account of payout %d", ErrZeroAccount, i)
		}
		at, ok := seen[ta]
		if !ok {
			at = len(keys)
			seen[ta] = at
			keys = append(keys, ta)
		}
		itemIndex[i] = at
	}
	readonlyAt := len(keys)
	keys = append(keys, acc.Authority, acc.Program, acc.TokenProgram, acc.Mint)

	// 表頭：一個簽名（fee payer）、零個唯讀的簽名者、四個唯讀的非簽名者。
	msg := []byte{1, 0, 4}
	msg = appendCompactU16(msg, len(keys))
	for _, k := range keys {
		msg = append(msg, k[:]...)
	}
	msg = append(msg, blockhash[:]...)

	// 指令清單：只有一條。指令自己帶的帳戶是 payer 的 token 帳戶、authority、token program、
	// mint，再加每一項的 token 帳戶；資料是 discriminator、版本，然後每一項 8 bytes 金額
	//（little-endian，Solana 的慣例）加 32 bytes 的 ref。
	msg = appendCompactU16(msg, 1)
	msg = append(msg, byte(readonlyAt+1)) // program id 的索引
	msg = appendCompactU16(msg, 4+len(items))
	msg = append(msg, 1, byte(readonlyAt), byte(readonlyAt+2), byte(readonlyAt+3))
	for _, at := range itemIndex {
		msg = append(msg, byte(at))
	}
	data := append(batchDiscriminator(), batchDataVersion)
	for _, it := range items {
		v := it.Amount.Uint64()
		for b := 0; b < 8; b++ {
			data = append(data, byte(v>>(8*b)))
		}
		data = append(data, it.Ref[:]...)
	}
	msg = appendCompactU16(msg, len(data))
	return append(msg, data...), nil
}

// appendCompactU16 是 Solana 的 compact-u16 長度編碼：7 個 bit 一組、little-endian，
// 最高位表示後面還有；127 以內一個 byte、16383 以內兩個。這就是「128 以下省一個 byte」
// 的那個編碼，bulk 的線性估算在資料長度跨過 128 的那一刻會多算一個 byte。
func appendCompactU16(b []byte, v int) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

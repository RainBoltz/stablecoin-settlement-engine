package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/merkle"
)

// Pubkey 是 Solana 的 32 bytes 帳戶位址。這裡只認原始 bytes：base58 是給人看的表示法，
// 標準函式庫沒有、也不需要有，intent 與名單上存什麼字串是上層的事。
type Pubkey [32]byte

// SolanaAccounts 是一筆 pay_batch 交易裡固定的五個帳戶，跟 bulk 對固定開銷的拆法同一組：
// 付手續費的 relayer 錢包、run PDA、vault 的 token 帳戶、SPL Token 程式、payout-run 程式。
// payer 不在名單上：他簽的是 root，不是這筆交易，這是整個 run 設計的重點。
type SolanaAccounts struct {
	FeePayer     Pubkey
	Run          Pubkey
	Vault        Pubkey
	TokenProgram Pubkey
	Program      Pubkey
}

var (
	// ErrEmptyRun：整輪名單是空的。空名單連 root 都算不出來，payer 沒有東西可簽。
	ErrEmptyRun = errors.New("chain: the payout run is empty")
	// ErrRunTooLarge：名單長到葉子編號塞不進 u16。鏈上程式的葉子編號與區塊編號都是 u16，
	// 這是 wire 上的形狀；bulk 的 bytes 規則其實在 16,384 筆就先擋下了（證明再深一層，
	// 滿批就超過 1,232），這條只是 builder 自己對自己的輸出負責。
	ErrRunTooLarge = errors.New("chain: a leaf index does not fit the u16 the program reads")
	// ErrAccountsMismatch：token 帳戶的清單跟名單對不上。builder 不自己推導每個 merchant 的
	// token 帳戶（那要先去鏈上查），呼叫端查完照名單的順序交進來。
	ErrAccountsMismatch = errors.New("chain: token accounts do not line up with the payouts")
	// ErrAmountOverflow：金額塞不進 u64。SPL Token 的帳戶餘額本身就是 u64
	//（Account 的 amount 欄位，https://github.com/solana-program/token/blob/main/interface/src/state.rs），
	// 所以任何轉帳金額也只有 u64；superset 的金額在 EVM 上編得進 uint256，在這裡就是編不進去。
	ErrAmountOverflow = errors.New("chain: the amount does not fit the u64 an SPL transfer carries")
	// ErrZeroAccount：有一個帳戶位址是零值。零值的 Pubkey 跟零值的 ref 一樣代表「還沒填」。
	ErrZeroAccount = errors.New("chain: an account key is zero")
	// ErrNoSuchBlock：要組的區塊不在這輪 run 上。
	ErrNoSuchBlock = errors.New("chain: the run has no such block")
)

// runBlock 是對齊區塊的寬度：8。它跟 bulk 的 Align 與鏈上程式的 BLOCK 是同一個數字，
// 三個地方一起動才動得了。
const runBlock = 8

// payBatchDiscriminator 是指令資料的前 8 個 bytes：sha256("global:pay_batch") 的前 8 個，
// 照 Anchor 對 instruction discriminator 的慣例（https://www.anchor-lang.com/docs/basics/idl），
// 用意跟 paymentref 的 DomainV1 一樣：別的程式對同一段資料不會算出同一個開頭。
func payBatchDiscriminator() []byte {
	d := sha256.Sum256([]byte("global:pay_batch"))
	return d[:8]
}

// PayoutRun 是一輪撥款在鏈下的另一半：整份名單的葉子與蓋住它們的樹。
// payer 簽的 root 從這裡來，relayer 每一批帶上鏈的證明也從這裡來，
// 所以它一定蓋住「整輪」而不是一批：樹只有一棵，批只是樹上對齊的區塊。
type PayoutRun struct {
	items    []bulk.Payout
	accounts []Pubkey
	tree     merkle.Tree
}

// NewRun 把整份名單收成一輪 run：逐筆算葉子、建樹。名單的順序就是葉子的順序，
// 跟 bulk.Pack 切區塊用的是同一個順序，兩邊照同一份名單走才對得上。
//
// 葉子的編碼跟鏈上程式重算的那一段逐 byte 相同：domain byte 由 merkle.Leaf 蓋上，
// 內容是葉子編號（u16 LE）、merchant 的 token 帳戶（32）、金額（u64 LE）、ref（32）。
// merchant 這裡放的是 token 帳戶而不是錢包地址，因為程式驗證時拿的是「實際被轉帳的
// 那個帳戶」：relayer 把帳戶掉包，葉子就對不上 root，這正是名單保護自己的方式。
func (s *Solana) NewRun(items []bulk.Payout, tokenAccounts []Pubkey) (*PayoutRun, error) {
	if len(items) == 0 {
		return nil, ErrEmptyRun
	}
	if len(items) > 1<<16 {
		return nil, fmt.Errorf("%w: %d payouts", ErrRunTooLarge, len(items))
	}
	if len(tokenAccounts) != len(items) {
		return nil, fmt.Errorf("%w: %d payouts, %d token accounts", ErrAccountsMismatch, len(items), len(tokenAccounts))
	}
	leaves := make([][merkle.Size]byte, len(items))
	for i, it := range items {
		if it.Amount == nil || it.Amount.Sign() <= 0 {
			return nil, fmt.Errorf("%w: payout %d", ErrBadAmount, i)
		}
		if !it.Amount.IsUint64() {
			return nil, fmt.Errorf("%w: payout %d is %s", ErrAmountOverflow, i, it.Amount)
		}
		if it.Ref.IsZero() {
			return nil, fmt.Errorf("%w: payout %d", ErrZeroRef, i)
		}
		if tokenAccounts[i] == (Pubkey{}) {
			return nil, fmt.Errorf("%w: token account of payout %d", ErrZeroAccount, i)
		}
		var data [2 + 32 + 8 + 32]byte
		binary.LittleEndian.PutUint16(data[:2], uint16(i))
		copy(data[2:34], tokenAccounts[i][:])
		binary.LittleEndian.PutUint64(data[34:42], it.Amount.Uint64())
		copy(data[42:], it.Ref[:])
		leaves[i] = merkle.Leaf(data[:])
	}
	// 不足一個區塊的名單墊到 8：鏈上程式的樹最小就是一個區塊寬，兩邊的 root 才會相同。
	// 再往上的墊（到 2 的冪次）Build 自己會做。
	for len(leaves) < runBlock {
		leaves = append(leaves, merkle.PadLeaf)
	}
	tree, err := merkle.Build(leaves)
	if err != nil {
		return nil, err
	}
	return &PayoutRun{
		items:    append([]bulk.Payout(nil), items...),
		accounts: append([]Pubkey(nil), tokenAccounts...),
		tree:     tree,
	}, nil
}

// Root 回報 payer 要簽的那 32 bytes：整份名單收成的一個承諾，init_run 圈存時寫進 PDA 的
// 就是它。root 裡沒有 blockhash、沒有任何會過期的東西，這是 payer 只簽一次就夠的原因。
func (r *PayoutRun) Root() [merkle.Size]byte {
	return r.tree.Root()
}

// Blocks 回報這輪切成幾個對齊區塊。它跟 bulk.Pack 對同一份名單切出的付款批一一對應：
// bulk 的付款批 #k 就是這裡的 block k-1，一邊從 1 數（報告給人看）、一邊從 0 數（跟著 wire）。
func (r *PayoutRun) Blocks() int {
	return (len(r.items) + runBlock - 1) / runBlock
}

// Depth 回報樹高。每一份 pay_batch 訊息帶的證明是 Depth-3 個雜湊：
// 區塊自己的 3 層在鏈上重算，剩下的層每層補一個兄弟節點。
func (r *PayoutRun) Depth() int {
	return r.tree.Depth()
}

// PayBatchMessage 把第 block 個對齊區塊組成一份 pay_batch 的訊息（message），
// 也就是 relayer 簽名蓋下去的那段 bytes。
//
// 跟 EVM 那一半最大的不同有兩個。第一，投遞的資料在訊息裡面：recent blockhash 是被簽掉的
// 一部分，換一個 blockhash 就是另一份簽名、另一筆交易。同一個事實對不同的簽名者是兩種
// 判決：payer 預簽會過期，所以他簽的是沒有 blockhash 的 root；relayer 的訊息過期就
// 重抓一個 blockhash 重組重簽，root 一個 byte 都不用動。第二，這筆交易會碰到的每一個
// 帳戶都得先列出來，所以區塊裡有幾個 merchant、訊息就多長，bulk 的 bytes 規則算的
// 正是這份輸出。
//
// 版面照 wire format（https://solana.com/docs/core/transactions/transaction-structure）：
// 表頭三個 byte、帳戶清單（compact-u16 長度＋每個 32 bytes）、blockhash、指令清單。
// 帳戶的順序是規格定的：要簽名的在前、可寫的在前、唯讀的在後，所以是
// relayer 的錢包（唯一的簽名者）、run PDA、vault、每個 merchant 的 token 帳戶（可寫），
// 然後 SPL Token 程式與 payout-run 程式（唯讀，表頭最後一個數字的那兩個）。
func (r *PayoutRun) PayBatchMessage(acc SolanaAccounts, blockhash [32]byte, block int) ([]byte, error) {
	fixed := []Pubkey{acc.FeePayer, acc.Run, acc.Vault, acc.TokenProgram, acc.Program}
	for _, k := range fixed {
		if k == (Pubkey{}) {
			return nil, fmt.Errorf("%w: one of the five fixed accounts", ErrZeroAccount)
		}
	}
	if block < 0 || block*runBlock >= len(r.items) {
		return nil, fmt.Errorf("%w: block %d, the run has %d", ErrNoSuchBlock, block, r.Blocks())
	}
	start := block * runBlock
	end := start + runBlock
	if end > len(r.items) {
		end = len(r.items)
	}
	items := r.items[start:end]

	// 帳戶清單：同一個 token 帳戶在一個區塊裡出現兩次，帳戶只列一次，兩項付款指向同一個
	// 位置。訊息因此比 bulk 估的短，方向是安全的那一邊（bulk 的規則寧可高估、不低估）。
	keys := []Pubkey{acc.FeePayer, acc.Run, acc.Vault}
	seen := make(map[Pubkey]int)
	itemIndex := make([]int, len(items))
	for i := range items {
		ta := r.accounts[start+i]
		at, ok := seen[ta]
		if !ok {
			at = len(keys)
			seen[ta] = at
			keys = append(keys, ta)
		}
		itemIndex[i] = at
	}
	readonlyAt := len(keys)
	keys = append(keys, acc.TokenProgram, acc.Program)

	// 表頭：一個簽名（relayer）、零個唯讀的簽名者、兩個唯讀的非簽名者。
	msg := []byte{1, 0, 2}
	msg = appendCompactU16(msg, len(keys))
	for _, k := range keys {
		msg = append(msg, k[:]...)
	}
	msg = append(msg, blockhash[:]...)

	// 指令清單：只有一條。指令自己帶的帳戶是 run、vault、SPL Token 程式，再照名單順序放
	// 每一項的 token 帳戶；資料是 discriminator、區塊編號（u16 LE）、項數，然後每一項
	// 8 bytes 金額（little-endian，Solana 的慣例）加 32 bytes 的 ref，最後整份
	// 「區塊走回 root」的證明，一層一個 32 bytes 的雜湊，整批共用。
	msg = appendCompactU16(msg, 1)
	msg = append(msg, byte(readonlyAt+1)) // program id 的索引
	msg = appendCompactU16(msg, 3+len(items))
	msg = append(msg, 1, 2, byte(readonlyAt))
	for _, at := range itemIndex {
		msg = append(msg, byte(at))
	}
	data := payBatchDiscriminator()
	data = binary.LittleEndian.AppendUint16(data, uint16(block))
	data = append(data, byte(len(items)))
	for _, it := range items {
		data = binary.LittleEndian.AppendUint64(data, it.Amount.Uint64())
		data = append(data, it.Ref[:]...)
	}
	proof, err := r.tree.BlockProof(block, 3)
	if err != nil {
		return nil, err
	}
	for _, h := range proof {
		data = append(data, h[:]...)
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

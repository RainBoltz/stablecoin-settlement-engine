package intent

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Intent 是一筆付款在鏈下的代表。它從 API 收到請求那一刻誕生，之後每一層
// （queue、relayer、listener）都只是在它身上蓋章，不會各自另開一份紀錄。
//
// 欄位刻意少：今天只放狀態機需要的東西。金額、token、雙方地址是「這筆付款是什麼」，
// 狀態、版本、歷程是「它走到哪了」。ID 先當成一個不透明字串，它跟 API 層的去重鍵
// 以及貫穿鏈上鏈下的追蹤鍵是什麼關係，之後會專門討論。
type Intent struct {
	ID string

	// Chain 用「協定:網路」表示，例如 evm:31337。之後多鏈時 adapter 靠它分派。
	Chain string
	// Token 是穩定幣合約地址（EVM 上是 0x 開頭的 hex；其他鏈各自的表示法）。
	Token string
	// Payer 出錢、Merchant 收錢。角色名沿用 devnet 的叫法。
	Payer    string
	Merchant string
	// Amount 是「請款金額」，用 token 的最小單位（USDC 是 6 位小數，所以 1 USDC = 1_000_000）。
	// 用 big.Int 而不用 uint64：18 位小數的 token 一筆百萬就會爆掉 uint64。
	Amount *big.Int

	State State
	// Version 每轉移一次加一。存檔時拿它做 compare-and-swap：
	// 兩個 worker 同時想推同一筆 intent，只有一個會成功，另一個拿到 ErrVersionConflict。
	Version uint64
	// TxHash 是「目前被認定在鏈上的那一筆交易」。settling 期間可能空著（還沒進區塊），
	// confirming 時一定有值，reorg 退回 settling 時會清掉。歷史上的雜湊都在 History 裡。
	TxHash string

	CreatedAt time.Time
	UpdatedAt time.Time
	// ExpiresAt 是簽名迴圈的期限：付款人在這之前沒回簽名，intent 就過期。
	// 零值代表不設限（測試方便；正式環境的 API 一定會給）。
	ExpiresAt time.Time

	// History 是這筆 intent 走過的每一步，只增不減。它回答「怎麼走到這裡的」，
	// 這是光看 State 回答不了的：一筆 settled 的 intent 有沒有經歷過 reorg，只有 History 知道。
	History []Transition
}

// Transition 是 History 裡的一步。
type Transition struct {
	From   State
	To     State
	By     Actor
	At     time.Time
	TxHash string
	Reason string
}

// String 用固定格式印一步，Example 與 CLI 都用這個格式，文章會直接貼。
func (t Transition) String() string {
	s := fmt.Sprintf("%-12s -> %-12s by %-8s", t.From, t.To, t.By)
	if t.TxHash != "" {
		s += "  tx " + t.TxHash
	}
	if t.Reason != "" {
		s += "  (" + t.Reason + ")"
	}
	return strings.TrimRight(s, " ")
}

// Spec 是建立 intent 時「這筆付款是什麼」的那一半。
type Spec struct {
	ID       string
	Chain    string
	Token    string
	Payer    string
	Merchant string
	Amount   *big.Int
	// ExpiresAt 可為零值，見 Intent.ExpiresAt。
	ExpiresAt time.Time
}

// ErrInvalidSpec：建立 intent 時的基本檢查沒過。
var ErrInvalidSpec = errors.New("intent: invalid spec")

// New 建立一筆處於 created 的 intent。這是唯一的入口：狀態機不接受憑空出現在別的狀態的 intent。
//
// 只做「這筆資料自己就看得出來的」檢查（金額為正、必填欄位有值）。
// 付款人有沒有錢、token 是不是黑名單，那是鏈上的事，這裡不知道也不假裝知道。
func New(spec Spec, now time.Time) (*Intent, error) {
	switch {
	case spec.ID == "":
		return nil, fmt.Errorf("%w: id is required", ErrInvalidSpec)
	case spec.Chain == "" || spec.Token == "" || spec.Payer == "" || spec.Merchant == "":
		return nil, fmt.Errorf("%w: chain, token, payer, merchant are required", ErrInvalidSpec)
	case spec.Amount == nil || spec.Amount.Sign() <= 0:
		return nil, fmt.Errorf("%w: amount must be positive", ErrInvalidSpec)
	case !spec.ExpiresAt.IsZero() && !spec.ExpiresAt.After(now):
		return nil, fmt.Errorf("%w: expiresAt must be in the future", ErrInvalidSpec)
	}
	return &Intent{
		ID:        spec.ID,
		Chain:     spec.Chain,
		Token:     spec.Token,
		Payer:     spec.Payer,
		Merchant:  spec.Merchant,
		Amount:    new(big.Int).Set(spec.Amount),
		State:     StateCreated,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: spec.ExpiresAt,
	}, nil
}

// Clone 回傳一份深拷貝。Store 進出都用拷貝，避免呼叫端拿著指標繞過 Apply 改狀態。
func (it *Intent) Clone() *Intent {
	if it == nil {
		return nil
	}
	c := *it
	if it.Amount != nil {
		c.Amount = new(big.Int).Set(it.Amount)
	}
	c.History = append([]Transition(nil), it.History...)
	return &c
}

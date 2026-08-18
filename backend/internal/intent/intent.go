package intent

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// Intent 是一筆付款在鏈下的代表。它從 API 收到請求那一刻誕生，之後每一層
// （queue、relayer、listener）都只是在它身上蓋章，不會各自另開一份紀錄。
//
// 欄位刻意少：金額、token、雙方地址是「這筆付款是什麼」，狀態、版本、歷程是「它走到哪了」。
//
// 一筆 intent 身上有兩個識別碼：ID 是伺服器產生的不透明字串（pi_…），鏈下每一層拿它找資料；
// Ref 是從 ID 與付款條件推導出來的 32 bytes（見 paymentref），唯一會跟著交易上鏈的那個。
// API 層的去重鍵（Idempotency Key）不在這裡：它只活在 API 邊界，intent 落地之後就用不到了。
type Intent struct {
	ID string
	// Ref 是這筆付款的 PaymentRef，New 的時候從 Terms 算出來，之後永遠不變。
	// Store 存檔時會重算一次核對，所以拿到一筆 intent 就能確定它的條件從落地到現在沒被動過。
	Ref paymentref.Ref

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
	it := &Intent{
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
	}
	// ref 在落地那一刻就有，早於任何交易存在。所以 ledger 與 queue 在錢動之前就拿得到它，
	// 不用等 tx hash；而 tx hash 之後可能有好幾個（重送、reorg），ref 只有一個。
	it.Ref = paymentref.Derive(it.Terms())
	return it, nil
}

// Terms 是這筆 intent 被 commit 進 PaymentRef 的那幾個欄位。只有「這筆付款是什麼」，沒有「走到哪了」。
func (it *Intent) Terms() paymentref.Terms {
	amount := ""
	if it.Amount != nil {
		amount = it.Amount.String()
	}
	return paymentref.Terms{
		IntentID: it.ID, Chain: it.Chain, Token: it.Token, Payer: it.Payer, Merchant: it.Merchant, Amount: amount,
	}
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

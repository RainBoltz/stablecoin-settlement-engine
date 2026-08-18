// Package ledger 是鏈下的帳本：錢在這個系統裡「動了沒、動了多少、現在在誰名下」的紀錄。
//
// 它跟 intent 的分工：intent 的 History 回答「這筆付款走到哪一步」，ledger 回答「錢在哪」。
// 一個 merchant 這個月收了多少 USDC、還有多少在路上、USDT 的轉帳稅吃掉多少，intent store 要全表掃過去算，
// 而且 intent 上只有一個 Amount（請款金額），Day 2 講的「請款金額」與「實收金額」要分開，得有另一本帳。
//
// 兩條設計紀律，整個 package 都圍著它們轉：
//
//   - 複式記帳（double-entry）：每一筆 Entry 至少兩條腿，同一種 asset，金額加總必須是零。
//     錢不會憑空出現、也不會憑空消失：payer 少 100，merchant 就要多 100；merchant 只多 99.9，
//     那 0.1 就得有第三條腿（fee）接住，不然 ledger 拒收。幽靈支付與轉帳稅在帳上藏不住，就是靠這條。
//     形式上沿用 Martin Fowler 的 Accounting Patterns（https://martinfowler.com/apsupp/accounting.pdf）：
//     entry 帶正負號、一筆 transaction 裡的 entries 加總為零；沒有用會計師的 debit / credit 兩欄，
//     因為這裡不做財報，正負號對工程師比較不會搞反，而不變量是同一條。
//
//   - 只加不改（append-only）：Journal 只有 Append，沒有 update、沒有 delete。錢動之前先記 hold，
//     鏈上確認了記 post，確定沒動記 void，三筆都是新的一列，沒有一列會被改。餘額不是存起來的欄位，
//     是把 journal 從頭算一遍算出來的投影（projection）；記憶體版每次都重算，資料庫版會存一張 balance 表當快取，
//     但真相永遠是 journal。每一列的 Hash 把上一列的 Hash 也算進去（跟 git commit 一樣），
//     所以任何一列被改過，從那一列起整條鏈都對不上，Verify 抓得到。
//
// hold / post / void 三段式沿用 TigerBeetle 的 two-phase transfer
// （https://docs.tigerbeetle.com/coding/two-phase-transfers/：pending 先占位，post 或 void 收尾，
// post 的金額可以少於 pending，而且「All transfers are immutable」，收尾是新的一筆紀錄不是改舊的）；
// Modern Treasury 的 Ledgers 也是 pending / posted 兩種餘額分開算
// （https://docs.moderntreasury.com/ledgers/docs/transaction-status-and-balances）。
// 本 package 為本系列從零設計，只取這兩個公開設計裡「錢只動一次」需要的那部分。
package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// Asset 是「哪條鏈上的哪顆幣」。同一筆 Entry 只能有一種 asset：USDC 與 USDT 不能互抵，
// evm:1 的 USDC 與 evm:31337 的 USDC 也是兩種東西。
type Asset struct {
	// Chain 用「協定:網路」表示，跟 intent.Chain 同一種寫法，例如 evm:31337。
	Chain string
	// Token 是合約地址，跟 intent.Token 一樣不做正規化：ledger 只保證「跟 intent 上存的一模一樣」。
	Token string
}

// String 印成 chain/token，例如 evm:31337/0x5FbD…。
func (a Asset) String() string { return a.Chain + "/" + a.Token }

// Account 是帳本上的一個科目，形式是「kind:owner」。
//
// 這個系統是 non-custodial 的，我們從頭到尾不持有客戶的錢，所以科目記的不是「我們的資產負債」，
// 而是「這筆付款讓誰的錢動了」：payer 的錢出去、merchant 的錢進來、被 token 自己抽走的稅。
// 科目不用事先開戶：第一次出現在某條腿上就存在，因為 payer 與 merchant 都是鏈上地址，
// 有多少個是鏈決定的，不是我們決定的。
type Account string

// PayerAccount 是付款人的科目：payer:<address>。
func PayerAccount(addr string) Account { return Account("payer:" + addr) }

// MerchantAccount 是收款人的科目：merchant:<address>。
func MerchantAccount(addr string) Account { return Account("merchant:" + addr) }

// FeeAccount 是被 token 合約自己抽走的那一份：fee:<token>。
// 轉帳稅（Day 2 的「金額對不上的」那一類）收款人是 token 的 owner，我們不認識他，但錢確實離開了 payer 而沒到 merchant，
// 那條腿總得有個地方落。之後對帳時，這個科目有餘額就代表有 token 在抽稅。
func FeeAccount(token string) Account { return Account("fee:" + token) }

// Leg 是 Entry 的一條腿：哪個科目、動多少。正數是進、負數是出。
type Leg struct {
	Account Account
	// Amount 用 token 的最小單位，跟 intent.Amount 一樣用 big.Int。
	Amount *big.Int
}

// Kind 是 Entry 的種類。三種合起來就是 two-phase transfer：hold 占位，post 或 void 收尾。
type Kind string

const (
	// KindHold：錢還沒動，先在帳上占位。relayer 把 intent 推到 settling 之後、廣播之前記這一筆，
	// 跟「先記 settling 再廣播」同一個道理：帳上先有這筆，交易才出門。它進的是 pending 餘額。
	KindHold Kind = "hold"
	// KindPost：鏈上確認錢動了，把 hold 收成 posted。腿記的是「實際發生的事」（listener 從鏈上讀到的實收金額），
	// 不是請款金額，兩者的差就是 fee 那條腿。只有 listener（與收尾的 operator）會記這一筆，
	// 因為只有他們手上有鏈上的事實，跟「只有 listener 能宣告 settled」是同一條規則。
	KindPost Kind = "post"
	// KindVoid：確定沒動錢，把 hold 放掉。永遠失敗的那一類（黑名單、凍結）走這裡。沒有腿，pending 歸零就是全部。
	KindVoid Kind = "void"
)

// Entry 是 journal 裡的一列。呼叫端填前半段；Seq、PrevHash、Hash 由 Journal 在 Append 時填。
//
// 一列進了 journal 就不會再變。要修正只能再加一列：post 錯了不是改 post，是開一筆反向的 intent，
// 走一次新的 hold / post。這跟狀態機「修正靠新 intent」是同一件事的兩面。
type Entry struct {
	// ID 由呼叫端給，慣例是「<intent id>/<kind>」，例如 pi_0001/hold。同一個 ID 送兩次，內容一樣就是重放（no-op），
	// 內容不一樣就是 bug（ErrConflict）。queue 與 listener 都是 at-least-once 的，同一筆記兩次是日常，
	// 所以這一層跟 Apply、Claim 一樣，重放要能安靜地過。
	ID string
	// Ref 是這筆錢對應的 PaymentRef。ledger 是 ref 離開 API 之後經過的第一站；對帳引擎拿鏈上撈到的 ref 來這裡找帳。
	Ref  paymentref.Ref
	Kind Kind
	// Holds 只有 post 與 void 會填：被收尾的那筆 hold 的 ID。一筆 hold 只能被收尾一次，post 或 void 擇一，
	// 第二次一律 ErrAlreadyResolved。這是「錢只動一次」在帳本上的長相。
	Holds string
	Asset Asset
	// Legs 至少兩條、加總為零、每條非零。void 沒有腿。
	Legs []Leg
	// By 是誰記的，跟 intent 的 actor 同一套名字（relayer、listener、operator）。
	By string
	// At 由呼叫端傳進來，跟 intent.Request 一樣，ledger 才測得動。
	At time.Time
	// TxHash 是 post 時鏈上那筆交易；hold 時還沒有。
	TxHash string
	// Memo 是給人看的一句話：void 的理由、post 時的備註。
	Memo string

	// Seq 從 1 開始、密集遞增，由 Journal 給。
	Seq uint64
	// PrevHash 是上一列的 Hash，第一列是零值。
	PrevHash [32]byte
	// Hash 是 sha256(PrevHash 加上這一列的正規編碼)，見 hash.go。
	Hash [32]byte
}

// 拒絕的理由分開列，跟 intent 一樣：ErrInvalidEntry、ErrUnbalanced、ErrConflict、ErrNoSuchHold 是 bug，要告警；
// ErrAlreadyResolved 可能是正常的競爭（listener 與 operator 同時想收尾），輸的一方放手。
var (
	// ErrInvalidEntry：欄位缺了、種類不對、腿的形狀不對。
	ErrInvalidEntry = errors.New("ledger: invalid entry")
	// ErrUnbalanced：腿加總不是零。這是複式記帳唯一的硬規則，沒有例外、沒有容差。
	ErrUnbalanced = errors.New("ledger: legs do not sum to zero")
	// ErrConflict：同一個 ID 已經在 journal 裡，而且內容不一樣。
	ErrConflict = errors.New("ledger: entry id already used with different content")
	// ErrNoSuchHold：post 或 void 指的那筆 hold 不存在、不是 hold、或 ref / asset 對不上。
	ErrNoSuchHold = errors.New("ledger: no matching hold")
	// ErrAlreadyResolved：那筆 hold 已經被 post 或 void 收尾過了。
	ErrAlreadyResolved = errors.New("ledger: hold already resolved")
	// ErrNotFound：沒有這一列。
	ErrNotFound = errors.New("ledger: not found")
	// ErrChainBroken：Verify 重算 hash 鏈時對不上，回報第一個壞掉的 Seq。
	ErrChainBroken = errors.New("ledger: hash chain broken")
)

// Validate 檢查一列「自己就看得出來的」問題：欄位齊不齊、腿平不平。跟別的列的關係（ID 撞了沒、hold 在不在）
// 要看 journal，那是 Append 的事。
func (e Entry) Validate() error {
	switch {
	case e.ID == "":
		return fmt.Errorf("%w: id is required", ErrInvalidEntry)
	case e.Ref.IsZero():
		return fmt.Errorf("%w: ref is required", ErrInvalidEntry)
	case e.Asset.Chain == "" || e.Asset.Token == "":
		return fmt.Errorf("%w: asset chain and token are required", ErrInvalidEntry)
	case e.By == "":
		return fmt.Errorf("%w: by is required", ErrInvalidEntry)
	case e.At.IsZero():
		return fmt.Errorf("%w: at is required", ErrInvalidEntry)
	}
	switch e.Kind {
	case KindHold:
		if e.Holds != "" {
			return fmt.Errorf("%w: hold must not reference another hold", ErrInvalidEntry)
		}
		return validateLegs(e.Legs)
	case KindPost:
		if e.Holds == "" {
			return fmt.Errorf("%w: post must reference the hold it resolves", ErrInvalidEntry)
		}
		return validateLegs(e.Legs)
	case KindVoid:
		if e.Holds == "" {
			return fmt.Errorf("%w: void must reference the hold it resolves", ErrInvalidEntry)
		}
		if len(e.Legs) != 0 {
			return fmt.Errorf("%w: void has no legs", ErrInvalidEntry)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidEntry, e.Kind)
	}
}

// validateLegs 是複式記帳的規則本體：至少兩條、每條非零、科目不重複、加總為零。
// 「至少兩條」是因為一條腿的分錄就是「錢憑空出現／消失」，那正是這本帳要擋的東西。
func validateLegs(legs []Leg) error {
	if len(legs) < 2 {
		return fmt.Errorf("%w: need at least two legs, got %d", ErrInvalidEntry, len(legs))
	}
	sum := new(big.Int)
	seen := make(map[Account]bool, len(legs))
	for i, l := range legs {
		if l.Account == "" {
			return fmt.Errorf("%w: leg %d has no account", ErrInvalidEntry, i)
		}
		if l.Amount == nil || l.Amount.Sign() == 0 {
			return fmt.Errorf("%w: leg %d (%s) has zero amount", ErrInvalidEntry, i, l.Account)
		}
		if seen[l.Account] {
			return fmt.Errorf("%w: account %s appears twice", ErrInvalidEntry, l.Account)
		}
		seen[l.Account] = true
		sum.Add(sum, l.Amount)
	}
	if sum.Sign() != 0 {
		return fmt.Errorf("%w: sum is %s", ErrUnbalanced, sum)
	}
	return nil
}

// Same 回報兩列「呼叫端填的那半」是不是一模一樣。Append 用它判斷同 ID 是重放還是衝突。
func (e Entry) Same(o Entry) bool {
	if e.ID != o.ID || e.Ref != o.Ref || e.Kind != o.Kind || e.Holds != o.Holds || e.Asset != o.Asset ||
		e.By != o.By || !e.At.Equal(o.At) || e.TxHash != o.TxHash || e.Memo != o.Memo || len(e.Legs) != len(o.Legs) {
		return false
	}
	for i := range e.Legs {
		if e.Legs[i].Account != o.Legs[i].Account || e.Legs[i].Amount.Cmp(o.Legs[i].Amount) != 0 {
			return false
		}
	}
	return true
}

// clone 深拷貝一列，Journal 進出都用拷貝，避免呼叫端拿著指標改到存的那份。
func (e Entry) clone() Entry {
	c := e
	c.Legs = make([]Leg, len(e.Legs))
	for i, l := range e.Legs {
		c.Legs[i] = Leg{Account: l.Account, Amount: new(big.Int).Set(l.Amount)}
	}
	return c
}

// String 用固定格式印一列，Example 會用、文章會貼：序號、種類、ref 前十碼、誰記的、每條腿、tx、memo。
// 地址與 ref 都縮寫，因為這是印給人看的；程式要完整值就讀欄位。
func (e Entry) String() string {
	s := fmt.Sprintf("#%-2d %-4s %s  by %-8s", e.Seq, e.Kind, shortHex(e.Ref.String()), e.By)
	for _, l := range e.Legs {
		s += fmt.Sprintf("  %s %s", shortAccount(l.Account), signed(l.Amount))
	}
	if e.TxHash != "" {
		s += "  tx " + e.TxHash
	}
	if e.Memo != "" {
		s += "  (" + e.Memo + ")"
	}
	return s
}

// shortHex 把 0x 開頭的長 hex 縮成前 10 碼加 …，其他字串原樣回傳。
func shortHex(s string) string {
	if len(s) > 12 && strings.HasPrefix(s, "0x") {
		return s[:10] + "…"
	}
	return s
}

// shortAccount 把 kind:0x… 裡的地址縮成 0x1234…abcd。
func shortAccount(a Account) string {
	kind, owner, ok := strings.Cut(string(a), ":")
	if !ok || len(owner) != 42 || !strings.HasPrefix(owner, "0x") {
		return string(a)
	}
	return kind + ":" + owner[:6] + "…" + owner[38:]
}

func signed(a *big.Int) string {
	if a.Sign() > 0 {
		return "+" + a.String()
	}
	return a.String()
}

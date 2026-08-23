// Package txfee 決定一筆卡住的交易「下一次要出多少價、要送什麼內容」。
//
// 昨天的 txseq 決定一筆交易站在發送帳戶的哪一格，也把「拿了號卻不知道有沒有送出去」定義成一個洞。這個 package 處理洞後面
// 那件事：同一個號上再送一筆出價更高的交易，把那一格搶回來。EVM 叫它 replacement，鏈上規則只有一條，就是同一個帳戶、
// 同一個 nonce 的交易最多只有一筆會進區塊，所以替換不會讓錢多動一次。
//
// 四條鏈能不能替換，跟昨天「號是誰算的」是同一條線：
//
//   - EVM：可以。同 nonce 再送一筆，出價要比原本那筆高一個門檻，節點才肯把 mempool 裡的舊交易換掉
//     （geth 的 --txpool.pricebump，預設 10，https://geth.ethereum.org/docs/fundamentals/command-line-options）。
//     EIP-1559 之後一筆交易出兩個價（maxFeePerGas 與 maxPriorityFeePerGas，
//     https://ethereum.org/en/developers/docs/gas/#maxfee），兩個都要過門檻。
//   - TON：可以。同一個 seqno 再送一則 external message，錢包合約只收剛好等於當前 seqno 的那一則
//     （https://docs.ton.org/contracts/standard/wallets/how-it-works），所以一樣是「最多一則會被收下」。
//     但 TON 沒有出價競爭，訊息帶的是 valid_until，時間到就被丟掉。
//   - Solana：不行。交易由簽名識別，重送同一份簽好的交易是冪等的（驗證節點在 blockhash 的窗口內記住處理過的簽名，
//     https://solana.com/docs/advanced/confirmation）；但改了 priority fee 就是另一份簽名、另一筆交易，
//     兩筆都可能上鏈。要加速只能重新組一筆，而重新組一筆就得自己保證不會付兩次。
//   - SUI：不行，而且危險。同一個 owned object 的同一個版本被兩筆還沒 finalize 的交易用到就是 equivocation，
//     那個 object 會被鎖到這個 epoch 結束（https://docs.sui.io/guides/developer/sui-101/avoid-equivocation）。
//     卡住的時候正確動作是原封不動重送同一筆。
//
// 所以這個 package 只服務前兩類，跟 txseq.Counter 服務的是同一批鏈。它是純函式：不碰鏈、不碰資料庫、不看時鐘，
// 呼叫端把「上一次出了多少價、廣播過幾次、卡多久」交給它，它回一個 Plan。
//
// 本 package 為本系列從零設計，只取公開文件裡需要的那部分。
package txfee

import (
	"errors"
	"fmt"
	"math/big"
)

// wei 換算用的常數。gwei 是 1e9 wei，這裡多留三位小數（1e6 wei）讓印出來的價看得出加價的尾數。
var (
	milliGwei = big.NewInt(1_000_000)
	thousand  = big.NewInt(1000)
	hundred   = big.NewInt(100)
)

// ErrCeiling：加價之後會超過 Policy.MaxCap，這筆交易不能再替換了。
//
// 它是「放棄」的訊號，不是「重試」的訊號：出價一旦到頂，加速與取消都送不出去，因為兩者都要贏過 mempool 裡的舊交易。
var ErrCeiling = errors.New("txfee: bumped fee cap exceeds the ceiling")

// Fee 是一筆 EVM 交易出的價，單位 wei。EIP-1559 之後是兩個數字：
//
//   - Cap（maxFeePerGas）：這筆交易每單位 gas 最多願意付多少，包含燒掉的 base fee。
//   - Tip（maxPriorityFeePerGas）：其中最多分多少給提案者，這是真正決定排隊順序的那個數字。
//
// 兩個都存在的理由是它們的天花板意義不同：Cap 是「我最多付多少」，Tip 是「我出價多少插隊」。替換的時候兩個都要加，
// 只加一個節點不收。
type Fee struct {
	Cap *big.Int
	Tip *big.Int
}

// NewFee 用 gwei 組一個 Fee，測試與設定檔用。小數點以下的 gwei 用 wei 直接寫。
func NewFee(capGwei, tipGwei int64) Fee {
	g := big.NewInt(1_000_000_000)
	return Fee{
		Cap: new(big.Int).Mul(big.NewInt(capGwei), g),
		Tip: new(big.Int).Mul(big.NewInt(tipGwei), g),
	}
}

// Zero 回報這是不是一個沒填過的 Fee。沒有紀錄的時候呼叫端要拿 Policy.Base 來補，不能拿零去加價（零加一成還是零）。
func (f Fee) Zero() bool {
	return f.Cap == nil || f.Tip == nil || f.Cap.Sign() == 0 || f.Tip.Sign() == 0
}

// String 用固定格式印一個出價，Example 與 relayer 的 Report 會直接貼這個格式。
func (f Fee) String() string {
	if f.Zero() {
		return "cap - tip -"
	}
	return fmt.Sprintf("cap %s tip %s", Gwei(f.Cap), Gwei(f.Tip))
}

// Gwei 把 wei 印成三位小數的 gwei。加價的尾數（33 gwei 加一成是 36.3）要看得見，不然讀者會以為門檻是整數。
func Gwei(v *big.Int) string {
	if v == nil {
		return "-"
	}
	q := new(big.Int).Quo(v, milliGwei)
	whole, frac := new(big.Int).QuoRem(q, thousand, new(big.Int))
	return fmt.Sprintf("%s.%03d gwei", whole, frac)
}

// Policy 是替換的規則，四個旋鈕。
type Policy struct {
	// Base 是第一次廣播出的價。真的接上鏈時這個數字要從鏈上讀（base fee 加上建議的 priority fee），
	// 那是 chain adapter 的事；這裡先寫死一個，讓 relayer 這一側可以完整跑起來。
	Base Fee
	// BumpPercent 是每次替換至少要加多少百分比。節點自己有一個門檻（geth 的 --txpool.pricebump 預設 10），
	// 低於它會被回 replacement transaction underpriced，等於白送一趟。
	BumpPercent uint64
	// MaxCap 是 Cap 的天花板。沒有天花板的加價迴圈會在鏈上塞車的時候把手續費燒到失控，
	// 而一筆付款值不值得用兩倍手續費推上去是商業決定，不是工程決定。
	MaxCap *big.Int
	// MaxTries 是一筆 intent 最多廣播幾次（含第一次）。超過就不再想辦法讓這筆付款成功，改成把號清出來。
	MaxTries int
}

// DefaultPolicy：30 gwei / 2 gwei 起跳，每次加 10%，Cap 加到 45 gwei 為止，一筆 intent 最多廣播 3 次。
//
// 三個數字都是這裡設的，不是誰的建議值：起價要換成從鏈上讀的，天花板與次數要換成商業上能接受的金額。
func DefaultPolicy() Policy {
	return Policy{
		Base:        NewFee(30, 2),
		BumpPercent: 10,
		MaxCap:      NewFee(45, 0).Cap,
		MaxTries:    3,
	}
}

// Bump 算出下一次要出的價：兩個欄位同時乘上 (100 + BumpPercent) / 100，無條件進位。
//
// 進位不是為了保守，是為了正確：節點算的門檻是整數除法的結果，我們這邊如果跟著無條件捨去，
// 遇到除不盡的數字就會剛好差一個 wei 進不去。加價一次多付的那一個 wei，比被退回來重送一趟便宜。
//
// 超過 MaxCap 回 ErrCeiling，而且不回一個「勉強等於天花板」的價：那個價贏不過舊交易，送出去只是浪費一次嘗試。
func (p Policy) Bump(f Fee) (Fee, error) {
	if f.Zero() {
		f = p.Base
	}
	next := Fee{Cap: bump(f.Cap, p.BumpPercent), Tip: bump(f.Tip, p.BumpPercent)}
	if p.MaxCap != nil && next.Cap.Cmp(p.MaxCap) > 0 {
		return Fee{}, fmt.Errorf("%w: %s > %s", ErrCeiling, Gwei(next.Cap), Gwei(p.MaxCap))
	}
	return next, nil
}

// bump 算 ceil(v * (100 + pct) / 100)。
func bump(v *big.Int, pct uint64) *big.Int {
	n := new(big.Int).Mul(v, new(big.Int).SetUint64(100+pct))
	q, r := new(big.Int).QuoRem(n, hundred, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

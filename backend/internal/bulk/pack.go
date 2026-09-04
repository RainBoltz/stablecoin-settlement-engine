package bulk

import (
	"errors"
	"fmt"
	"math/bits"
)

// ErrNoRules：這份 Limits 一條規則都沒有。多半是拿 Defaults() 查了一條還沒實作的鏈，
// 查不到會拿到零值；不擋下來的話整份名單會被裝進同一批，然後在送出去的時候才炸開。
var ErrNoRules = errors.New("bulk: the chain has no limits configured")

// Pack 照名單原本的順序切批，一批一筆交易。
//
// 順序不重排是刻意的：一份撥款名單是人與系統看得懂的東西，重排之後對不回原本那份檔案，
// 出事時要靠 event 的順序回頭比對就會多一層轉換。Align 模式下順序還多背一件事：
// 名單的順序就是葉子的順序，payer 簽的 root 蓋住這個順序，重排等於換了一棵樹。
//
// 切法看 Limits.Align。0 是貪心：一項一項試著塞進當前這一批，塞不下就換新的一批（EVM）。
// 大於 0 是對齊：一批固定切在 Align 的倍數邊界、最多 Align 項，因為整批要共用一份
// 「對齊區塊走回 root」的證明；貪心切出來的邊界跟樹的區塊對不齊，證明就得一片葉子一份。
// 要開帳戶的那幾項不影響任何一批的價錢：它們被抽出來排進 Prepare，在送錢之前先做掉。
func Pack(items []Payout, l Limits) (Plan, error) {
	if len(items) == 0 {
		return Plan{}, ErrEmptyRun
	}
	if len(l.Rules) == 0 {
		return Plan{}, fmt.Errorf("%w: %q", ErrNoRules, l.Chain)
	}

	plan := Plan{Chain: l.Chain, Payouts: len(items), RentUnit: l.RentUnit}
	var err error
	if l.Align == 0 {
		err = packGreedy(&plan, items, l)
	} else {
		err = packAligned(&plan, items, l)
	}
	if err != nil {
		return Plan{}, err
	}
	if err := packPrepare(&plan, items, l); err != nil {
		return Plan{}, err
	}
	plan.Rent = uint64(plan.NewAccounts) * l.NewAccountRent
	return plan, nil
}

// packGreedy 一項一項試著塞進當前這一批，塞不下就換新的一批。
func packGreedy(plan *Plan, items []Payout, l Limits) error {
	used := make([]uint64, len(l.Rules))
	var batch Batch
	for i, it := range items {
		for _, r := range l.Rules {
			if r.Base+r.Item > r.Cap {
				return fmt.Errorf("%w: payout %d needs %d %s and a transaction only has %d",
					ErrItemTooLarge, i, r.Base+r.Item, r.Unit, r.Cap)
			}
		}
		if len(batch.Items) > 0 && !fits(used, l) {
			plan.Batches = append(plan.Batches, seal(batch, used, l, len(plan.Batches)+1))
			batch = Batch{}
			used = make([]uint64, len(l.Rules))
		}
		for j, r := range l.Rules {
			used[j] += r.Item
		}
		batch.Items = append(batch.Items, it)
	}
	plan.Batches = append(plan.Batches, seal(batch, used, l, len(plan.Batches)+1))
	return nil
}

// packAligned 把名單切在 Align 的倍數邊界上，一批最多 Align 項。
// 樹高由墊滿後的名單長度決定，每一批的證明因此一樣長，一批的價錢只跟它裝了幾項有關。
func packAligned(plan *Plan, items []Payout, l Limits) error {
	align := int(l.Align)
	width := align
	for width < len(items) {
		width *= 2
	}
	plan.Levels = bits.Len(uint(width)) - 1
	plan.ProofHashes = plan.Levels - (bits.Len(uint(align)) - 1)

	overhead := make([]uint64, len(l.Rules))
	for j, r := range l.Rules {
		overhead[j] = r.Base + uint64(plan.ProofHashes)*r.PerLevel
		if overhead[j]+r.Item > r.Cap {
			return fmt.Errorf("%w: one payout plus the proof needs %d %s and a transaction only has %d",
				ErrItemTooLarge, overhead[j]+r.Item, r.Unit, r.Cap)
		}
		if overhead[j]+uint64(align)*r.Item > r.Cap {
			return fmt.Errorf("%w: a full block of %d needs %d %s and a transaction only has %d",
				ErrBlockTooLarge, align, overhead[j]+uint64(align)*r.Item, r.Unit, r.Cap)
		}
	}

	for start := 0; start < len(items); start += align {
		end := start + align
		if end > len(items) {
			end = len(items)
		}
		batch := Batch{Index: len(plan.Batches) + 1, Items: items[start:end]}
		batch.Used = make([]Usage, len(l.Rules))
		for j, r := range l.Rules {
			batch.Used[j] = Usage{
				Unit: r.Unit,
				Used: overhead[j] + uint64(len(batch.Items))*r.Item,
				Cap:  r.Cap,
			}
		}
		plan.Batches = append(plan.Batches, batch)
	}
	return nil
}

// packPrepare 把要開帳戶的那幾項抽出來，照 PrepareRules 排成送錢之前的 prepare batch。
// 沒有 PrepareRules 的鏈只數 NewAccounts、不排批：EVM 上這個旗標沒有對應的工作。
func packPrepare(plan *Plan, items []Payout, l Limits) error {
	var creations []Payout
	for _, it := range items {
		if it.NewTokenAccount {
			creations = append(creations, it)
		}
	}
	plan.NewAccounts = len(creations)
	if len(l.PrepareRules) == 0 || len(creations) == 0 {
		return nil
	}

	used := make([]uint64, len(l.PrepareRules))
	var batch Batch
	for i, it := range creations {
		for _, r := range l.PrepareRules {
			if r.Base+r.Item > r.Cap {
				return fmt.Errorf("%w: account %d needs %d %s and a transaction only has %d",
					ErrItemTooLarge, i, r.Base+r.Item, r.Unit, r.Cap)
			}
		}
		if len(batch.Items) > 0 && !fitsRules(used, l.PrepareRules) {
			plan.Prepare = append(plan.Prepare, sealPrep(batch, used, l.PrepareRules, len(plan.Prepare)+1))
			batch = Batch{}
			used = make([]uint64, len(l.PrepareRules))
		}
		for j, r := range l.PrepareRules {
			used[j] += r.Item
		}
		batch.Items = append(batch.Items, it)
		batch.NewAccounts++
	}
	plan.Prepare = append(plan.Prepare, sealPrep(batch, used, l.PrepareRules, len(plan.Prepare)+1))
	return nil
}

// fits 回報「再加一項之後每一條規則都還沒超過」。任何一條過不了就要換一批：
// 上限之間沒有互相折抵這回事，交易太長就是送不出去，跟它還剩多少帳戶額度無關。
func fits(used []uint64, l Limits) bool {
	return fitsRules(used, l.Rules)
}

func fitsRules(used []uint64, rules []Rule) bool {
	for j, r := range rules {
		if r.Base+used[j]+r.Item > r.Cap {
			return false
		}
	}
	return true
}

// seal 把當前這一批收成 Batch，順便把每一條規則的用量記下來。
// 用量記的是含固定開銷的總數，因為報告要回答的是「這一批離上限還有多遠」。
func seal(b Batch, used []uint64, l Limits, index int) Batch {
	b.Index = index
	b.Used = make([]Usage, len(l.Rules))
	for j, r := range l.Rules {
		b.Used[j] = Usage{Unit: r.Unit, Used: r.Base + used[j], Cap: r.Cap}
	}
	return b
}

// sealPrep 是 seal 的 prepare 版本：規則換成 PrepareRules，批標記成 Prep。
func sealPrep(b Batch, used []uint64, rules []Rule, index int) Batch {
	b.Index = index
	b.Prep = true
	b.Used = make([]Usage, len(rules))
	for j, r := range rules {
		b.Used[j] = Usage{Unit: r.Unit, Used: r.Base + used[j], Cap: r.Cap}
	}
	return b
}

package bulk

import (
	"errors"
	"fmt"
)

// ErrNoRules：這份 Limits 一條規則都沒有。多半是拿 Defaults() 查了一條還沒實作的鏈，
// 查不到會拿到零值；不擋下來的話整份名單會被裝進同一批，然後在送出去的時候才炸開。
var ErrNoRules = errors.New("bulk: the chain has no limits configured")

// Pack 照名單原本的順序，一項一項試著塞進當前這一批，塞不下就換新的一批。
//
// 順序不重排是刻意的：一份撥款名單是人與系統看得懂的東西，重排之後對不回原本那份檔案，
// 出事時要靠 event 的順序回頭比對就會多一層轉換。裝箱演算法可以把總批數壓得更低，
// 但省下來的是幾筆交易的手續費，換掉的是「第幾批對到名單第幾行」這件事，不划算。
//
// 每一項貴不貴不一樣（要先開帳戶的那幾項比較貴），所以這裡一項一項試；
// 先算出「一批幾項」再切的話，會漏掉那幾項比較貴的。
func Pack(items []Payout, l Limits) (Plan, error) {
	if len(items) == 0 {
		return Plan{}, ErrEmptyRun
	}
	if len(l.Rules) == 0 {
		return Plan{}, fmt.Errorf("%w: %q", ErrNoRules, l.Chain)
	}

	plan := Plan{Chain: l.Chain, Payouts: len(items), RentUnit: l.RentUnit}
	used := make([]uint64, len(l.Rules))
	var batch Batch

	for i, it := range items {
		cost := costOf(it, l)
		for j, r := range l.Rules {
			if r.Base+cost[j] > r.Cap {
				return Plan{}, fmt.Errorf("%w: payout %d needs %d %s and a transaction only has %d",
					ErrItemTooLarge, i, r.Base+cost[j], r.Unit, r.Cap)
			}
		}
		if len(batch.Items) > 0 && !fits(used, cost, l) {
			plan.Batches = append(plan.Batches, seal(batch, used, l, len(plan.Batches)+1))
			batch = Batch{}
			used = make([]uint64, len(l.Rules))
		}
		for j := range l.Rules {
			used[j] += cost[j]
		}
		batch.Items = append(batch.Items, it)
		if it.NewTokenAccount {
			batch.NewAccounts++
			plan.NewAccounts++
		}
	}
	plan.Batches = append(plan.Batches, seal(batch, used, l, len(plan.Batches)+1))
	plan.Rent = uint64(plan.NewAccounts) * l.NewAccountRent
	return plan, nil
}

// costOf 是一項付款在每一條規則上要付多少，不含一筆交易的固定開銷。
func costOf(it Payout, l Limits) []uint64 {
	cost := make([]uint64, len(l.Rules))
	for j, r := range l.Rules {
		cost[j] = r.Item
		if it.NewTokenAccount {
			cost[j] += r.Extra
		}
	}
	return cost
}

// fits 回報「再加一項之後每一條規則都還沒超過」。任何一條過不了就要換一批：
// 上限之間沒有互相折抵這回事，交易太長就是送不出去，跟它還剩多少帳戶額度無關。
func fits(used, cost []uint64, l Limits) bool {
	for j, r := range l.Rules {
		if r.Base+used[j]+cost[j] > r.Cap {
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

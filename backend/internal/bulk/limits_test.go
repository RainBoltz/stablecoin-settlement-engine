package bulk_test

import (
	"strings"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
)

// 這裡每一個數字都是從別人的公開文件抄來的，抄錯或改版之後沒人記得出處，
// 整個 package 就只是一組看起來很像的常數。所以出處跟數字綁在一起，缺一個就不給過。
func TestDefaults_EveryNumberHasAPublicSource(t *testing.T) {
	for chain, limits := range bulk.Defaults() {
		if len(limits.Rules) == 0 {
			t.Fatalf("%s has no rules", chain)
		}
		if limits.Chain != chain {
			t.Fatalf("%s is keyed as %q", limits.Chain, chain)
		}
		for _, r := range limits.Rules {
			if !strings.HasPrefix(r.Source, "https://") {
				t.Fatalf("%s/%s has no public source", chain, r.Unit)
			}
			if r.Cap == 0 || r.Item == 0 {
				t.Fatalf("%s/%s has an empty cap or item cost", chain, r.Unit)
			}
			if r.Base >= r.Cap {
				t.Fatalf("%s/%s: base %d does not leave room under cap %d", chain, r.Unit, r.Base, r.Cap)
			}
		}
	}
}

// MaxItems 是給呼叫端估規模用的捷徑，Pack 走的是另一條路（一項一項試）。
// 兩條路對同一份「每項都一樣貴」的名單必須給同一個答案，不然估出來的規模會騙人。
func TestDefaults_MaxItemsMatchesWhatPackProduces(t *testing.T) {
	for chain, limits := range bulk.Defaults() {
		want := limits.MaxItems()
		if want <= 0 {
			t.Fatalf("%s: MaxItems = %d", chain, want)
		}
		plan, err := bulk.Pack(items(want+1, 0), limits)
		if err != nil {
			t.Fatalf("%s: Pack: %v", chain, err)
		}
		if len(plan.Batches) != 2 {
			t.Fatalf("%s: %d payouts became %d batches, want 2", chain, want+1, len(plan.Batches))
		}
		if got := len(plan.Batches[0].Items); got != want {
			t.Fatalf("%s: first batch holds %d, MaxItems says %d", chain, got, want)
		}
	}
}

// 有好幾條規則的時候，一批多大由最緊的那一條決定，不是平均、也不是第一條。
func TestLimits_MaxItemsTakesTheTightestRule(t *testing.T) {
	l := bulk.Limits{
		Chain: "two-rules",
		Rules: []bulk.Rule{
			{Unit: "loose", Cap: 1000, Base: 0, Item: 1, Source: "test"},
			{Unit: "tight", Cap: 100, Base: 0, Item: 10, Source: "test"},
		},
	}
	if got := l.MaxItems(); got != 10 {
		t.Fatalf("MaxItems = %d, want 10", got)
	}
}

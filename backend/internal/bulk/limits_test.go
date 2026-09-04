package bulk_test

import (
	"strings"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
)

// 防的情境：有人改上限的數字卻說不出出處。這個 package 的每一個數字都是抄來的，
// 抄來的數字要留下它是從哪裡抄的；付款批與 prepare batch 的規則一視同仁。
func TestDefaults_EveryNumberHasAPublicSource(t *testing.T) {
	for chain, l := range bulk.Defaults() {
		rules := append(append([]bulk.Rule{}, l.Rules...), l.PrepareRules...)
		if len(rules) == 0 {
			t.Fatalf("%s has no rules at all", chain)
		}
		for _, r := range rules {
			if !strings.HasPrefix(r.Source, "https://") {
				t.Fatalf("%s rule %q has no public source", chain, r.Unit)
			}
			if r.Cap == 0 || r.Item == 0 {
				t.Fatalf("%s rule %q has a zero cap or item", chain, r.Unit)
			}
			if r.Base+r.Item > r.Cap {
				t.Fatalf("%s rule %q cannot fit even one item", chain, r.Unit)
			}
		}
		if l.Align != 0 && l.Align&(l.Align-1) != 0 {
			t.Fatalf("%s align = %d, must be a power of two: the tree is binary", chain, l.Align)
		}
	}
}

// 防的情境：MaxItems 跟 Pack 各講各話。它是給呼叫端估規模的上界：
// 貪心的鏈（EVM）它就是批的大小；對齊的鏈它被 Align 封頂，但一批帶著證明之後
// 實際塞得下幾項只有 Pack 知道，所以這裡只釘「不高估不低估的那一半」。
func TestDefaults_MaxItemsMatchesWhatPackProduces(t *testing.T) {
	evm := bulk.Defaults()["evm"]
	if got := evm.MaxItems(); got != 562 {
		t.Fatalf("evm MaxItems = %d, want 562", got)
	}
	plan, err := bulk.Pack(payouts(600, 0), evm)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(plan.Batches) != 2 || len(plan.Batches[0].Items) != 562 || len(plan.Batches[1].Items) != 38 {
		t.Fatalf("evm 600 items packed into %d batches, want 562+38", len(plan.Batches))
	}

	sol := bulk.Defaults()["solana"]
	if got := sol.MaxItems(); got != 8 {
		t.Fatalf("solana MaxItems = %d, want 8 (capped by Align)", got)
	}
}

// 防的情境：好幾條規則同時管一批的時候，最緊的那一條說了算。
func TestLimits_MaxItemsTakesTheTightestRule(t *testing.T) {
	l := bulk.Limits{
		Chain: "test",
		Rules: []bulk.Rule{
			{Unit: "loose", Cap: 1000, Base: 0, Item: 1},
			{Unit: "tight", Cap: 100, Base: 20, Item: 10},
		},
	}
	if got := l.MaxItems(); got != 8 {
		t.Fatalf("MaxItems = %d, want 8 from the tight rule", got)
	}
}

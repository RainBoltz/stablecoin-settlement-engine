package chain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// fake 是餵給 Register 的可調 adapter：從一個答得齊的底子開始，每條測試把其中一題改壞。
type fake struct {
	protocol string
	seq      txseq.Sequencer
	fin      finality.Policy
	limits   bulk.Limits
}

func (f fake) Protocol() string           { return f.protocol }
func (f fake) Sequencer() txseq.Sequencer { return f.seq }
func (f fake) Finality() finality.Policy  { return f.fin }
func (f fake) BatchLimits() bulk.Limits   { return f.limits }

// goodFake 是一個四題都答得出來的假鏈。協定名刻意不用真的那四條，免得跟 Default() 打架。
func goodFake() fake {
	return fake{
		protocol: "fakechain",
		seq:      txseq.Unordered{},
		fin:      finality.Policy{Marker: "notarised", RequireMarker: true},
		limits: bulk.Limits{
			Chain: "fakechain",
			Rules: []bulk.Rule{{
				Unit: "bytes", Cap: 1000, Base: 100, Item: 10,
				Source: "https://example.org/spec",
			}},
			RentUnit: "wei",
		},
	}
}

// TestRegistry_FindsTheAdapterByProtocol 釘住 For 認協定不認網路：同一個協定的每個網路
// 對回同一個 adapter，跟 listener 對不可逆規則的用法一致。
func TestRegistry_FindsTheAdapterByProtocol(t *testing.T) {
	reg := chain.Default()
	local, err := reg.For("evm:31337")
	if err != nil {
		t.Fatalf("For(evm:31337): %v", err)
	}
	mainnet, err := reg.For("evm:1")
	if err != nil {
		t.Fatalf("For(evm:1): %v", err)
	}
	if local != mainnet {
		t.Fatalf("evm:31337 and evm:1 should share one adapter")
	}
	bare, err := reg.For("evm")
	if err != nil || bare != local {
		t.Fatalf("For(evm) = %v, %v; want the same adapter", bare, err)
	}
}

// TestRegistry_RejectsAChainWithNoAdapter 釘住「查不到不給預設」：錯誤要包 ErrUnknownChain，
// 而且訊息裡要有協定名，值班的人才知道是哪條鏈沒接。四條鏈都接齊之後，拿一條這個系統
// 從來沒有打算接的鏈來問。
func TestRegistry_RejectsAChainWithNoAdapter(t *testing.T) {
	reg := chain.Default()
	_, err := reg.For("aptos:mainnet")
	if !errors.Is(err, chain.ErrUnknownChain) {
		t.Fatalf("For(aptos:mainnet) = %v, want ErrUnknownChain", err)
	}
	if !strings.Contains(err.Error(), `"aptos"`) {
		t.Fatalf("the error should name the protocol: %v", err)
	}
}

// TestRegistry_APolicyAloneIsNotAnAdapter 釘住這個 package 存在的理由：finality.Defaults()
// 認識 sui，但一份不可逆規則不等於一條接好的鏈。一個沒註冊 sui adapter 的 Registry
// 對 sui:mainnet 照樣回 ErrUnknownChain，不會因為別的 package 認得它就放行。
func TestRegistry_APolicyAloneIsNotAnAdapter(t *testing.T) {
	if _, ok := finality.Defaults()["sui"]; !ok {
		t.Fatalf("the premise broke: finality.Defaults() no longer knows sui")
	}
	reg, err := chain.NewRegistry(chain.NewEVM(), chain.NewSolana(), chain.NewTON())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, err = reg.For("sui:mainnet")
	if !errors.Is(err, chain.ErrUnknownChain) {
		t.Fatalf("For(sui:mainnet) = %v, want ErrUnknownChain", err)
	}
}

// TestRegistry_RejectsADuplicateProtocol 釘住「一個協定一個代表」：第二個 evm adapter
// 就是第二條發號線，Register 要擋下來。
func TestRegistry_RejectsADuplicateProtocol(t *testing.T) {
	reg, err := chain.NewRegistry(chain.NewEVM())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := reg.Register(chain.NewEVM()); !errors.Is(err, chain.ErrDuplicateAdapter) {
		t.Fatalf("second Register = %v, want ErrDuplicateAdapter", err)
	}
}

// TestRegistry_RejectsAnAdapterThatSkipsAQuestion 逐題把一個好 adapter 改壞，
// 每一種「接了一半」的樣子都要在註冊那一刻被擋下來，免得半夜由一筆付款去發現。
func TestRegistry_RejectsAnAdapterThatSkipsAQuestion(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(*fake)
	}{
		{"no protocol name", func(f *fake) { f.protocol = "" }},
		{"protocol carries a network", func(f *fake) { f.protocol = "fakechain:1"; f.limits.Chain = "fakechain:1" }},
		{"protocol is not lower-case", func(f *fake) { f.protocol = "Fakechain"; f.limits.Chain = "Fakechain" }},
		{"nil sequencer", func(f *fake) { f.seq = nil }},
		{"finality accepts anything", func(f *fake) { f.fin = finality.Policy{} }},
		{"marker required but unnamed", func(f *fake) { f.fin = finality.Policy{RequireMarker: true} }},
		{"limits describe another chain", func(f *fake) { f.limits.Chain = "evm" }},
		{"no batch rules", func(f *fake) { f.limits.Rules = nil }},
		{"no rent unit", func(f *fake) { f.limits.RentUnit = "" }},
		{"a rule with a zero cap", func(f *fake) { f.limits.Rules[0].Cap = 0 }},
		{"a rule with no room for one payout", func(f *fake) { f.limits.Rules[0].Base = 995 }},
		{"a rule with no public source", func(f *fake) { f.limits.Rules[0].Source = "trust me" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := goodFake()
			tc.spoil(&f)
			_, err := chain.NewRegistry(f)
			if !errors.Is(err, chain.ErrIncompleteAdapter) {
				t.Fatalf("NewRegistry = %v, want ErrIncompleteAdapter", err)
			}
		})
	}
}

// TestRegistry_AcceptsACompleteAdapter 對照組：goodFake 本人要註冊得進去，
// 不然上面那些 subtest 擋下的可能是底子本身，未必是被改壞的那一題。
func TestRegistry_AcceptsACompleteAdapter(t *testing.T) {
	reg, err := chain.NewRegistry(goodFake())
	if err != nil {
		t.Fatalf("NewRegistry(goodFake) = %v, want nil", err)
	}
	if _, err := reg.For("fakechain:testnet"); err != nil {
		t.Fatalf("For(fakechain:testnet) = %v, want the fake", err)
	}
}

// TestRegistry_ListsProtocolsSorted 釘住 Protocols 的輸出穩定：報告與錯誤訊息會印它。
func TestRegistry_ListsProtocolsSorted(t *testing.T) {
	ps := chain.Default().Protocols()
	if len(ps) != 4 || ps[0] != "evm" || ps[1] != "solana" || ps[2] != "sui" || ps[3] != "ton" {
		t.Fatalf("Protocols() = %v, want [evm solana sui ton]", ps)
	}
}

package chain_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// TestEVM_HandsOutOrderedSlots 釘住 evm 那一題的答案是真的發號器：號碼從 0 開始、
// 收尾之後才輪到下一個。
func TestEVM_HandsOutOrderedSlots(t *testing.T) {
	ctx := context.Background()
	seq := chain.NewEVM().Sequencer()
	account := "0x0A11cE0000000000000000000000000000000001"
	first, err := seq.Reserve(ctx, account)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !first.Ordered || first.Value != 0 {
		t.Fatalf("first reservation = %+v, want ordered #0", first)
	}
	if err := seq.Resolve(ctx, first, txseq.SentYes); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	second, err := seq.Reserve(ctx, account)
	if err != nil {
		t.Fatalf("second Reserve: %v", err)
	}
	if second.Value != 1 {
		t.Fatalf("second reservation = %+v, want #1", second)
	}
}

// TestEVM_SequencerIsSharedNotMinted 釘住「發號的狀態只能有一份」：同一個 adapter
// 每次交出同一個 sequencer。每問一次就鑄一顆新的，等於兩條互相撞號的線。
func TestEVM_SequencerIsSharedNotMinted(t *testing.T) {
	e := chain.NewEVM()
	if e.Sequencer() != e.Sequencer() {
		t.Fatalf("Sequencer() should hand back the same instance every time")
	}
}

// TestSolana_NeedsNoSlot 釘住 solana 那一題的答案是「不需要」，跟「沒有答案」是兩回事：
// 取號照樣成功、位置是空的，補號則回 ErrNoGap（沒有序列就沒有空缺）。
func TestSolana_NeedsNoSlot(t *testing.T) {
	ctx := context.Background()
	seq := chain.NewSolana().Sequencer()
	res, err := seq.Reserve(ctx, "any-solana-account")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Ordered {
		t.Fatalf("reservation = %+v, want no slot", res)
	}
	if _, err := seq.ReserveGap(ctx, "any-solana-account"); !errors.Is(err, txseq.ErrNoGap) {
		t.Fatalf("ReserveGap = %v, want ErrNoGap", err)
	}
}

// TestTON_HandsOutOrderedSlotsLikeEVM 釘住 ton 那一題的答案跟 evm 是同一種：seqno 是發送方
// 自己往上加的，所以是一個真的發號器，而且跟 evm 一樣每次交出同一個實例。
func TestTON_HandsOutOrderedSlotsLikeEVM(t *testing.T) {
	ctx := context.Background()
	a := chain.NewTON()
	if a.Sequencer() != a.Sequencer() {
		t.Fatalf("Sequencer() should hand back the same instance every time")
	}
	wallet := "0:1111111111111111111111111111111111111111111111111111111111111111"
	res, err := a.Sequencer().Reserve(ctx, wallet)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !res.Ordered || res.Value != 0 {
		t.Fatalf("reservation = %+v, want ordered #0", res)
	}
	if err := a.Sequencer().Resolve(ctx, res, txseq.SentYes); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

// TestReplacement_OnlyEVMCan 釘住替換是選答題：evm 交得出規則，solana 與 ton 連介面都沒實作，
// 呼叫端拿到一個乾脆的 false，沒有零值規則這種東西。ton 尤其容易被誤答：它跟 evm 一樣自己發號，
// 但錢包合約對同一個 seqno 只收一則，而 external message 沒有出價可以加，卡住只能原封不動重送。
func TestReplacement_OnlyEVMCan(t *testing.T) {
	pol, ok := chain.Replacement(chain.NewEVM())
	if !ok {
		t.Fatalf("evm should be able to replace")
	}
	if pol.BumpPercent != 10 || pol.MaxTries != 3 {
		t.Fatalf("evm replacement policy = %+v, want the txfee defaults", pol)
	}
	if _, ok := chain.Replacement(chain.NewSolana()); ok {
		t.Fatalf("solana must not answer the replacement question")
	}
	if _, ok := chain.Replacement(chain.NewTON()); ok {
		t.Fatalf("ton must not answer the replacement question")
	}
}

// TestAdapters_AnswerWithTheRulesThatAlreadyExist 釘住「adapter 只當索引」：
// 三個 adapter 交出來的規則就是 finality.Defaults() 與 bulk.Defaults() 裡的那幾條本人，
// 一個數字都不自己抄。誰改了那兩份設定，這裡自動跟著變，不會出現兩個版本。
func TestAdapters_AnswerWithTheRulesThatAlreadyExist(t *testing.T) {
	for _, a := range []chain.Adapter{chain.NewEVM(), chain.NewSolana(), chain.NewTON()} {
		p := a.Protocol()
		if got, want := a.Finality(), finality.Defaults()[p]; got != want {
			t.Fatalf("%s finality = %+v, want the finality.Defaults entry %+v", p, got, want)
		}
		if got, want := a.BatchLimits(), bulk.Defaults()[p]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s limits = %+v, want the bulk.Defaults entry %+v", p, got, want)
		}
	}
}

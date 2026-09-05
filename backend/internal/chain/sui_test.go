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

// TestSUI_NeedsNoSlot 釘住 sui 那一題的答案跟 solana 是同一種：交易指名的是 object 的版本，
// 送出當下從鏈上讀，沒有帳戶層級的序號可以發，所以取號成功、位置是空的，補號回 ErrNoGap。
func TestSUI_NeedsNoSlot(t *testing.T) {
	ctx := context.Background()
	seq := chain.NewSUI().Sequencer()
	res, err := seq.Reserve(ctx, "any-sui-address")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Ordered {
		t.Fatalf("reservation = %+v, want no slot", res)
	}
	if _, err := seq.ReserveGap(ctx, "any-sui-address"); !errors.Is(err, txseq.ErrNoGap) {
		t.Fatalf("ReserveGap = %v, want ErrNoGap", err)
	}
}

// TestSUI_CannotReplace 釘住 sui 不答替換那一題：同一顆 owned object 的同一個版本被兩筆交易用到
// 就是 equivocation，object 鎖到 epoch 結束。所以它跟 solana、ton 一樣連 Replacer 都不實作，
// 呼叫端拿到的是乾脆的 false。
func TestSUI_CannotReplace(t *testing.T) {
	if _, ok := chain.Replacement(chain.NewSUI()); ok {
		t.Fatalf("sui must not answer the replacement question")
	}
}

// TestSUI_AnswersWithTheRulesThatAlreadyExist 釘住「adapter 只當索引」在第四條鏈上照舊成立：
// 不可逆規則是 finality.Defaults() 裡 sui 那一條本人，上限是 bulk.Defaults() 裡 sui 那一條本人，
// 一個數字都不自己抄。
func TestSUI_AnswersWithTheRulesThatAlreadyExist(t *testing.T) {
	a := chain.NewSUI()
	if a.Protocol() != "sui" {
		t.Fatalf("protocol = %q, want sui", a.Protocol())
	}
	if got, want := a.Finality(), finality.Defaults()["sui"]; got != want {
		t.Fatalf("sui finality = %+v, want the finality.Defaults entry %+v", got, want)
	}
	if got, want := a.BatchLimits(), bulk.Defaults()["sui"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("sui limits = %+v, want the bulk.Defaults entry %+v", got, want)
	}
	if got := a.Finality().Marker; got != "checkpoint" {
		t.Fatalf("sui finality marker = %q, want checkpoint", got)
	}
}

// TestSUI_RegistersIntoTheDefaultRegistry 釘住四條鏈到齊：Default() 認得 sui:mainnet，
// 而且拿到的就是 SUI adapter；四個協定名照字母排。
func TestSUI_RegistersIntoTheDefaultRegistry(t *testing.T) {
	reg := chain.Default()
	a, err := reg.For("sui:mainnet")
	if err != nil {
		t.Fatalf("For(sui:mainnet): %v", err)
	}
	if _, ok := a.(*chain.SUI); !ok {
		t.Fatalf("For(sui:mainnet) = %T, want *chain.SUI", a)
	}
	if got, want := reg.Protocols(), []string{"evm", "solana", "sui", "ton"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Protocols() = %v, want %v", got, want)
	}
}

// TestSUI_BytesIsTheCapThatBindsOnAPTB 釘住三條規則裡先撞到的是 bytes：一個 PTB 名義上裝得下
// 1,024 個 command，但每一筆付款要 190 bytes，128 KB 在 688 筆就到頂，比 1,023 早得多。
// 有人把 bytes 那條規則拿掉、或把 Item 算小了，這條測試會先於鏈上的拒絕發現。
func TestSUI_BytesIsTheCapThatBindsOnAPTB(t *testing.T) {
	l := chain.NewSUI().BatchLimits()
	if got := l.MaxItems(); got != 688 {
		t.Fatalf("sui MaxItems = %d, want 688 from the bytes rule", got)
	}
	var byUnit = map[string]int{}
	for _, r := range l.Rules {
		byUnit[r.Unit] = int((r.Cap - r.Base) / r.Item)
	}
	if byUnit["commands"] != 1023 || byUnit["objects"] != 1024 || byUnit["bytes"] != 688 {
		t.Fatalf("per-rule room = %v, want commands 1023, objects 1024, bytes 688", byUnit)
	}
}

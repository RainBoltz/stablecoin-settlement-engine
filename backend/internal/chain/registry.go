package chain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	// ErrUnknownChain：這條鏈沒有 adapter。不給預設值：「不認識的鏈」跟「還沒接的鏈」在這裡是
	// 同一件事，猜一個預設 adapter 等於替一條不認識的鏈發交易。
	ErrUnknownChain = errors.New("chain: no adapter registered for this protocol")
	// ErrDuplicateAdapter：這個協定已經有 adapter 了。一個協定一個代表，理由很實際：
	// sequencer 的狀態只能有一份，第二個 adapter 就是第二條互相撞號的發號線。
	ErrDuplicateAdapter = errors.New("chain: an adapter for this protocol is already registered")
	// ErrIncompleteAdapter：這個 adapter 有問題答不出來，或答案自相矛盾。錯誤訊息會說是哪一題。
	ErrIncompleteAdapter = errors.New("chain: the adapter does not answer every question")
)

// Registry 是協定名對 adapter 的對照表。它做兩件事：註冊的時候把答不齊的 adapter 擋下來，
// 查詢的時候把 "evm:31337" 這種完整的鏈名切出協定、對回 adapter。
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry 建一個 Registry 並依序註冊。任何一個 adapter 被拒絕，整個建構就失敗：
// 一個接了一半的系統比一個接不起來的系統難查得多。
func NewRegistry(adapters ...Adapter) (*Registry, error) {
	r := &Registry{adapters: make(map[string]Adapter)}
	for _, a := range adapters {
		if err := r.Register(a); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register 註冊一個 adapter，答不齊全部問題的直接拒絕。
//
// 拒絕得這麼早是刻意的：ErrNoPolicy、ErrNoRules 這一類錯誤要等某筆付款真的走到那一步才出現，
// 而那可能是半夜；Register 把同一類錯誤搬到接線的那一刻，炸在部署的人面前，不炸在值班的人面前。
func (r *Registry) Register(a Adapter) error {
	if err := vet(a); err != nil {
		return err
	}
	p := a.Protocol()
	if _, ok := r.adapters[p]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateAdapter, p)
	}
	r.adapters[p] = a
	return nil
}

// For 把一筆 intent 的 Chain（"evm:31337"）對回它的 adapter。
//
// 冒號前面是協定、後面是網路，跟 CAIP-2（https://chainagnostic.org/CAIPs/caip-2）的
// namespace:reference 同一個形狀；同一個協定的每個網路共用同一個 adapter，
// 跟 listener 對不可逆規則的用法一致。查不到就回 ErrUnknownChain，錯誤訊息帶著協定名。
func (r *Registry) For(id string) (Adapter, error) {
	p, _, _ := strings.Cut(id, ":")
	a, ok := r.adapters[p]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownChain, p)
	}
	return a, nil
}

// Protocols 回報已註冊的協定名，排序過，給報告與錯誤訊息用。
func (r *Registry) Protocols() []string {
	ps := make([]string, 0, len(r.adapters))
	for p := range r.adapters {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

// Default 回一個註冊了目前有實作的三條鏈的 Registry。SUI 之後再補。
//
// 這裡的 panic 走不到：三個 adapter 的完整性都有測試釘住，會炸的唯一情況是有人改壞了
// 其中一個 adapter 又沒跑測試，而那正是應該炸的情況。
func Default() *Registry {
	r, err := NewRegistry(NewEVM(), NewSolana(), NewTON())
	if err != nil {
		panic(err)
	}
	return r
}

// vet 逐題檢查一個 adapter。每一條都對應一種「接了一半」的樣子，錯誤訊息要說得出是哪一題、
// 為什麼不行；訊息是給接鏈的人看的，所以寫英文、帶協定名。
func vet(a Adapter) error {
	p := a.Protocol()
	if p == "" {
		return fmt.Errorf("%w: %T has no protocol name", ErrIncompleteAdapter, a)
	}
	if strings.ContainsRune(p, ':') {
		return fmt.Errorf("%w: protocol %q carries a network suffix; networks share one adapter", ErrIncompleteAdapter, p)
	}
	if p != strings.ToLower(p) {
		return fmt.Errorf("%w: protocol %q is not lower-case, and For does no case folding", ErrIncompleteAdapter, p)
	}
	if a.Sequencer() == nil {
		return fmt.Errorf("%w: %s has no sequencer; a chain that needs no slots answers with txseq.Unordered", ErrIncompleteAdapter, p)
	}
	f := a.Finality()
	if !f.RequireMarker && f.Confirmations == 0 {
		return fmt.Errorf("%w: %s has a finality policy that would accept any included transaction", ErrIncompleteAdapter, p)
	}
	if f.RequireMarker && f.Marker == "" {
		return fmt.Errorf("%w: %s waits for a finality marker but does not name it", ErrIncompleteAdapter, p)
	}
	l := a.BatchLimits()
	if l.Chain != p {
		return fmt.Errorf("%w: %s hands out batch limits that describe %q", ErrIncompleteAdapter, p, l.Chain)
	}
	if len(l.Rules) == 0 {
		return fmt.Errorf("%w: %s has no batch limit rules", ErrIncompleteAdapter, p)
	}
	if l.RentUnit == "" {
		return fmt.Errorf("%w: %s names no rent unit", ErrIncompleteAdapter, p)
	}
	for _, rule := range l.Rules {
		if rule.Unit == "" || rule.Cap == 0 || rule.Item == 0 {
			return fmt.Errorf("%w: %s has a batch rule with a zero unit, cap or item", ErrIncompleteAdapter, p)
		}
		if rule.Base+rule.Item > rule.Cap {
			return fmt.Errorf("%w: the %s rule of %s has no room for a single payout", ErrIncompleteAdapter, rule.Unit, p)
		}
		if !strings.HasPrefix(rule.Source, "https://") {
			return fmt.Errorf("%w: the %s rule of %s has no public source", ErrIncompleteAdapter, rule.Unit, p)
		}
	}
	return nil
}

package ledger

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustAppend(t *testing.T, j Journal, e Entry) Entry {
	t.Helper()
	stored, applied, err := j.Append(context.Background(), e)
	if err != nil {
		t.Fatalf("append %s: %v", e.ID, err)
	}
	if !applied {
		t.Fatalf("append %s: not applied", e.ID)
	}
	return stored
}

func balance(t *testing.T, j Journal, acct Account, asset Asset) (pending, posted int64) {
	t.Helper()
	b, err := j.Balance(context.Background(), acct, asset)
	if err != nil {
		t.Fatal(err)
	}
	return b.Pending.Int64(), b.Posted.Int64()
}

// TestJournal_AppendIsIdempotent：同 ID 同內容是重放，什麼都不動、Seq 不增；同 ID 不同內容是 ErrConflict。
// listener 與 queue 都是 at-least-once，同一個 post 送兩次是日常，第二次要能安靜地過；
// 但同一個 ID 帶著不同的金額進來，那是 bug，不能默默吞掉其中一個。
func TestJournal_AppendIsIdempotent(t *testing.T) {
	ctx := context.Background()
	j := NewMemoryJournal()
	first := mustAppend(t, j, hold("pi_0001", refA, assetUSDC, 100))

	again, applied, err := j.Append(ctx, hold("pi_0001", refA, assetUSDC, 100))
	if err != nil || applied || again.Seq != first.Seq || again.Hash != first.Hash {
		t.Fatalf("replay: applied=%v err=%v seq=%d", applied, err, again.Seq)
	}
	_, _, err = j.Append(ctx, hold("pi_0001", refA, assetUSDC, 101))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("same id different amount: want ErrConflict, got %v", err)
	}
	n := 0
	_ = j.Scan(ctx, func(Entry) error { n++; return nil })
	if n != 1 {
		t.Fatalf("journal should still have one entry, got %d", n)
	}
}

// TestJournal_HoldResolvesExactlyOnce：一筆 hold 只能被 post 或 void 收尾一次。post 之後再 void、void 之後再 post、
// post 兩次（不同 ID），全部 ErrAlreadyResolved。這是「錢只動一次」在帳本上的長相。
func TestJournal_HoldResolvesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	j := NewMemoryJournal()
	mustAppend(t, j, hold("pi_0001", refA, assetUSDC, 100))
	mustAppend(t, j, post("pi_0001", refA, assetUSDC, twoLegs(100)))

	if _, _, err := j.Append(ctx, void("pi_0001", refA, assetUSDC)); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("void after post: want ErrAlreadyResolved, got %v", err)
	}
	second := post("pi_0001", refA, assetUSDC, twoLegs(100))
	second.ID = "pi_0001/post-again"
	if _, _, err := j.Append(ctx, second); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second post: want ErrAlreadyResolved, got %v", err)
	}

	mustAppend(t, j, hold("pi_0002", refB, assetUSDT, 100))
	mustAppend(t, j, void("pi_0002", refB, assetUSDT))
	if _, _, err := j.Append(ctx, post("pi_0002", refB, assetUSDT, twoLegs(100))); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("post after void: want ErrAlreadyResolved, got %v", err)
	}
}

// TestJournal_PostNeedsAMatchingHold：post / void 指的 hold 要存在、要真的是 hold、ref 與 asset 都要對得上。
// 拿 pi_0001 的 hold 去收 pi_0002 的帳、或用 USDT 的 post 去收 USDC 的 hold，都是 ErrNoSuchHold。
func TestJournal_PostNeedsAMatchingHold(t *testing.T) {
	ctx := context.Background()
	j := NewMemoryJournal()
	mustAppend(t, j, hold("pi_0001", refA, assetUSDC, 100))
	mustAppend(t, j, post("pi_0001", refA, assetUSDC, twoLegs(100)))

	cases := map[string]Entry{
		"hold does not exist": post("pi_0009", refA, assetUSDC, twoLegs(100)),
		"points at a post":    {ID: "x", Ref: refA, Kind: KindVoid, Holds: "pi_0001/post", Asset: assetUSDC, By: "relayer", At: t0},
		"other ref":           {ID: "y", Ref: refB, Kind: KindVoid, Holds: "pi_0001/hold", Asset: assetUSDC, By: "relayer", At: t0},
		"other asset":         {ID: "z", Ref: refA, Kind: KindVoid, Holds: "pi_0001/hold", Asset: assetUSDT, By: "relayer", At: t0},
	}
	for name, e := range cases {
		if _, _, err := j.Append(ctx, e); !errors.Is(err, ErrNoSuchHold) {
			t.Errorf("%s: want ErrNoSuchHold, got %v", name, err)
		}
	}
}

// TestJournal_PendingThenPosted：hold 進 pending、post 把 pending 搬到 posted、void 只把 pending 放掉。
// post 的腿可以跟 hold 不一樣：請款 100、實收 99.9，差的 0.1 落在 fee 科目，merchant 的 posted 就是 99.9，
// 這就是 Day 2「請款金額」與「實收金額」分開的實作。
func TestJournal_PendingThenPosted(t *testing.T) {
	j := NewMemoryJournal()
	m, p, f := MerchantAccount(merchant), PayerAccount(payer), FeeAccount(usdt)

	mustAppend(t, j, hold("pi_0002", refB, assetUSDT, 100_000_000))
	if pending, posted := balance(t, j, m, assetUSDT); pending != 100_000_000 || posted != 0 {
		t.Fatalf("after hold: merchant pending=%d posted=%d", pending, posted)
	}
	if pending, _ := balance(t, j, p, assetUSDT); pending != -100_000_000 {
		t.Fatalf("after hold: payer pending=%d", pending)
	}

	mustAppend(t, j, post("pi_0002", refB, assetUSDT, []Leg{
		{p, big.NewInt(-100_000_000)}, {m, big.NewInt(99_900_000)}, {f, big.NewInt(100_000)},
	}))
	if pending, posted := balance(t, j, m, assetUSDT); pending != 0 || posted != 99_900_000 {
		t.Fatalf("after post: merchant pending=%d posted=%d", pending, posted)
	}
	if pending, posted := balance(t, j, f, assetUSDT); pending != 0 || posted != 100_000 {
		t.Fatalf("after post: fee pending=%d posted=%d", pending, posted)
	}
	if pending, posted := balance(t, j, p, assetUSDT); pending != 0 || posted != -100_000_000 {
		t.Fatalf("after post: payer pending=%d posted=%d", pending, posted)
	}

	mustAppend(t, j, hold("pi_0003", refA, assetUSDC, 50))
	mustAppend(t, j, void("pi_0003", refA, assetUSDC))
	if pending, posted := balance(t, j, m, assetUSDC); pending != 0 || posted != 0 {
		t.Fatalf("after void: merchant USDC pending=%d posted=%d", pending, posted)
	}
	// USDT 的帳不受 USDC 那筆影響：asset 之間不互抵。
	if _, posted := balance(t, j, m, assetUSDT); posted != 99_900_000 {
		t.Fatalf("USDC void leaked into USDT: posted=%d", posted)
	}
	// 從沒出現過的科目是兩個零，不是錯誤。
	if pending, posted := balance(t, j, Account("nobody:0x0"), assetUSDC); pending != 0 || posted != 0 {
		t.Fatalf("unknown account: pending=%d posted=%d", pending, posted)
	}
}

// TestJournal_BalanceIsAProjection：Journal.Balance 的結果跟「拿 Scan 匯出的 entries 自己 fold 一遍」逐一相等。
// 餘額不是存起來的欄位，任何人拿 journal 都能重算出同一份。
func TestJournal_BalanceIsAProjection(t *testing.T) {
	ctx := context.Background()
	j := NewMemoryJournal()
	mustAppend(t, j, hold("pi_0001", refA, assetUSDC, 100))
	mustAppend(t, j, hold("pi_0002", refB, assetUSDT, 100))
	mustAppend(t, j, post("pi_0001", refA, assetUSDC, twoLegs(100)))
	mustAppend(t, j, post("pi_0002", refB, assetUSDT, []Leg{
		{PayerAccount(payer), big.NewInt(-100)}, {MerchantAccount(merchant), big.NewInt(97)}, {FeeAccount(usdt), big.NewInt(3)},
	}))
	mustAppend(t, j, hold("pi_0003", refA, assetUSDC, 7))

	var exported []Entry
	_ = j.Scan(ctx, func(e Entry) error { exported = append(exported, e); return nil })
	folded := Balances(exported)
	if len(folded) == 0 {
		t.Fatal("projection is empty")
	}
	for k, want := range folded {
		got, err := j.Balance(ctx, k.Account, k.Asset)
		if err != nil || got.Pending.Cmp(want.Pending) != 0 || got.Posted.Cmp(want.Posted) != 0 {
			t.Errorf("%s %s: journal says %s, projection says %s (err %v)", k.Account, k.Asset, got, want, err)
		}
	}
	// 每一種 asset 的所有科目加總，pending 與 posted 各自都是零：這是複式記帳給整本帳的不變量。
	for _, asset := range []Asset{assetUSDC, assetUSDT} {
		sumP, sumQ := new(big.Int), new(big.Int)
		for k, b := range folded {
			if k.Asset == asset {
				sumP.Add(sumP, b.Pending)
				sumQ.Add(sumQ, b.Posted)
			}
		}
		if sumP.Sign() != 0 || sumQ.Sign() != 0 {
			t.Errorf("%s: whole-ledger sum pending=%s posted=%s, want 0 0", asset, sumP, sumQ)
		}
	}
}

// TestJournal_ByRefKeepsOrder：一筆付款的所有列照 Seq 回來，跟別的 ref 不混。
func TestJournal_ByRefKeepsOrder(t *testing.T) {
	ctx := context.Background()
	j := NewMemoryJournal()
	mustAppend(t, j, hold("pi_0001", refA, assetUSDC, 100))
	mustAppend(t, j, hold("pi_0002", refB, assetUSDT, 100))
	mustAppend(t, j, post("pi_0001", refA, assetUSDC, twoLegs(100)))

	got, err := j.ByRef(ctx, refA)
	if err != nil || len(got) != 2 || got[0].Kind != KindHold || got[1].Kind != KindPost || got[0].Seq != 1 || got[1].Seq != 3 {
		t.Fatalf("by ref: %v %+v", err, got)
	}
	if e, err := j.Get(ctx, "pi_0002/hold"); err != nil || e.Seq != 2 {
		t.Fatalf("get: %v %+v", err, e)
	}
	if _, err := j.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: want ErrNotFound, got %v", err)
	}
	// 拿到的是拷貝，改它不會動到存的那份。
	got[0].Legs[0].Amount.SetInt64(1)
	again, _ := j.Get(ctx, "pi_0001/hold")
	if again.Legs[0].Amount.Int64() != -100 {
		t.Fatalf("journal leaked its internal slice: %+v", again.Legs)
	}
}

// pinnedHead 是下面那三列 append 完之後最後一列的 Hash，寫死當 golden：任何人動到 DomainV1、欄位順序、編碼，
// 這條測試會先叫。改編碼就是改所有已經匯出去、給稽核留底的 hash 的意義，必須是刻意的、有 diff 的。
const pinnedHead = "4458a9453291440e0104ff8029f885cb74022369caee1fb55c7187a03bf6e968"

// TestJournal_HashChainIsPinned：固定的三列算出固定的鏈頭；順序換了、任何一個欄位改了都不是這個值。
func TestJournal_HashChainIsPinned(t *testing.T) {
	j := NewMemoryJournal()
	mustAppend(t, j, hold("pi_0001", refA, assetUSDC, 100_000_000))
	mustAppend(t, j, hold("pi_0002", refB, assetUSDT, 100_000_000))
	last := mustAppend(t, j, post("pi_0001", refA, assetUSDC, twoLegs(100_000_000)))
	if got := hex.EncodeToString(last.Hash[:]); got != pinnedHead {
		t.Fatalf("chain head changed:\n got %s\nwant %s", got, pinnedHead)
	}
	if last.Seq != 3 || last.PrevHash == [32]byte{} {
		t.Fatalf("seq=%d prev=%x", last.Seq, last.PrevHash)
	}
	if err := Verify(context.Background(), j); err != nil {
		t.Fatalf("fresh journal should verify: %v", err)
	}
}

// TestJournal_TamperingIsDetected：直接改存起來的某一列（金額、時間、把兩列對調），Verify 回 ErrChainBroken，
// 而且指出的是第一個壞掉的 Seq。這是「只加不改」能被檢查的那一半：有寫入權的人還是改得動，但改完藏不住。
func TestJournal_TamperingIsDetected(t *testing.T) {
	ctx := context.Background()
	build := func() *MemoryJournal {
		j := NewMemoryJournal()
		mustAppend(t, j, hold("pi_0001", refA, assetUSDC, 100))
		mustAppend(t, j, hold("pi_0002", refB, assetUSDT, 100))
		mustAppend(t, j, post("pi_0001", refA, assetUSDC, twoLegs(100)))
		mustAppend(t, j, void("pi_0002", refB, assetUSDT))
		return j
	}
	tampers := map[string]struct {
		do      func(j *MemoryJournal)
		wantSeq string
	}{
		"amount on seq 2": {func(j *MemoryJournal) { j.entries[1].Legs[1].Amount.SetInt64(101) }, "seq 2"},
		"time on seq 3":   {func(j *MemoryJournal) { j.entries[2].At = j.entries[2].At.Add(time.Second) }, "seq 3"},
		"memo on seq 4":   {func(j *MemoryJournal) { j.entries[3].Memo = "looked fine to me" }, "seq 4"},
		"delete seq 2":    {func(j *MemoryJournal) { j.entries = append(j.entries[:1], j.entries[2:]...) }, "seq 3"},
		"swap seq 1 and 2": {func(j *MemoryJournal) {
			j.entries[0], j.entries[1] = j.entries[1], j.entries[0]
		}, "seq 2"},
	}
	for name, tp := range tampers {
		j := build()
		if err := Verify(ctx, j); err != nil {
			t.Fatalf("%s: baseline should verify: %v", name, err)
		}
		tp.do(j)
		err := Verify(ctx, j)
		if !errors.Is(err, ErrChainBroken) {
			t.Errorf("%s: want ErrChainBroken, got %v", name, err)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, tp.wantSeq) {
			t.Errorf("%s: should point at %s, got %q", name, tp.wantSeq, msg)
		}
	}
}

// TestJournal_ConcurrentAppends：五十個 goroutine 同時 append 不同的 hold，Seq 密集不重複、鏈完整、每一筆都在。
// Append 裡的鎖是唯一的原子點，跟 intent 的 CAS、idempotency 的 Claim 一樣，同時進來的只有一個拿得到下一個 Seq。
func TestJournal_ConcurrentAppends(t *testing.T) {
	ctx := context.Background()
	j := NewMemoryJournal()
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := hold(fmt.Sprintf("pi_%04d", i), refA, assetUSDC, int64(i+1))
			if _, _, err := j.Append(ctx, e); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if err := Verify(ctx, j); err != nil {
		t.Fatalf("verify: %v", err)
	}
	seen := make(map[uint64]bool)
	_ = j.Scan(ctx, func(e Entry) error { seen[e.Seq] = true; return nil })
	if len(seen) != n {
		t.Fatalf("want %d distinct seqs, got %d", n, len(seen))
	}
	for s := uint64(1); s <= n; s++ {
		if !seen[s] {
			t.Fatalf("seq %d missing", s)
		}
	}
}

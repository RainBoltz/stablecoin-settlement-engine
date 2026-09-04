package boc_test

import (
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/boc"
)

// 這個檔案的 golden 全部是用 @ton/core 0.60.1（TON 官方生態的 TypeScript 實作）算出來、貼過來的：
// 同一棵樹兩邊要算出同一個雜湊、序列化出同一段 bytes，錢包合約驗簽名時認的才會是我們簽的那個雜湊。
// 跑出來不一樣是這裡的程式碼錯了，不是 golden 錯了。

func mustBuild(t *testing.T, b *boc.Builder) *boc.Cell {
	t.Helper()
	c, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return c
}

func hexHash(c *boc.Cell) string {
	h := c.Hash()
	return hex.EncodeToString(h[:])
}

// 防的情境：雜湊的 representation 少算了描述 byte，或 BoC 的表頭排錯位。
// 最小的 cell（32 個 bit、沒有 ref）兩邊都要對得上。
func TestCell_GoldenHashAndBoCOfATinyCell(t *testing.T) {
	c := mustBuild(t, boc.NewBuilder().Uint(0xf8a7ea5, 32))
	if got := hexHash(c); got != "1204352a90cd2724c2313d6537d71d7975bbd9b9295600042cfa134bd4d45326" {
		t.Fatalf("hash = %s", got)
	}
	if got := hex.EncodeToString(c.ToBoC()); got != "b5ee9c724101010100060000080f8a7ea5f327cac0" {
		t.Fatalf("boc = %s", got)
	}
}

// 防的情境：不滿一個 byte 的資料忘了補 completion tag、深度算錯、或同一個 cell 被序列化兩次。
// 這棵樹三層深、一片葉子被兩個地方指到、根的 bit 數不是 8 的倍數，三件事一次釘住。
func TestCell_GoldenTreeWithSharedRefAndOddBits(t *testing.T) {
	leaf := mustBuild(t, boc.NewBuilder().Uint(5, 3))
	mid := mustBuild(t, boc.NewBuilder().Uint(0xabcd, 16).Ref(leaf).Ref(leaf))
	addr, err := boc.ParseAddress("EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N")
	if err != nil {
		t.Fatalf("ParseAddress: %v", err)
	}
	root := mustBuild(t, boc.NewBuilder().Bit(true).Coins(big.NewInt(123456789)).Address(addr).Ref(mid).Ref(leaf))

	if got := hexHash(leaf); got != "c8235418b5cd55bc46073ea5cf9f3aac5a594ed782bee88dcd0acfd8ede4c756" {
		t.Fatalf("leaf hash = %s", got)
	}
	if got := hexHash(mid); got != "e54915e107f2a4b7e5d4a4b251e0375c7fb971af727449a88c6deb13039faa01" {
		t.Fatalf("mid hash = %s", got)
	}
	if got := hexHash(root); got != "e7caf8bed62fd6f24bb4134802592de658283f44e46088cb62f147f5dfe43efe" {
		t.Fatalf("root hash = %s", got)
	}
	if leaf.Depth() != 0 || mid.Depth() != 1 || root.Depth() != 2 {
		t.Fatalf("depths = %d %d %d, want 0 1 2", leaf.Depth(), mid.Depth(), root.Depth())
	}
	want := "b5ee9c7241010301003300024ca03ade68ac0083dfd552e63729b472fcbcc8c45ebcc6691702558b68ec7527e1ba403a0f31a801020204abcd02020001b0aedf2fc8"
	if got := hex.EncodeToString(root.ToBoC()); got != want {
		t.Fatalf("boc = %s", got)
	}
	if st := root.Count(); st.Cells != 3 || st.Depth != 2 {
		t.Fatalf("Count = %+v, want 3 cells 2 deep (the shared leaf counts once)", st)
	}
}

// 防的情境：VarUInteger 16 的長度算錯。零是 4 個 bit，256 要兩個 byte，上限是 15 個 byte。
func TestBuilder_CoinsUseTheShortestLength(t *testing.T) {
	cases := []struct {
		v    string
		bits int
		hash string
	}{
		{"0", 4, "5331fed036518120c7f345726537745c5929b8ea1fa37b99b2bb58f702671541"},
		{"1", 12, "d46edee086ccbace01f45c13d26d49b68f74cd1b7616f4662e699c82c6ec728b"},
		{"255", 12, "bd16b2d60c93163fbed832e91a5faec484715c48176857c57dcedf9f6e0f32f6"},
		{"256", 20, "16559011ce6f0f7aaa765179e73ef293f39610f5baa3838a1dc8c52da95793b3"},
		{"50000000", 36, "b709317bf2bc6d5c09257ced07d1e45a7a379aba1c056b412f100d1401015749"},
		{"1329227995784915872903807060280344575", 124, "07d470f83cea8b41383aab0113b84f4be3842bc6ec0c46d84664a647d5550dc9"},
	}
	for _, tc := range cases {
		v, _ := new(big.Int).SetString(tc.v, 10)
		c := mustBuild(t, boc.NewBuilder().Coins(v))
		if c.Bits() != tc.bits || hexHash(c) != tc.hash {
			t.Fatalf("coins %s: %d bits %s", tc.v, c.Bits(), hexHash(c))
		}
		if back := c.Begin().Coins(); back.Cmp(v) != 0 {
			t.Fatalf("coins %s read back as %s", tc.v, back)
		}
	}
	too := new(big.Int).Lsh(big.NewInt(1), 120)
	if _, err := boc.NewBuilder().Coins(too).Build(); !errors.Is(err, boc.ErrCoinsTooLarge) {
		t.Fatalf("2^120 should not fit VarUInteger 16, got %v", err)
	}
}

// 防的情境：addr_none 與負的 workchain 編錯。message 表頭的 src 就是 addr_none，masterchain 是 -1。
func TestBuilder_AddrNoneAndNegativeInt(t *testing.T) {
	c := mustBuild(t, boc.NewBuilder().Address(boc.Address{}).Int(-1, 8).Uint(0, 1))
	if c.Bits() != 11 || hexHash(c) != "9dd0a0378dfa6a3f7bc506e777f556bfc84c0243432e9f7c04acdeb3f119935d" {
		t.Fatalf("cell = %d bits %s", c.Bits(), hexHash(c))
	}
	if got := hex.EncodeToString(c.ToBoC()); got != "b5ee9c724101010100040000033fd06f0a53c5" {
		t.Fatalf("boc = %s", got)
	}
	s := c.Begin()
	if a := s.Address(); !a.IsZero() {
		t.Fatalf("read back %v, want addr_none", a)
	}
	if wc := s.Int(8); wc != -1 {
		t.Fatalf("workchain read back as %d", wc)
	}
}

// 防的情境：Builder 塞過頭卻沒有人擋。1,024 個 bit 與第 5 個 ref 都要被拒絕，而且是 Build 的時候
// 一次回報，不是中途 panic。
func TestBuilder_RefusesMoreThanACellHolds(t *testing.T) {
	b := boc.NewBuilder()
	for i := 0; i < 1024; i++ {
		b.Bit(true)
	}
	if _, err := b.Build(); !errors.Is(err, boc.ErrTooManyBits) {
		t.Fatalf("1024 bits: %v, want ErrTooManyBits", err)
	}
	leaf := mustBuild(t, boc.NewBuilder())
	b = boc.NewBuilder()
	for i := 0; i < 5; i++ {
		b.Ref(leaf)
	}
	if _, err := b.Build(); !errors.Is(err, boc.ErrTooManyRefs) {
		t.Fatalf("5 refs: %v, want ErrTooManyRefs", err)
	}
	if c := mustBuild(t, boc.NewBuilder().Uint(0, 1023)); c.Bits() != 1023 {
		t.Fatalf("1023 bits should fit, got %d", c.Bits())
	}
}

// 防的情境：Slice 讀過頭卻回一個看起來正常的零值。錯誤黏住、最後 Err 一定要看得到。
func TestSlice_ReadingPastTheEndIsAnError(t *testing.T) {
	c := mustBuild(t, boc.NewBuilder().Uint(0xab, 8))
	s := c.Begin()
	if v := s.Uint(8); v != 0xab || s.Err() != nil {
		t.Fatalf("first byte = %x, err %v", v, s.Err())
	}
	if s.Remaining() != 0 {
		t.Fatalf("Remaining = %d, want 0", s.Remaining())
	}
	_ = s.Uint(1)
	if !errors.Is(s.Err(), boc.ErrShortRead) {
		t.Fatalf("reading past the end: %v, want ErrShortRead", s.Err())
	}
	if s.Ref() != nil || !errors.Is(s.Err(), boc.ErrShortRead) {
		t.Fatalf("Ref on a cell with no refs should be ErrShortRead")
	}
}

// 防的情境：兩種寫法的地址對不上同一個帳戶。raw、bounceable、non-bounceable 三個字串是同一個地址；
// CRC 壞掉的要拒收，因為地址少一個字元就是把錢付給別人。
func TestAddress_ThreeSpellingsOneAccount(t *testing.T) {
	raw := "0:83dfd552e63729b472fcbcc8c45ebcc6691702558b68ec7527e1ba403a0f31a8"
	for _, s := range []string{raw, "EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N", "UQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqEBI"} {
		a, err := boc.ParseAddress(s)
		if err != nil {
			t.Fatalf("ParseAddress(%s): %v", s, err)
		}
		if a.String() != raw {
			t.Fatalf("ParseAddress(%s) = %s", s, a)
		}
	}
	a, _ := boc.ParseAddress(raw)
	if got := a.Friendly(true); got != "EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N" {
		t.Fatalf("Friendly(bounceable) = %s", got)
	}
	if got := a.Friendly(false); got != "UQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqEBI" {
		t.Fatalf("Friendly(non-bounceable) = %s", got)
	}
	mc, err := boc.ParseAddress("Ef8zMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzM0vF")
	if err != nil || mc.String() != "-1:3333333333333333333333333333333333333333333333333333333333333333" {
		t.Fatalf("masterchain address = %v, %v", mc, err)
	}
	for _, bad := range []string{"", "0:abcd", "EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2M", "notanaddress"} {
		if _, err := boc.ParseAddress(bad); !errors.Is(err, boc.ErrBadAddress) {
			t.Fatalf("ParseAddress(%q) = %v, want ErrBadAddress", bad, err)
		}
	}
}

// 防的情境：組進去的東西讀不回來。整數、金額、地址、ref 走一趟 Builder 再走一趟 Slice，
// 每個欄位都要原樣回來。
func TestSlice_ReadsBackWhatTheBuilderWrote(t *testing.T) {
	addr, _ := boc.ParseAddress("EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N")
	inner := mustBuild(t, boc.NewBuilder().Uint(7, 3))
	c := mustBuild(t, boc.NewBuilder().
		Uint(0x7362d09c, 32).Int(-239, 32).Coins(big.NewInt(1)).Address(addr).
		Bytes([]byte{1, 2, 3}).MaybeRef(nil).MaybeRef(inner).Ref(inner))
	s := c.Begin()
	if op := s.Uint(32); op != 0x7362d09c {
		t.Fatalf("op = %x", op)
	}
	if id := s.Int(32); id != -239 {
		t.Fatalf("int32 = %d", id)
	}
	if v := s.Coins(); v.Int64() != 1 {
		t.Fatalf("coins = %s", v)
	}
	if a := s.Address(); a != addr {
		t.Fatalf("address = %v", a)
	}
	if p := s.Bytes(3); p[0] != 1 || p[1] != 2 || p[2] != 3 {
		t.Fatalf("bytes = %v", p)
	}
	if r := s.MaybeRef(); r != nil {
		t.Fatalf("first MaybeRef should be nil")
	}
	if r := s.MaybeRef(); r == nil || r.Hash() != inner.Hash() {
		t.Fatalf("second MaybeRef should be the inner cell")
	}
	if r := s.Ref(); r == nil || r.Hash() != inner.Hash() {
		t.Fatalf("Ref should be the inner cell")
	}
	if s.Remaining() != 0 || s.RemainingRefs() != 0 || s.Err() != nil {
		t.Fatalf("leftover: %d bits %d refs, err %v", s.Remaining(), s.RemainingRefs(), s.Err())
	}
}

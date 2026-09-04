// Package boc 是 TON 的資料單位：cell，以及把一棵 cell 樹序列化成 bag of cells（BoC）的最小實作。
//
// TON 上沒有「一段 bytes」這種東西：message、合約狀態、交易紀錄全部是 cell 組成的樹。一個 cell 最多裝
// 1,023 個 bit 與 4 個指向別的 cell 的 ref，超過就得把內容切到下一個 cell、用 ref 接起來。
// 這個 package 只做鏈下組 message 需要的那一小部分：一個 Builder 把 bit、整數、金額、地址、ref 塞進
// 一個 cell；一個 Slice 反過來把它們讀出來；Cell.Hash 算出 TON 對一個 cell 的標準雜湊
// （錢包合約簽的就是這個雜湊）；Cell.ToBoC 把整棵樹序列化成送給節點的 bytes。
//
// 只支援 ordinary cell：沒有 exotic cell、沒有 level、沒有 merkle proof。組一筆付款 message
// 用不到那些，而它們是這個格式最容易做錯的部分。
//
// 雜湊與序列化的格式照 TON 的 cell 規格：一個 cell 的 representation 是兩個描述 byte
// （ref 數、bit 數）、補完 completion tag 的資料、每個 ref 的深度（2 bytes）、每個 ref 的雜湊（32 bytes），
// 對它做 SHA-256 就是這個 cell 的雜湊；BoC 是 magic 加表頭加逐個 cell 的描述與 ref 索引，
// 尾端帶一個 CRC32-C。這兩件事都跟 @ton/core（TON 官方生態最常用的 TypeScript 實作）逐 byte 對過，
// 見測試裡的 golden。
//
// 本 package 為本系列從零設計，只取公開規格裡需要的那部分。
package boc

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math/big"
)

const (
	// MaxBits 是一個 cell 裝得下的 bit 數上限。
	MaxBits = 1023
	// MaxRefs 是一個 cell 能指向幾個別的 cell。
	MaxRefs = 4
)

var (
	// ErrTooManyBits：塞進一個 cell 的資料超過 1,023 bits。呼叫端要把內容切到 ref 裡，這裡不代切。
	ErrTooManyBits = errors.New("boc: a cell holds at most 1023 bits")
	// ErrTooManyRefs：一個 cell 指向超過 4 個 cell。
	ErrTooManyRefs = errors.New("boc: a cell holds at most 4 refs")
	// ErrShortRead：Slice 讀到的東西比要讀的短，或 ref 已經讀完。讀到一半發現不夠，代表這個 cell
	// 不是預期的形狀，呼叫端拿到的值不能信。
	ErrShortRead = errors.New("boc: the cell is shorter than what was read from it")
	// ErrCoinsTooLarge：金額塞不進 VarUInteger 16（最多 15 bytes，也就是 2^120 - 1）。
	ErrCoinsTooLarge = errors.New("boc: the amount does not fit VarUInteger 16")
	// ErrDepth：cell 樹深超過 BoC 的表示範圍（深度存 16 bits）。
	ErrDepth = errors.New("boc: the cell tree is deeper than 65535")
)

// Cell 是一個組好的 ordinary cell：不可變，雜湊在建立時算好。
type Cell struct {
	data  []byte
	bits  int
	refs  []*Cell
	hash  [32]byte
	depth int
}

// Bits 回報這個 cell 裝了幾個 bit。
func (c *Cell) Bits() int { return c.bits }

// Refs 回報這個 cell 指向幾個 cell。
func (c *Cell) Refs() int { return len(c.refs) }

// Ref 回報第 i 個 ref。
func (c *Cell) Ref(i int) *Cell { return c.refs[i] }

// Depth 是這個 cell 往下最長的一條 ref 鏈有幾層：沒有 ref 是 0。
func (c *Cell) Depth() int { return c.depth }

// Hash 是 TON 對這個 cell 的標準雜湊：對 representation 做 SHA-256。兩個 cell 的內容與 ref
// 完全相同就有同一個雜湊，所以它可以拿來當 cell 的身分；錢包合約驗簽名時簽的也是它。
func (c *Cell) Hash() [32]byte { return c.hash }

// Begin 開始讀這個 cell。
func (c *Cell) Begin() *Slice { return &Slice{c: c} }

// representation 是被雜湊的那段 bytes：兩個描述 byte、補完的資料、ref 的深度、ref 的雜湊。
func representation(data []byte, bits int, refs []*Cell) []byte {
	repr := make([]byte, 0, 2+len(data)+34*len(refs))
	repr = append(repr, byte(len(refs)), byte(bits/8+(bits+7)/8))
	repr = append(repr, padded(data, bits)...)
	for _, r := range refs {
		repr = binary.BigEndian.AppendUint16(repr, uint16(r.depth))
	}
	for _, r := range refs {
		repr = append(repr, r.hash[:]...)
	}
	return repr
}

// padded 把不滿一個 byte 的資料補上 completion tag：一個 1，後面補 0 到 byte 邊界。
// 剛好整數個 byte 的資料不補，描述 byte 的奇偶會告訴讀的人有沒有補。
func padded(data []byte, bits int) []byte {
	out := append([]byte(nil), data[:(bits+7)/8]...)
	if bits%8 != 0 {
		out[len(out)-1] |= 0x80 >> (bits % 8)
	}
	return out
}

// Builder 逐段把內容塞進一個 cell。任何一步超過上限都會記下錯誤，Build 的時候一次回報，
// 中間的呼叫不用逐個檢查。
type Builder struct {
	data []byte
	bits int
	refs []*Cell
	err  error
}

// NewBuilder 開始組一個 cell。
func NewBuilder() *Builder {
	return &Builder{data: make([]byte, (MaxBits+7)/8)}
}

// Bit 塞一個 bit。
func (b *Builder) Bit(v bool) *Builder {
	if b.err != nil {
		return b
	}
	if b.bits+1 > MaxBits {
		b.err = ErrTooManyBits
		return b
	}
	if v {
		b.data[b.bits/8] |= 0x80 >> (b.bits % 8)
	}
	b.bits++
	return b
}

// Uint 塞一個 n bit 的無號整數，高位在前。
func (b *Builder) Uint(v uint64, n int) *Builder {
	for i := n - 1; i >= 0; i-- {
		b.Bit(v>>uint(i)&1 == 1)
	}
	return b
}

// Int 塞一個 n bit 的有號整數（二補數），高位在前。workchain 是 int8、W5 的 wallet_id 是 int32。
func (b *Builder) Int(v int64, n int) *Builder {
	return b.Uint(uint64(v)&(1<<uint(n)-1), n)
}

// Bytes 塞一段 bytes。
func (b *Builder) Bytes(p []byte) *Builder {
	for _, c := range p {
		b.Uint(uint64(c), 8)
	}
	return b
}

// Coins 塞一個金額，格式是 VarUInteger 16：4 個 bit 的長度，接著那麼多個 byte 的大端整數，
// 零就是長度 0。TON 的 nanoton 與 jetton 的最小單位都用這個格式。
func (b *Builder) Coins(v *big.Int) *Builder {
	if b.err != nil {
		return b
	}
	if v == nil || v.Sign() < 0 || v.BitLen() > 120 {
		b.err = ErrCoinsTooLarge
		return b
	}
	p := v.Bytes()
	b.Uint(uint64(len(p)), 4)
	return b.Bytes(p)
}

// Address 塞一個地址：零值是 addr_none（兩個 0 bit），其他是 addr_std（10、沒有 anycast、
// int8 的 workchain、256 bits 的帳戶雜湊）。
func (b *Builder) Address(a Address) *Builder {
	if a.IsZero() {
		return b.Uint(0, 2)
	}
	b.Uint(2, 2).Bit(false).Int(int64(a.Workchain), 8)
	return b.Bytes(a.Hash[:])
}

// Ref 掛一個 ref。
func (b *Builder) Ref(c *Cell) *Builder {
	if b.err != nil {
		return b
	}
	if len(b.refs) >= MaxRefs {
		b.err = ErrTooManyRefs
		return b
	}
	b.refs = append(b.refs, c)
	return b
}

// MaybeRef 是 TL-B 的 Maybe ^Cell：nil 是一個 0 bit，否則一個 1 bit 加一個 ref。
func (b *Builder) MaybeRef(c *Cell) *Builder {
	if c == nil {
		return b.Bit(false)
	}
	return b.Bit(true).Ref(c)
}

// Build 交出組好的 cell。
func (b *Builder) Build() (*Cell, error) {
	if b.err != nil {
		return nil, b.err
	}
	c := &Cell{
		data: append([]byte(nil), b.data[:(b.bits+7)/8]...),
		bits: b.bits,
		refs: append([]*Cell(nil), b.refs...),
	}
	for _, r := range c.refs {
		if r.depth+1 > c.depth {
			c.depth = r.depth + 1
		}
	}
	if c.depth > 0xffff {
		return nil, ErrDepth
	}
	c.hash = sha256.Sum256(representation(c.data, c.bits, c.refs))
	return c, nil
}

// Slice 從一個 cell 依序讀內容。跟 Builder 一樣，錯誤黏住不放：任何一步讀過頭，
// 之後的每一步都回零值，最後用 Err 一次檢查。
type Slice struct {
	c   *Cell
	pos int
	ref int
	err error
}

// Err 回報到目前為止有沒有讀過頭。
func (s *Slice) Err() error { return s.err }

// Remaining 回報還剩幾個 bit 沒讀。
func (s *Slice) Remaining() int { return s.c.bits - s.pos }

// RemainingRefs 回報還剩幾個 ref 沒讀。
func (s *Slice) RemainingRefs() int { return len(s.c.refs) - s.ref }

// Bit 讀一個 bit。
func (s *Slice) Bit() bool {
	if s.err != nil {
		return false
	}
	if s.pos >= s.c.bits {
		s.err = ErrShortRead
		return false
	}
	v := s.c.data[s.pos/8]&(0x80>>(s.pos%8)) != 0
	s.pos++
	return v
}

// Uint 讀一個 n bit 的無號整數。
func (s *Slice) Uint(n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		v <<= 1
		if s.Bit() {
			v |= 1
		}
	}
	return v
}

// Int 讀一個 n bit 的有號整數。
func (s *Slice) Int(n int) int64 {
	v := s.Uint(n)
	if n < 64 && v>>(uint(n)-1) == 1 {
		v |= ^uint64(0) << uint(n)
	}
	return int64(v)
}

// Bytes 讀 n 個 byte。
func (s *Slice) Bytes(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(s.Uint(8))
	}
	return p
}

// Coins 讀一個 VarUInteger 16。
func (s *Slice) Coins() *big.Int {
	n := s.Uint(4)
	return new(big.Int).SetBytes(s.Bytes(int(n)))
}

// Address 讀一個地址：addr_none 回零值，addr_std 回 workchain 與雜湊。其他兩種（addr_extern、
// addr_var）付款 message 裡不會出現，讀到就當成讀壞了。
func (s *Slice) Address() Address {
	switch s.Uint(2) {
	case 0:
		return Address{}
	case 2:
		if s.Bit() {
			s.err = ErrShortRead
			return Address{}
		}
		a := Address{Workchain: int8(s.Int(8))}
		copy(a.Hash[:], s.Bytes(32))
		return a
	default:
		s.err = ErrShortRead
		return Address{}
	}
}

// Ref 讀下一個 ref。
func (s *Slice) Ref() *Cell {
	if s.err != nil {
		return nil
	}
	if s.ref >= len(s.c.refs) {
		s.err = ErrShortRead
		return nil
	}
	r := s.c.refs[s.ref]
	s.ref++
	return r
}

// MaybeRef 讀一個 Maybe ^Cell。
func (s *Slice) MaybeRef() *Cell {
	if !s.Bit() {
		return nil
	}
	return s.Ref()
}

// Stats 是一棵 cell 樹的規模：去重之後有幾個 cell、最深幾層。鏈對 external message 的上限就是拿這兩個數
// 加上 BoC 的長度來管的。
type Stats struct {
	Cells int
	Depth int
}

// Count 回報以這個 cell 為根的樹有多少個不同的 cell（同一個雜湊只算一次，跟 BoC 的算法一致）。
func (c *Cell) Count() Stats {
	return Stats{Cells: len(order(c)), Depth: c.depth}
}

// order 是 BoC 裡 cell 的排列順序：從根開始的深度優先，同一個 cell（同一個雜湊）只出現一次，
// 而且每個 cell 都排在它指向的 cell 前面。這個順序跟 @ton/core 的 topologicalSort 一致
// （後序走訪、ref 從最後一個往前走、最後整個反過來），所以序列化出來的 bytes 也一致。
func order(root *Cell) []*Cell {
	var post []*Cell
	seen := make(map[[32]byte]bool)
	var visit func(c *Cell)
	visit = func(c *Cell) {
		if seen[c.hash] {
			return
		}
		seen[c.hash] = true
		for i := len(c.refs) - 1; i >= 0; i-- {
			visit(c.refs[i])
		}
		post = append(post, c)
	}
	visit(root)
	for i, j := 0, len(post)-1; i < j; i, j = i+1, j-1 {
		post[i], post[j] = post[j], post[i]
	}
	return post
}

// bocMagic 是 BoC 的開頭四個 bytes。
const bocMagic = 0xb5ee9c72

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ToBoC 把這棵樹序列化成 bag of cells：沒有索引表、帶 CRC32-C，跟 @ton/core 預設的 toBoc() 同一種形狀。
// 節點收 external message 收的就是這段 bytes，鏈對 external message 的大小上限（65,535 bytes）算的也是它。
func (c *Cell) ToBoC() []byte {
	cells := order(c)
	index := make(map[[32]byte]int, len(cells))
	for i, x := range cells {
		index[x.hash] = i
	}
	sizeBytes := bytesFor(uint64(len(cells)))
	total := 0
	for _, x := range cells {
		total += 2 + (x.bits+7)/8 + len(x.refs)*sizeBytes
	}
	offBytes := bytesFor(uint64(total))

	out := make([]byte, 0, 6+4*sizeBytes+offBytes+total+4)
	out = binary.BigEndian.AppendUint32(out, bocMagic)
	out = append(out, byte(0x40|sizeBytes), byte(offBytes)) // 沒有索引、有 CRC、沒有 cache bits、flags 0
	out = appendBE(out, uint64(len(cells)), sizeBytes)
	out = appendBE(out, 1, sizeBytes) // 一個根
	out = appendBE(out, 0, sizeBytes) // 沒有缺席的 cell
	out = appendBE(out, uint64(total), offBytes)
	out = appendBE(out, 0, sizeBytes) // 根是第 0 個
	for _, x := range cells {
		out = append(out, byte(len(x.refs)), byte(x.bits/8+(x.bits+7)/8))
		out = append(out, padded(x.data, x.bits)...)
		for _, r := range x.refs {
			out = appendBE(out, uint64(index[r.hash]), sizeBytes)
		}
	}
	return binary.LittleEndian.AppendUint32(out, crc32.Checksum(out, castagnoli))
}

// bytesFor 是表示 v 最少要幾個 byte，最少一個。
func bytesFor(v uint64) int {
	n := 1
	for v >= 1<<(8*uint(n)) {
		n++
	}
	return n
}

// appendBE 用 n 個 byte 的大端整數接上去。
func appendBE(b []byte, v uint64, n int) []byte {
	for i := n - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*uint(i))))
	}
	return b
}

// Short 印一個雜湊的前 8 個 hex 字元加 …，給報告用；跟 recon 縮 hash 的方式同一種。
func Short(h [32]byte) string {
	return fmt.Sprintf("%x…", h[:4])
}

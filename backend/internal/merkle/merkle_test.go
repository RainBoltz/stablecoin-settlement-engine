package merkle_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/merkle"
)

// leafOf 造一片測試用的葉子：內容只要每片不同就夠了，編碼是呼叫端的事。
func leafOf(i int) [merkle.Size]byte {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], uint64(i))
	return merkle.Leaf(data[:])
}

// run 造一排 n 片互不相同的葉子。
func run(n int) [][merkle.Size]byte {
	leaves := make([][merkle.Size]byte, n)
	for i := range leaves {
		leaves[i] = leafOf(i)
	}
	return leaves
}

// blockLeaves 取第 block 個對齊區塊的葉子，不足的位置照規則放 PadLeaf。
func blockLeaves(leaves [][merkle.Size]byte, block, align int) [][merkle.Size]byte {
	out := make([][merkle.Size]byte, align)
	for i := range out {
		idx := block*align + i
		if idx < len(leaves) {
			out[i] = leaves[idx]
		} else {
			out[i] = merkle.PadLeaf
		}
	}
	return out
}

// 防的情境：名單筆數幾乎不會剛好是 2 的冪次。墊滿是 Build 自己的事，
// 呼叫端手動墊出來的樹要跟它算出同一個 root，不然兩邊的「第幾個區塊」對不上。
func TestBuild_PadsToAPowerOfTwo(t *testing.T) {
	leaves := run(300)
	auto, err := merkle.Build(leaves)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	padded := make([][merkle.Size]byte, 512)
	copy(padded, leaves)
	for i := 300; i < 512; i++ {
		padded[i] = merkle.PadLeaf
	}
	manual, err := merkle.Build(padded)
	if err != nil {
		t.Fatalf("Build padded: %v", err)
	}
	if auto.Root() != manual.Root() {
		t.Fatalf("auto-padded root %x != manually padded root %x", auto.Root(), manual.Root())
	}
	if auto.Depth() != 9 {
		t.Fatalf("depth of a 300-leaf tree = %d, want 9", auto.Depth())
	}
}

// 防的情境：空名單。root 算不出來，也不該有人拿全零的樹去簽。
func TestBuild_RejectsAnEmptyRun(t *testing.T) {
	if _, err := merkle.Build(nil); !errors.Is(err, merkle.ErrEmptyTree) {
		t.Fatalf("err = %v, want ErrEmptyTree", err)
	}
}

// 防的情境：整輪撥款的每一批都要驗得過，包含最後那個沒填滿、靠 PadLeaf 補位的區塊。
func TestBlockProof_VerifiesEveryAlignedBlock(t *testing.T) {
	leaves := run(300)
	tree, err := merkle.Build(leaves)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	const align, alignLog2 = 8, 3
	blocks := 512 / align
	for b := 0; b < blocks; b++ {
		proof, err := tree.BlockProof(b, alignLog2)
		if err != nil {
			t.Fatalf("BlockProof(%d): %v", b, err)
		}
		if len(proof) != tree.Depth()-alignLog2 {
			t.Fatalf("proof of block %d has %d hashes, want %d", b, len(proof), tree.Depth()-alignLog2)
		}
		if !merkle.VerifyBlock(tree.Root(), b, blockLeaves(leaves, b, align), proof) {
			t.Fatalf("block %d does not verify", b)
		}
	}
}

// 防的情境：切批的程式碼把區塊編號算歪一格。範圍外要拿到明確的錯，不是一份驗不過的證明。
func TestBlockProof_RejectsABlockOutOfRange(t *testing.T) {
	tree, err := merkle.Build(run(300))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := tree.BlockProof(64, 3); !errors.Is(err, merkle.ErrBlockOutOfRange) {
		t.Fatalf("err = %v, want ErrBlockOutOfRange", err)
	}
	if _, err := tree.BlockProof(-1, 3); !errors.Is(err, merkle.ErrBlockOutOfRange) {
		t.Fatalf("err = %v, want ErrBlockOutOfRange", err)
	}
	if _, err := tree.BlockProof(0, 10); !errors.Is(err, merkle.ErrBlockTooLarge) {
		t.Fatalf("err = %v, want ErrBlockTooLarge", err)
	}
}

// 防的情境：relayer 帶上鏈的內容跟 payer 簽的名單差了一個字。
// 金額動一格，重算出來的就是另一個 root，整批要被擋下來。
func TestVerifyBlock_RejectsATamperedLeaf(t *testing.T) {
	leaves := run(300)
	tree, err := merkle.Build(leaves)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	proof, err := tree.BlockProof(2, 3)
	if err != nil {
		t.Fatalf("BlockProof: %v", err)
	}
	tampered := blockLeaves(leaves, 2, 8)
	tampered[5] = merkle.Leaf([]byte("one lamport more"))
	if merkle.VerifyBlock(tree.Root(), 2, tampered, proof) {
		t.Fatal("a tampered leaf verified against the signed root")
	}
}

// 防的情境：拿第 3 個區塊的證明去交第 4 個區塊的內容。區塊編號簽在驗證的路徑裡，對不上就過不了。
func TestVerifyBlock_RejectsAProofFromAnotherBlock(t *testing.T) {
	leaves := run(300)
	tree, err := merkle.Build(leaves)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	proof3, err := tree.BlockProof(3, 3)
	if err != nil {
		t.Fatalf("BlockProof: %v", err)
	}
	if merkle.VerifyBlock(tree.Root(), 4, blockLeaves(leaves, 4, 8), proof3) {
		t.Fatal("block 4 verified with the proof of block 3")
	}
}

// 防的情境：區塊裡兩筆付款被對調。左右順序照位置接、不排序，換位就是另一個 root。
func TestVerifyBlock_RejectsAReorderedBlock(t *testing.T) {
	leaves := run(16)
	tree, err := merkle.Build(leaves)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	proof, err := tree.BlockProof(0, 3)
	if err != nil {
		t.Fatalf("BlockProof: %v", err)
	}
	swapped := blockLeaves(leaves, 0, 8)
	swapped[0], swapped[1] = swapped[1], swapped[0]
	if merkle.VerifyBlock(tree.Root(), 0, swapped, proof) {
		t.Fatal("a reordered block verified against the signed root")
	}
}

// 防的情境：跨語言重寫。鏈上程式是 Rust，兩邊只要有一個 byte 的編碼歧義，
// root 就對不上，所以這裡釘一個固定輸入的 root，Rust 那一側的測試釘同一個值。
// 名單先墊到一個區塊（8 片）再建樹，因為程式那一側的樹最小就是一個區塊寬。
func TestBuild_GoldenRootAcrossImplementations(t *testing.T) {
	leaves := make([][merkle.Size]byte, 8)
	for i := 0; i < 3; i++ {
		leaves[i] = merkle.Leaf([]byte(fmt.Sprintf("leaf-%d", i)))
	}
	tree, err := merkle.Build(leaves)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	const want = "319fbb219345322cf146ff74a9c59b43c5156fd5651c676733df324001e63f66"
	if got := fmt.Sprintf("%x", tree.Root()); got != want {
		t.Fatalf("root = %s, want %s", got, want)
	}
}

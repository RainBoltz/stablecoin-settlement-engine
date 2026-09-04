// Package merkle 把一份名單收成一個 32 bytes 的承諾（root），並替名單上對齊的區塊開證明。
//
// 它服務的是「一輪撥款只簽一次」：payer 把 root 簽上鏈之後，relayer 分批把名單的內容
// 帶回鏈上，鏈上的程式重算一次就知道這一批是不是名單裡的那幾行。所以這裡的證明不是
// 一片葉子一份（那是 airdrop 讓每個人自己來領的形狀），而是一個對齊區塊共用一份：
// 整批 2^k 片葉子只要付「區塊的根走回 root」那一段的兄弟節點，batch 越大攤得越薄。
//
// 雜湊用 SHA-256 而不是 keccak256，理由跟 paymentref 相同：Go 標準函式庫就有，
// 而 Solana 程式那一側有對應的 sha256 syscall，兩邊算得出同一個 root。葉子與內部節點
// 各帶一個 domain byte（0x00 與 0x01），一片葉子的雜湊值永遠當不成一個內部節點，反之亦然。
//
// 本 package 為本系列從零設計，只取公開設計裡需要的那部分。
package merkle

import (
	"crypto/sha256"
	"errors"
	"math/bits"
)

// Size 是這棵樹裡每一個雜湊值的長度。
const Size = sha256.Size

const (
	domainLeaf byte = 0x00
	domainNode byte = 0x01
)

// ErrEmptyTree：一片葉子都沒有。空名單連 root 都算不出來，呼叫端應該在更早的地方擋下它。
var ErrEmptyTree = errors.New("merkle: the tree has no leaves")

// ErrBlockOutOfRange：要求證明的區塊不在樹上。
var ErrBlockOutOfRange = errors.New("merkle: the block is out of range")

// ErrBlockTooLarge：對齊區塊比整棵樹還大。
var ErrBlockTooLarge = errors.New("merkle: the aligned block is larger than the tree")

// Leaf 把一片葉子的原始內容雜湊成葉子節點。內容怎麼編碼是呼叫端的事：
// 這裡只保證掛上 domain byte，讓葉子跟內部節點活在不同的雜湊空間。
func Leaf(data []byte) [Size]byte {
	h := sha256.New()
	h.Write([]byte{domainLeaf})
	h.Write(data)
	var out [Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// PadLeaf 是墊到 2 的冪次用的空葉子。它是全零的 32 bytes，不經過 Leaf()：
// 真葉子一定帶著 domainLeaf 算出來，沒有任何已知內容的葉子會剛好等於全零。
var PadLeaf [Size]byte

// node 把兩個子節點收成一個內部節點。左右順序照位置，不排序：
// 驗證的一方知道自己在第幾個區塊，每一層往哪邊接是算得出來的，不需要方向旗標。
func node(left, right [Size]byte) [Size]byte {
	h := sha256.New()
	h.Write([]byte{domainNode})
	h.Write(left[:])
	h.Write(right[:])
	var out [Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Tree 是一棵建好的樹。levels[0] 是墊滿之後的葉子層，最後一層只有 root 一個節點。
type Tree struct {
	levels [][][Size]byte
}

// Build 把一排葉子節點（已經過 Leaf() 的雜湊值）建成一棵樹。
// 葉子數不是 2 的冪次時用 PadLeaf 墊滿：墊出來的那幾片不對應任何付款，
// 也永遠不會出現在任何一批的實際項目裡，它們只是讓每一層都收得成對。
func Build(leaves [][Size]byte) (Tree, error) {
	if len(leaves) == 0 {
		return Tree{}, ErrEmptyTree
	}
	width := 1
	for width < len(leaves) {
		width *= 2
	}
	level := make([][Size]byte, width)
	copy(level, leaves)
	for i := len(leaves); i < width; i++ {
		level[i] = PadLeaf
	}

	t := Tree{levels: [][][Size]byte{level}}
	for len(level) > 1 {
		next := make([][Size]byte, len(level)/2)
		for i := range next {
			next[i] = node(level[2*i], level[2*i+1])
		}
		t.levels = append(t.levels, next)
		level = next
	}
	return t, nil
}

// Root 回報整棵樹的承諾。
func (t Tree) Root() [Size]byte {
	return t.levels[len(t.levels)-1][0]
}

// Depth 回報樹高：葉子走回 root 要經過幾層。墊滿之後 2^Depth 就是葉子層的寬度。
func (t Tree) Depth() int {
	return len(t.levels) - 1
}

// BlockProof 開出「第 block 個對齊區塊」的證明：區塊裡有 2^alignLog2 片葉子，
// 證明是區塊自己的根一路走回 root 沿途缺的另一半，一層一個，共 Depth-alignLog2 個。
// 整批共用這一份，是 batch 越大證明攤得越薄的原因。
func (t Tree) BlockProof(block int, alignLog2 int) ([][Size]byte, error) {
	if alignLog2 > t.Depth() {
		return nil, ErrBlockTooLarge
	}
	blocks := len(t.levels[alignLog2])
	if block < 0 || block >= blocks {
		return nil, ErrBlockOutOfRange
	}
	proof := make([][Size]byte, 0, t.Depth()-alignLog2)
	idx := block
	for level := alignLog2; level < t.Depth(); level++ {
		proof = append(proof, t.levels[level][idx^1])
		idx >>= 1
	}
	return proof, nil
}

// VerifyBlock 用一份區塊證明重算 root。leaves 要帶滿整個區塊（不足的位置放 PadLeaf），
// 順序照名單原本的順序。這個函式跟鏈上程式做的是同一件事，測試靠它保證兩邊對得起來。
func VerifyBlock(root [Size]byte, block int, leaves [][Size]byte, proof [][Size]byte) bool {
	if len(leaves) == 0 || bits.OnesCount(uint(len(leaves))) != 1 {
		return false
	}
	level := make([][Size]byte, len(leaves))
	copy(level, leaves)
	for len(level) > 1 {
		next := make([][Size]byte, len(level)/2)
		for i := range next {
			next[i] = node(level[2*i], level[2*i+1])
		}
		level = next
	}
	acc := level[0]
	idx := block
	for _, sibling := range proof {
		if idx%2 == 0 {
			acc = node(acc, sibling)
		} else {
			acc = node(sibling, acc)
		}
		idx >>= 1
	}
	return acc == root
}

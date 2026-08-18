package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// DomainV1 是 journal hash 的前綴，跟 paymentref.DomainV1 同一個用意：跟別的系統對同一批 bytes 算出來的 SHA-256
// 不會撞在一起，換編碼就換版本號。
const DomainV1 = "stablecoin-settlement-engine/ledger-journal/v1"

// Preimage 是一列被雜湊的原始位元組：前綴、上一列的 Hash、然後是呼叫端填的每一個欄位，每個欄位前面帶 uvarint 長度
// （長度前綴的理由跟 paymentref.Preimage 一樣：欄位邊界不會混）。Seq 也算進去，所以同一列搬到別的位置也對不上。
//
// 公開這個函式的理由跟 paymentref 一樣：稽核工具拿到 journal 的匯出，不必信任我們的資料庫，自己從第一列算到最後一列，
// 對得上最後那個 Hash 就代表中間沒有任何一列被動過。
func Preimage(prev [32]byte, e Entry) []byte {
	b := make([]byte, 0, 256)
	put := func(s string) {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	put(DomainV1)
	b = append(b, prev[:]...)
	b = binary.AppendUvarint(b, e.Seq)
	put(e.ID)
	b = append(b, e.Ref[:]...)
	put(string(e.Kind))
	put(e.Holds)
	put(e.Asset.Chain)
	put(e.Asset.Token)
	b = binary.AppendUvarint(b, uint64(len(e.Legs)))
	for _, l := range e.Legs {
		put(string(l.Account))
		put(l.Amount.String())
	}
	put(e.By)
	put(e.At.UTC().Format(time.RFC3339Nano))
	put(e.TxHash)
	put(e.Memo)
	return b
}

// hashEntry 算一列的 Hash：sha256(Preimage(prev, e))。
func hashEntry(prev [32]byte, e Entry) [32]byte {
	return sha256.Sum256(Preimage(prev, e))
}

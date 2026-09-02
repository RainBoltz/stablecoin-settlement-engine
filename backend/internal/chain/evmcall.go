package chain

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
)

// settleBatchSelector 是 settleBatch(address,address,(address,uint256,bytes32)[]) 的函式選擇子：
// ABI 規定 calldata 的前四個 bytes 是函式簽章 keccak256 的前四個 bytes
// （https://docs.soliditylang.org/en/v0.8.26/abi-spec.html）。
// keccak256 不在 Go 標準函式庫裡，所以這四個 bytes 是用 Foundry 的
// `cast sig "settleBatch(address,address,(address,uint256,bytes32)[])"` 算好釘死的：
// 選擇子是資料：跟著函式簽章走，簽章不變它就不變。
const settleBatchSelector = "\xd0\xe1\xd6\x48"

// ErrBadAddress：不是 0x 加 40 個 hex 字元。EVM 的 builder 只收這一種寫法，
// 理由跟 paymentref.Parse 一樣：同一個東西有兩種以上的寫法，遲早有兩個系統對不上。
var ErrBadAddress = errors.New("chain: not a 0x-prefixed 20-byte hex address")

// SettleBatchCalldata 把一批付款編成結算合約 settleBatch 的 calldata。
//
// 輸出裝的是「付款」的全部；「交易」還缺投遞的那一半：nonce、gas 的出價、chain id 都不在裡面，
// 它們住在信封上，由 txseq 與 txfee 在送出的時候才決定。付款與投遞分開放，
// 正是同一筆付款可以換個信封重送（替換）的原因。
//
// 編碼照 ABI 規格（https://docs.soliditylang.org/en/v0.8.26/abi-spec.html）：三個參數的
// head 各占一個 word，動態陣列在 head 放偏移量、內容接在後面，先長度、再逐項三個 word。
// Payout 是靜態 tuple，所以逐項直接展開，沒有第二層偏移量。
func (e *EVM) SettleBatchCalldata(token, payer string, items []bulk.Payout) ([]byte, error) {
	if len(items) == 0 {
		return nil, ErrEmptyBatch
	}
	tok, err := parseAddress(token)
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	pay, err := parseAddress(payer)
	if err != nil {
		return nil, fmt.Errorf("payer: %w", err)
	}

	out := make([]byte, 0, 4+96+32+96*len(items))
	out = append(out, settleBatchSelector...)
	out = appendAddressWord(out, tok)
	out = appendAddressWord(out, pay)
	// 第三個參數是動態的：head 放的是它的內容從參數區的第幾個 byte 開始（0x60，跳過三個 head word）。
	out = appendUintWord(out, 0x60)
	out = appendUintWord(out, uint64(len(items)))
	for i, it := range items {
		m, err := parseAddress(it.Merchant)
		if err != nil {
			return nil, fmt.Errorf("payout %d merchant: %w", i, err)
		}
		if it.Amount == nil || it.Amount.Sign() <= 0 {
			return nil, fmt.Errorf("%w: payout %d", ErrBadAmount, i)
		}
		if it.Amount.BitLen() > 256 {
			return nil, fmt.Errorf("%w: payout %d does not fit a uint256", ErrBadAmount, i)
		}
		if it.Ref.IsZero() {
			return nil, fmt.Errorf("%w: payout %d", ErrZeroRef, i)
		}
		out = appendAddressWord(out, m)
		out = append(out, it.Amount.FillBytes(make([]byte, 32))...)
		out = append(out, it.Ref[:]...)
	}
	return out, nil
}

// parseAddress 把 0x 加 40 個 hex 的地址讀成 20 bytes，大小寫不拘。
// 不驗 EIP-55 的大小寫檢查碼：intent 上存什麼就編什麼，正規化是 API 收請求時的事。
func parseAddress(s string) ([20]byte, error) {
	var a [20]byte
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return a, fmt.Errorf("%w: %q", ErrBadAddress, s)
	}
	if _, err := hex.Decode(a[:], []byte(s[2:])); err != nil {
		return a, fmt.Errorf("%w: %q", ErrBadAddress, s)
	}
	return a, nil
}

// appendAddressWord 把 20 bytes 的地址靠右放進一個 32 bytes 的 word。
func appendAddressWord(b []byte, a [20]byte) []byte {
	b = append(b, make([]byte, 12)...)
	return append(b, a[:]...)
}

// appendUintWord 把一個小整數靠右放進一個 32 bytes 的 word。
func appendUintWord(b []byte, v uint64) []byte {
	w := make([]byte, 32)
	for i := 0; v > 0; i++ {
		w[31-i] = byte(v)
		v >>= 8
	}
	return append(b, w...)
}

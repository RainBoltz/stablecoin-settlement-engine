// build.go 是 adapter 的第二半：把一批付款組成那條鏈的未簽名交易內容。
//
// 這一半刻意沒有共用介面。Adapter 的四題答案都長一樣（一個名字、一份規則），所以收得進
// 同一個介面；組交易連參數都不長一樣：EVM 要三個 20 bytes 的地址，Solana 要六個固定帳戶、
// 每個 merchant 的 token 帳戶、還要一個已經在訊息裡簽掉的 recent blockhash。硬把它們塞進
// 同一個 Build(batch) 的話，參數只能是 map[string]any 這種誰都不知道要放什麼的東西。
//
// 兩個 builder 共用的只有輸入與義務：輸入是同一份 []bulk.Payout（merchant、金額、ref，
// 付款的身分），義務是名單上每一把 ref 都要一字節不差地出現在輸出裡，鏈上的 listener
// 靠的就是它。ref 是唯一兩邊都認得的東西；除此之外，兩條鏈的輸出連「錢包簽的是什麼」
// 都不一樣（EVM 簽的是包著 calldata 的信封的雜湊，Solana 簽的就是 message 本身）。
//
// 簽名不在這裡，也不會在這個 package 的任何地方：EVM 的簽名要 keccak256 與 secp256k1，
// 兩個都不在 Go 標準函式庫裡，一個「共用簽名器」等於逼整個 repo 吃下第一個外部依賴。
// 誰簽、用什麼曲線簽，是每條鏈自己的事，留在 adapter 的邊界後面。
package chain

import "errors"

var (
	// ErrEmptyBatch：這一批是空的。結算合約對空 batch 的回答是 revert（"the batch is empty"），
	// 組一筆一定會被拒絕的交易沒有意義。
	ErrEmptyBatch = errors.New("chain: the batch is empty")
	// ErrZeroRef：名單上有一筆付款帶著零值的 ref。ref 的零值代表「還沒算」（見 paymentref），
	// 而合約的 _reserve 對零 ref 一律 revert；這種東西越早被擋下來，離出錯的那一行越近。
	ErrZeroRef = errors.New("chain: a payout carries a zero ref")
	// ErrBadAmount：金額是 nil、零或負數。合約的 _reserve 要求 amount > 0，
	// 組出來也只是送一筆必定 revert 的交易。
	ErrBadAmount = errors.New("chain: a payout amount must be a positive integer")
)

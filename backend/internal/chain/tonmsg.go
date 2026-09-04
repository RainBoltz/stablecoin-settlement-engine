package chain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math/big"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/boc"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// tonmsg.go 是 TON 那一半的 builder：把一批付款組成 relayer 要簽的那一則 external message。
//
// 這條鏈上沒有「一筆交易裡的 N 筆付款」這種東西。合約之間只能互送 message，每一則 message 在收件的
// 合約那裡各自變成一筆交易，可能在不同的 shard、不同的區塊裡執行（TON 文件把這叫
// asynchronous message handling，https://docs.ton.org/blockchain-basics/core-concepts）。
// relayer 簽的是一則給錢包合約的 external message，裡面裝 N 個「送一則 message」的動作；錢包那筆交易
// 只負責把 N 則 internal message 送出去，錢一毛都還沒動。一則付款 message 接下來會走的路是 TEP-74 定的
//（https://github.com/ton-blockchain/TEPs/blob/master/text/0074-jettons-standard.md）：
// 錢包 → 我們的 jetton wallet（transfer）→ merchant 的 jetton wallet（internal_transfer）
// → merchant 本人（transfer_notification）與我們的錢包（excesses）：錢包那筆交易之後還有
// 四則 message、四筆交易，散在四個帳戶上，見 TONHops。
//
// 所以這裡的「batch」只是運輸單位：N 則 message 共用一個 seqno、一個簽名、一筆錢包交易，
// 送出去之後各走各的，一則失敗不會回滾另外 N-1 則。跟 EVM 的 settleBatch 剛好相反，
// 而且 TON 上沒有辦法要到「整批一起成功或失敗」，因為沒有任何一筆交易同時碰得到 N 個收款的合約。
//
// 錢包用的是 W5（v5r1）：「the new contract allows you to send up to 255 messages at a time」
//（https://docs.ton.org/standard/wallets/v5），跟 TVM 對一筆交易 action list 的上限同一個數字。
// 簽名的對象是這裡組出來的 signing cell 的雜湊：opcode、wallet_id、valid_until、seqno、動作清單。
// 版面照 W5 的 TL-B，並跟 @ton/ton 16.3.0 的 createWalletTransferV5R1 逐 byte 對過（見測試）；組出來的
// request 也在 @ton/sandbox 裡讓真的 W5 合約與真的 jetton 合約收過一遍（contracts/ton，make ton-test）。

const (
	// tonAuthSignedExternal 是 W5 外部簽名請求的 opcode（ASCII 的 "sign"）。
	tonAuthSignedExternal = 0x7369676e
	// tonActionSendMsg 是 TVM action list 裡「送一則 message」那種動作的 tag。
	tonActionSendMsg = 0x0ec3c86d
	// tonSendMode 是每一則 message 的 send mode：1 是手續費從錢包餘額另外付、不從附上的 value 扣，
	// 2 是這一則送不出去時略過它、不要讓整筆錢包交易失敗。W5 對 external message 強制要 +2，
	// 理由跟這裡的設計一致：一則壞掉的 message 不該拖住同一批的其他 254 則。
	tonSendMode = 1 + 2

	// TONOpTransfer 到 TONOpExcesses 是 TEP-74 定的四個 op，數值是 TL-B 宣告字串的 crc32。
	TONOpTransfer             = 0xf8a7ea5
	TONOpInternalTransfer     = 0x178d4519
	TONOpTransferNotification = 0x7362d09c
	TONOpExcesses             = 0xd53276db

	// TONMaxMessages 是一則 W5 external message 裝得下的動作數：255。超過的話錢包交易的 action phase
	// 直接失敗（「If there are more than 255 actions queued for execution, the action phase will
	// throw an error with an exit code 33」，https://docs.ton.org/tvm/exit-codes），
	// 而不是送出前 255 則。
	TONMaxMessages = 255

	// TONDefaultWalletID 是 W5 錢包的預設 wallet_id：「wallet_id = network_global_id ^ context_id」
	//（https://docs.ton.org/standard/wallets/v5），mainnet 的 network_global_id 是 -239，
	// context 是「basechain、v5r1、subwallet 0」那 32 個 bit，XOR 出來就是 0x7FFFFF11。
	// 同一把公鑰在不同網路、不同 subwallet 上是不同的合約地址，這個數字就是差別所在。
	TONDefaultWalletID = 0x7FFFFF11

	// TONAttach 是每一則付款 message 附上的 TON：0.05 TON（50,000,000 nanoton）。這筆錢不是手續費，
	// 是這則 message 接下來三四筆交易的 gas、可能要替 merchant 部署 jetton wallet 的儲存費、
	// 以及 forward 出去的那一點點；花剩的由 merchant 的 jetton wallet 用 excesses 退回我們的錢包。
	// 下限來自參考實作 jetton-wallet.fc 的 throw_unless(709, msg_value > forward_ton_amount +
	// fwd_count * fwd_fee + 2 * gas_consumption() + min_tons_for_storage())，gas_consumption 是
	// 0.015 TON、min_tons_for_storage 是 0.01 TON
	//（https://github.com/ton-blockchain/token-contract/blob/main/ft/jetton-wallet.fc）；
	// 0.05 是在那個下限（約 0.04 加兩份 fwd_fee）之上留一點餘裕的設定值，不是量出來的。
	TONAttach = 50_000_000

	// TONForward 是 forward_ton_amount：1 nanoton。TEP-74 規定它是零的時候 merchant 收不到
	// transfer_notification，所以給最小的非零值，notification 才會發出去
	//（https://docs.ton.org/applications/payments/jettons：「Services must set forward_ton_amount
	// to at least 0.000000001 Gram (1 nanogram) when sending tokens to trigger notifications.」）。
	TONForward = 1
)

// TONPayloadOp 是我們放在 forward_payload 開頭的 op，後面接 32 bytes 的 ref。
// 照 TEP-74 對 op 的慣例算：TL-B 宣告字串的 crc32 去掉最高位。它讓收到 payload 的人
// 分得出這是一把 ref 而不是一段文字（文字 comment 的 op 是 0）。
var TONPayloadOp = uint64(crc32.ChecksumIEEE([]byte(tonPayloadScheme)) & 0x7fffffff)

// tonPayloadScheme 是那段 payload 的 TL-B 宣告，只拿來算 op。
const tonPayloadScheme = "payment_ref ref:bits256 = ForwardPayload"

// TONAccounts 是一則 external message 固定會碰到的帳戶，跟 SolanaAccounts 對固定開銷的拆法是同一種角色：
// 簽名的錢包（seqno 住在這裡，也是 excesses 退款的地址）與這顆 jetton 在這個錢包名下的
// jetton wallet（每一則付款 message 的收件人）。
//
// merchant 的 jetton wallet 刻意不在名單上，跟 Solana 相反：TEP-74 的 transfer 收的是
// merchant 本人的地址，他的 jetton wallet 由我們的 jetton wallet 在鏈上自己算、必要時順便部署
// （「and optionally deploy it」）。鏈下不用先去查誰有帳戶、誰沒有，也就沒有 prepare batch。
type TONAccounts struct {
	Wallet       boc.Address
	JettonWallet boc.Address
	// WalletID 是 W5 的 wallet_id，簽進每一則 external message 裡；零值用 TONDefaultWalletID。
	WalletID int32
}

var (
	// ErrTooManyMessages：一則 external message 裝不下這麼多動作。bulk 的 messages 規則會先擋，
	// 這條是 builder 對自己的輸出負責。
	ErrTooManyMessages = errors.New("chain: a W5 request carries at most 255 messages")
	// ErrBadTONAddress：merchant 的地址讀不出來（既不是 raw 也不是 user-friendly 的寫法）。
	ErrBadTONAddress = errors.New("chain: not a TON address")
)

// TONRequest 是組好的一則 external message，還沒簽名：signing cell、它裡面每一則付款 message、每一則的 body。
type TONRequest struct {
	Seqno      uint32
	ValidUntil uint32
	items      []bulk.Payout
	bodies     []*boc.Cell
	messages   []*boc.Cell
	signing    *boc.Cell
}

// TransferRequest 把一批付款組成一則 W5 external message 的 signing cell。
//
// 一項一則 message：每一則是給我們 jetton wallet 的 internal message，附 TONAttach 的 TON、body 是 TEP-74
// 的 transfer。seqno 與 valid_until 簽在裡面：seqno 讓錢包只收這一則一次，valid_until 讓一則
// 沒被收下的 message 過期作廢，兩個都是防重放；跟 Solana 的 blockhash 一樣是「投遞資料簽在付款裡」
// 的形狀，所以卡住的時候只能原封不動重送，過期就重簽。
func (t *TON) TransferRequest(acc TONAccounts, seqno, validUntil uint32, items []bulk.Payout) (*TONRequest, error) {
	if len(items) == 0 {
		return nil, ErrEmptyBatch
	}
	if len(items) > TONMaxMessages {
		return nil, fmt.Errorf("%w: %d", ErrTooManyMessages, len(items))
	}
	if acc.Wallet.IsZero() || acc.JettonWallet.IsZero() {
		return nil, fmt.Errorf("%w: wallet or jetton wallet", ErrZeroAccount)
	}
	if acc.WalletID == 0 {
		acc.WalletID = TONDefaultWalletID
	}

	req := &TONRequest{
		Seqno:      seqno,
		ValidUntil: validUntil,
		items:      append([]bulk.Payout(nil), items...),
		bodies:     make([]*boc.Cell, len(items)),
		messages:   make([]*boc.Cell, len(items)),
	}
	for i, it := range items {
		body, err := TONTransferBody(acc, it)
		if err != nil {
			return nil, fmt.Errorf("payout %d: %w", i, err)
		}
		msg, err := tonInternalMessage(acc.JettonWallet, big.NewInt(TONAttach), body)
		if err != nil {
			return nil, err
		}
		req.bodies[i], req.messages[i] = body, msg
	}
	// OutList 是一條鏈結串列：每一格帶一個動作、ref 到前一格，最裡面是一個空 cell。這就是 TVM 的
	// c5 暫存器的形狀：後加進去的動作包在外面，action phase 從最裡面那一格開始執行。所以名單的
	// 第一則 message 放在最深處、最後一則在最外面，錢包就照名單的順序把它們送出去；
	// @ton/ton 16 起的 createWalletTransferV5R1 同一種排法（15.x 曾經反過來包，送出的順序是倒的）。
	list, err := boc.NewBuilder().Build()
	if err != nil {
		return nil, err
	}
	for i := range items {
		list, err = boc.NewBuilder().Ref(list).Uint(tonActionSendMsg, 32).Uint(tonSendMode, 8).Ref(req.messages[i]).Build()
		if err != nil {
			return nil, err
		}
	}
	// W5 的 signing cell：opcode、wallet_id、valid_until、seqno、Maybe ^OutList、然後一個 0 bit
	// 表示沒有 extended action（加減 extension 那一類，付款用不到）。簽名接在這些 bit 後面，
	// 錢包合約驗的是「簽名以外的部分」做成 cell 的雜湊，也就是這個 cell 的 Hash。
	req.signing, err = boc.NewBuilder().
		Uint(tonAuthSignedExternal, 32).Int(int64(acc.WalletID), 32).
		Uint(uint64(validUntil), 32).Uint(uint64(seqno), 32).
		MaybeRef(list).Bit(false).Build()
	if err != nil {
		return nil, err
	}
	return req, nil
}

// TONTransferBody 組一則 TEP-74 transfer 的 body。
//
// query_id 放 ref 的前 8 個 bytes：transfer_notification 與 excesses 都會原樣帶回 query_id，
// 所以之後從鏈上撈到的每一則回音都對得回是哪一筆付款。destination 是 merchant 本人，
// response_destination 是我們的錢包（花剩的 TON 退回這裡），custom_payload 不用，
// forward_ton_amount 是 TONForward，forward_payload 走 ref 那一邊、裝 TONPayloadOp 加 ref：
// 這是 ref 唯一上鏈的位置，而且會被 merchant 的 jetton wallet 原樣轉進 transfer_notification。
func TONTransferBody(acc TONAccounts, it bulk.Payout) (*boc.Cell, error) {
	if it.Ref.IsZero() {
		return nil, ErrZeroRef
	}
	if it.Amount == nil || it.Amount.Sign() <= 0 {
		return nil, ErrBadAmount
	}
	to, err := boc.ParseAddress(it.Merchant)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrBadTONAddress, it.Merchant)
	}
	payload, err := tonPayload(it.Ref)
	if err != nil {
		return nil, err
	}
	return boc.NewBuilder().
		Uint(TONOpTransfer, 32).
		Uint(binary.BigEndian.Uint64(it.Ref[:8]), 64).
		Coins(it.Amount).
		Address(to).
		Address(acc.Wallet).
		MaybeRef(nil).
		Coins(big.NewInt(TONForward)).
		Bit(true).Ref(payload).
		Build()
}

// tonPayload 是 forward_payload 的 cell：op 加 32 bytes 的 ref。
func tonPayload(ref paymentref.Ref) (*boc.Cell, error) {
	return boc.NewBuilder().Uint(TONPayloadOp, 32).Bytes(ref[:]).Build()
}

// tonInternalMessage 組一則錢包要送出去的 internal message（TL-B 的 MessageRelaxed）：表頭 6 個 bit 是
// int_msg_info、ihr_disabled、bounce、bounced 加 addr_none 的 src（也就是 FunC 裡常見的 0x18），
// 然後收件人、附上的 value（沒有 extra currency）、ihr_fee 與 fwd_fee 都填 0（錢包送出時由
// action phase 補上真值）、created_lt 與 created_at 同樣填 0、沒有 init、body 走 ref 那一邊。
//
// bounce 開著是刻意的：付給 jetton wallet 的 message 要是被拒（jetton 不夠、合約不存在），
// 附上的 TON 會以 bounced message 退回錢包，而不是留在對面。
func tonInternalMessage(to boc.Address, value *big.Int, body *boc.Cell) (*boc.Cell, error) {
	return boc.NewBuilder().
		Uint(0b011000, 6).
		Address(to).
		Coins(value).Bit(false).
		Coins(big.NewInt(0)).Coins(big.NewInt(0)).
		Uint(0, 64).Uint(0, 32).
		Bit(false).
		Bit(true).Ref(body).
		Build()
}

// Cell 是 relayer 要簽的 signing cell。
func (r *TONRequest) Cell() *boc.Cell { return r.signing }

// SigningHash 是 relayer 的 ed25519 私鑰要簽的 32 bytes：signing cell 的雜湊。
func (r *TONRequest) SigningHash() [32]byte { return r.signing.Hash() }

// Messages 回報這則 external message 裝了幾則付款 message。
func (r *TONRequest) Messages() int { return len(r.messages) }

// Body 回報第 i 筆付款的 transfer body。
func (r *TONRequest) Body(i int) *boc.Cell { return r.bodies[i] }

// Message 回報第 i 筆付款那則 internal message（MessageRelaxed）。
func (r *TONRequest) Message(i int) *boc.Cell { return r.messages[i] }

// Size 回報 signing cell 序列化成 BoC 之後有幾個 bytes。真的送出去的 external message 比它多一個
// 512 bits 的簽名與一段 external message 表頭，bulk 的 bytes 規則估的是後者，見 limits.go。
func (r *TONRequest) Size() int { return len(r.signing.ToBoC()) }

// Stats 回報 signing cell 那棵樹有幾個 cell、幾層深。深度是 OutList 那條鏈結串列的長度加上
// 每一則 message 自己的三層，鏈對 external message 的上限是 512 層。
func (r *TONRequest) Stats() boc.Stats { return r.signing.Count() }

// Hop 是一筆 jetton 付款在鏈上會經過的一步：哪個帳戶收到帶哪個 op 的 message、它會在那裡變成
// 什麼交易。四步的順序是 TEP-74 與參考實作定的，鏈下追一筆付款走到哪，對的就是這張表。
type Hop struct {
	Op   uint64
	Name string
	// From 與 To 是角色，不是地址：merchant 的 jetton wallet 地址鏈下組 message 時根本不知道。
	From, To string
	// Bounce 記這一步的 message 是不是 bounceable：是的話對面失敗會把 message（與剩下的 TON）退回來，
	// 這是 TON 上唯一的「失敗回報」。
	Bounce bool
}

// TONHops 是一則 transfer message 從錢包出發之後的四步。前兩步 bounceable：對面拒收就退回來，
// 錢沒有離開；後兩步是參考實作刻意不 bounce 的（merchant 本人可能是一個沒部署的地址），
// 送不到也不影響已經入帳的 jetton。
func TONHops() []Hop {
	return []Hop{
		{Op: TONOpTransfer, Name: "transfer", From: "wallet", To: "our jetton wallet", Bounce: true},
		{Op: TONOpInternalTransfer, Name: "internal_transfer", From: "our jetton wallet", To: "merchant's jetton wallet", Bounce: true},
		{Op: TONOpTransferNotification, Name: "transfer_notification", From: "merchant's jetton wallet", To: "merchant"},
		{Op: TONOpExcesses, Name: "excesses", From: "merchant's jetton wallet", To: "wallet"},
	}
}

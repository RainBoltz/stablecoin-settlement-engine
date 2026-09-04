package chain_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"math/big"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/boc"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// 這個檔案的 golden（signing hash、BoC 長度、深度、body 與 message 的雜湊）是用 @ton/ton 16.3.0 的
// createWalletTransferV5R1 加 @ton/core 0.63.1 組同一批付款、剝掉尾端 512 bits 的簽名之後算出來的；
// contracts/ton/golden.mjs 會重算一次（make ton-test）。跑出來不一樣是這裡的版面錯了，要修程式碼，不要改 golden。
// 12 與 255 筆的雜湊在 @ton/ton 15.x 上會不同：那時的 OutList 是反著包的，送出順序是倒的，見 tonmsg.go。

// tonRun 造一份 n 筆的名單，merchant 用 raw 寫法的 TON 地址；ref 的算法跟 buildRun 一樣，
// 所以同一筆 intent 在四條鏈上的 ref 都是同一把。
func tonRun(n int) []bulk.Payout {
	items := make([]bulk.Payout, 0, n)
	for i := 1; i <= n; i++ {
		merchant := fmt.Sprintf("0:%060x%04x", 10, i)
		items = append(items, bulk.Payout{
			Ref: paymentref.Derive(paymentref.Terms{
				IntentID: fmt.Sprintf("pi_%04d", i),
				Chain:    "payout-run/2026-09",
				Token:    "USDC",
				Payer:    "platform",
				Merchant: merchant,
				Amount:   "100000000",
			}),
			Merchant: merchant,
			Amount:   big.NewInt(100_000_000),
		})
	}
	return items
}

// tonAccounts 是測試與 Example 共用的兩個帳戶：簽名的錢包與它名下這顆 jetton 的 jetton wallet。
func tonAccounts() chain.TONAccounts {
	w, _ := boc.ParseAddress("0:1111111111111111111111111111111111111111111111111111111111111111")
	jw, _ := boc.ParseAddress("0:2222222222222222222222222222222222222222222222222222222222222222")
	return chain.TONAccounts{Wallet: w, JettonWallet: jw}
}

func hexOf(h [32]byte) string { return hex.EncodeToString(h[:]) }

// 防的情境：W5 的版面錯一個 bit（opcode、wallet_id 的正負、OutList 的方向、那個 extended
// action 的 0 bit），錢包合約驗出來的雜湊就不是 relayer 簽的那個，整批 message 一則都送不出去。
// 三個規模的 signing hash 都對 @ton/ton 釘死。
func TestTransferRequest_GoldenSigningHashAgainstTonTon(t *testing.T) {
	cases := []struct {
		n     int
		hash  string
		size  int
		depth int
	}{
		{1, "11477c77b380ddd89d666ec1301f8804aba66bda80f3a340973c6a5a87cd4d24", 227, 4},
		{12, "ea0b3151dc289944aaa01872a7ce4713d204e00203eb5ad3323e868746b6a175", 2318, 15},
		{255, "d1dbacd41cb2ca51e5488d6593eff5b4670f69f16b4ae61694324dcc4dd63ed1", 49513, 258},
	}
	for _, tc := range cases {
		req, err := chain.NewTON().TransferRequest(tonAccounts(), 41, 1_800_000_300, tonRun(tc.n))
		if err != nil {
			t.Fatalf("TransferRequest(%d): %v", tc.n, err)
		}
		if got := hexOf(req.SigningHash()); got != tc.hash {
			t.Fatalf("%d payouts: signing hash = %s, want %s", tc.n, got, tc.hash)
		}
		if got := req.Size(); got != tc.size {
			t.Fatalf("%d payouts: boc = %d bytes, want %d", tc.n, got, tc.size)
		}
		if st := req.Stats(); st.Depth != tc.depth {
			t.Fatalf("%d payouts: depth = %d, want %d", tc.n, st.Depth, tc.depth)
		}
		if req.Messages() != tc.n {
			t.Fatalf("%d payouts: Messages() = %d", tc.n, req.Messages())
		}
	}
}

// 防的情境：transfer body 的欄位順序或編碼跟 TEP-74 對不上。body 與包著它的 MessageRelaxed
// 各釘一個雜湊，然後照規格逐欄位讀回來：query_id 是 ref 的前 8 個 bytes、forward 是 1 nanoton、
// forward_payload 走 ref 那一邊、裡面是我們的 op 加原封不動的 32 bytes。
func TestTONTransferBody_ReadsBackByTEP74(t *testing.T) {
	items := tonRun(1)
	acc := tonAccounts()
	req, err := chain.NewTON().TransferRequest(acc, 41, 1_800_000_300, items)
	if err != nil {
		t.Fatalf("TransferRequest: %v", err)
	}
	body, msg := req.Body(0), req.Message(0)
	if got := hexOf(body.Hash()); got != "f5ba54a3f91743ab0260b746f8f22dda28222eaf471becdd138bb262aa71204c" {
		t.Fatalf("body hash = %s", got)
	}
	if got := hexOf(msg.Hash()); got != "73a7772522003c31c50846ee396f847c4411e83444cd30ce14c82b16899adaf4" {
		t.Fatalf("message hash = %s", got)
	}

	s := body.Begin()
	if op := s.Uint(32); op != chain.TONOpTransfer {
		t.Fatalf("op = %#x, want transfer", op)
	}
	if q := s.Uint(64); q != binary.BigEndian.Uint64(items[0].Ref[:8]) {
		t.Fatalf("query_id = %#x, want the first 8 bytes of the ref", q)
	}
	if amt := s.Coins(); amt.Cmp(items[0].Amount) != 0 {
		t.Fatalf("amount = %s", amt)
	}
	if to := s.Address(); to.String() != items[0].Merchant {
		t.Fatalf("destination = %s, want %s", to, items[0].Merchant)
	}
	if resp := s.Address(); resp != acc.Wallet {
		t.Fatalf("response_destination = %s, want our wallet", resp)
	}
	if custom := s.MaybeRef(); custom != nil {
		t.Fatalf("custom_payload should be empty")
	}
	if fwd := s.Coins(); fwd.Int64() != chain.TONForward {
		t.Fatalf("forward_ton_amount = %s, want 1 nanoton", fwd)
	}
	if !s.Bit() {
		t.Fatalf("forward_payload should be in a ref")
	}
	payload := s.Ref().Begin()
	if op := payload.Uint(32); op != chain.TONPayloadOp {
		t.Fatalf("payload op = %#x, want %#x", op, chain.TONPayloadOp)
	}
	if ref := payload.Bytes(32); !bytes.Equal(ref, items[0].Ref[:]) {
		t.Fatalf("payload ref = %x, want %x", ref, items[0].Ref[:])
	}
	if s.Remaining() != 0 || s.RemainingRefs() != 0 || s.Err() != nil || payload.Remaining() != 0 || payload.Err() != nil {
		t.Fatalf("leftover bits or a short read: body %d/%d refs %v, payload %d %v",
			s.Remaining(), s.RemainingRefs(), s.Err(), payload.Remaining(), payload.Err())
	}

	// message 表頭：0x18 那 6 個 bit（internal message、ihr 關、bounce 開、不是 bounced、沒有 src）、
	// 收件人是我們的 jetton wallet、附 0.05 TON。
	m := msg.Begin()
	if hdr := m.Uint(6); hdr != 0b011000 {
		t.Fatalf("header bits = %06b, want 011000", hdr)
	}
	if dst := m.Address(); dst != acc.JettonWallet {
		t.Fatalf("dest = %s, want our jetton wallet", dst)
	}
	if v := m.Coins(); v.Int64() != chain.TONAttach {
		t.Fatalf("value = %s, want 0.05 TON", v)
	}
}

// 防的情境：ref 被改了編碼（轉成文字、截短、換順序），listener 在鏈上就找不到它。
// 跟 EVM 與 Solana 那條共同義務一樣：名單上每一把 ref 都要一字節不差地在輸出裡剛好出現一次。
func TestTransferRequest_EveryRefRidesOnceInTheBoC(t *testing.T) {
	items := tonRun(12)
	req, err := chain.NewTON().TransferRequest(tonAccounts(), 41, 1_800_000_300, items)
	if err != nil {
		t.Fatalf("TransferRequest: %v", err)
	}
	out := req.Cell().ToBoC()
	for i, it := range items {
		if n := bytes.Count(out, it.Ref[:]); n != 1 {
			t.Fatalf("ref %d appears %d times in the BoC, want once", i, n)
		}
	}
}

// 防的情境：bulk 的 bytes 規則估得比真的長度低。它估的是簽好之後的整則 external message，所以比 signing
// cell 的 BoC 多一個 512 bits 的簽名（64）與一段 external message 表頭（35）；估計要在真值之上，
// 而且高估的部分有上限：規則照 cell 超過 255 個的情況算，那時每個 ref 的索引占兩個 byte、
// BoC 表頭也多 6 個 bytes，cell 少的時候一則 message 四個 ref 各省一個 byte，所以最多高估 4n + 6。
func TestTransferRequest_BytesRuleStaysAboveTheRealSize(t *testing.T) {
	var rule bulk.Rule
	for _, r := range bulk.Defaults()["ton"].Rules {
		if r.Unit == "bytes" {
			rule = r
		}
	}
	if rule.Unit == "" {
		t.Fatalf("the ton limits have no bytes rule")
	}
	for _, n := range []int{1, 12, 63, 64, 100, 255} {
		req, err := chain.NewTON().TransferRequest(tonAccounts(), 41, 1_800_000_300, tonRun(n))
		if err != nil {
			t.Fatalf("TransferRequest(%d): %v", n, err)
		}
		real := req.Size() + 64 + 35
		est := int(rule.Base) + n*int(rule.Item)
		if est < real {
			t.Fatalf("%d payouts: bulk estimates %d bytes, the signed message is %d", n, est, real)
		}
		if est > real+4*n+6 {
			t.Fatalf("%d payouts: bulk estimates %d bytes for a %d-byte message, too loose", n, est, real)
		}
	}
}

// 防的情境：depth 規則跟真的樹對不上。OutList 是鏈結串列，一則 message 一層，
// 再加 signing cell 與最深那則 message 自己的三層。
func TestTransferRequest_DepthRuleMatchesTheTree(t *testing.T) {
	var rule bulk.Rule
	for _, r := range bulk.Defaults()["ton"].Rules {
		if r.Unit == "depth" {
			rule = r
		}
	}
	for _, n := range []int{1, 12, 255} {
		req, err := chain.NewTON().TransferRequest(tonAccounts(), 41, 1_800_000_300, tonRun(n))
		if err != nil {
			t.Fatalf("TransferRequest(%d): %v", n, err)
		}
		if got, want := req.Stats().Depth, int(rule.Base)+n*int(rule.Item); got != want {
			t.Fatalf("%d payouts: depth = %d, the rule says %d", n, got, want)
		}
	}
}

// 防的情境：一份 300 筆的名單被裝進同一則 external message。bulk 照 messages 那條切成 255 加 45，
// 每一批各自組得出來、各自在三條上限之內；第 256 則直接被 builder 拒絕。
func TestTransferRequest_PackCutsAtTheMessagesCap(t *testing.T) {
	items := tonRun(300)
	plan, err := bulk.Pack(items, bulk.Defaults()["ton"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(plan.Batches) != 2 || len(plan.Batches[0].Items) != 255 || len(plan.Batches[1].Items) != 45 {
		t.Fatalf("300 payouts packed into %d batches, want 255+45", len(plan.Batches))
	}
	if len(plan.Prepare) != 0 || plan.Rent != 0 {
		t.Fatalf("ton should need no prepare batch and no rent, got %d prepare batches, rent %d", len(plan.Prepare), plan.Rent)
	}
	for _, b := range plan.Batches {
		req, err := chain.NewTON().TransferRequest(tonAccounts(), uint32(40+b.Index), 1_800_000_300, b.Items)
		if err != nil {
			t.Fatalf("batch %d: %v", b.Index, err)
		}
		for _, u := range b.Used {
			if u.Used > u.Cap {
				t.Fatalf("batch %d: %s %d over the cap %d", b.Index, u.Unit, u.Used, u.Cap)
			}
		}
		if req.Size()+64+35 > 65535 || req.Stats().Depth > 512 {
			t.Fatalf("batch %d: %d bytes, %d deep", b.Index, req.Size(), req.Stats().Depth)
		}
	}
	if _, err := chain.NewTON().TransferRequest(tonAccounts(), 41, 1_800_000_300, tonRun(256)); !errors.Is(err, chain.ErrTooManyMessages) {
		t.Fatalf("256 payouts: %v, want ErrTooManyMessages", err)
	}
}

// 防的情境：壞掉的輸入被組成一則一定會被拒的 message。每一種都要在組的時候就擋下來，
// 而且錯誤要指得出是第幾筆。
func TestTransferRequest_RefusesWhatTheChainWouldReject(t *testing.T) {
	acc := tonAccounts()
	build := func(items []bulk.Payout) error {
		_, err := chain.NewTON().TransferRequest(acc, 41, 1_800_000_300, items)
		return err
	}
	if err := build(nil); !errors.Is(err, chain.ErrEmptyBatch) {
		t.Fatalf("empty: %v", err)
	}
	items := tonRun(2)
	items[1].Ref = paymentref.Ref{}
	if err := build(items); !errors.Is(err, chain.ErrZeroRef) || err.Error()[:8] != "payout 1" {
		t.Fatalf("zero ref: %v", err)
	}
	items = tonRun(1)
	items[0].Amount = big.NewInt(0)
	if err := build(items); !errors.Is(err, chain.ErrBadAmount) {
		t.Fatalf("zero amount: %v", err)
	}
	items = tonRun(1)
	items[0].Merchant = "0x1000000000000000000000000000000000000001"
	if err := build(items); !errors.Is(err, chain.ErrBadTONAddress) {
		t.Fatalf("evm address on ton: %v", err)
	}
	if _, err := chain.NewTON().TransferRequest(chain.TONAccounts{Wallet: acc.Wallet}, 41, 1_800_000_300, tonRun(1)); !errors.Is(err, chain.ErrZeroAccount) {
		t.Fatalf("no jetton wallet: %v", err)
	}
}

// 防的情境：有人改了 payload 的 TL-B 字串卻沒改 op，或反過來。op 照 TEP-74 的慣例是宣告字串的
// crc32 去掉最高位，這裡把數值本身也釘住，鏈上任何一個已經在認它的人才不會被換掉。
func TestTONPayloadOp_IsTheCRC32OfItsScheme(t *testing.T) {
	want := uint64(crc32.ChecksumIEEE([]byte("payment_ref ref:bits256 = ForwardPayload")) & 0x7fffffff)
	if chain.TONPayloadOp != want || chain.TONPayloadOp != 0x3121432c {
		t.Fatalf("TONPayloadOp = %#x, want %#x (0x3121432c)", chain.TONPayloadOp, want)
	}
}

// 防的情境：有人把四步的順序或 bounce 旗標改掉。前兩步 bounceable 是錢退得回來的唯一保證，
// 後兩步刻意不 bounce，因為 merchant 本人可能是一個沒部署的地址。
func TestTONHops_FourStepsTwoBounceable(t *testing.T) {
	hops := chain.TONHops()
	if len(hops) != 4 {
		t.Fatalf("%d hops, want 4", len(hops))
	}
	ops := []uint64{chain.TONOpTransfer, chain.TONOpInternalTransfer, chain.TONOpTransferNotification, chain.TONOpExcesses}
	for i, h := range hops {
		if h.Op != ops[i] {
			t.Fatalf("hop %d op = %#x, want %#x", i, h.Op, ops[i])
		}
		if h.Bounce != (i < 2) {
			t.Fatalf("hop %d (%s) bounce = %v", i, h.Name, h.Bounce)
		}
	}
}

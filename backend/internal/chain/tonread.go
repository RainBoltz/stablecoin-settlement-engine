package chain

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/boc"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/listener"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/recon"
)

// tonread.go 是 TON adapter 讀鏈的那一半：listener 的 Watcher 與 recon 的 Source。
//
// 前兩條鏈上這一半很薄：拿 tx hash 問一筆交易、拿一段高度撈 event。TON 上薄不起來，因為
// 「這筆付款的交易」根本不是一筆。relayer 手上只有它送出去的那則 external message 的雜湊；那則 message 讓錢包
// 跑了一筆交易，但錢包交易成功只代表 seqno 用掉了、N 則付款 message 送出去了。錢有沒有到，要沿著
// message 一路追到 merchant 的 jetton wallet 那筆交易（見 TONHops），而每一步都可能還沒發生、
// 或者失敗了以一則 bounced message 退回。所以這裡先把一筆付款的路追成一個 TONTrace，
// 再把 trace 翻譯成 listener 看得懂的 Observation：
//
//   - Included：錢包收下了那則 external message（seqno 用掉了）。從這一刻起 message 不會憑空消失，
//     只會晚到，所以 Included 之後永遠不會變成 lost；沒被收下的 external message 過了 valid_until 就真的沒了。
//   - Height 與 Final：看最後一步那筆交易被哪個 masterchain block 引用，不看錢包那筆。
//     錢包交易在 masterchain 101 就不可逆了，錢可能要到 103 才入帳，對帳的 window 認的是後者。
//   - Succeeded：最後一步是 merchant 的 jetton wallet 把 internal_transfer 執行成功。
//     任何一步 bounce 回來都是失敗，退回來的那筆交易就是終點。
//
// recon 的 Source 反過來從 merchant 的 jetton wallet 出發：只認 internal_transfer 執行成功那筆交易，
// 因為只有它會加 merchant 的餘額。同一把 ref 在一條 trace 上會出現三次（transfer、
// internal_transfer、transfer_notification），掃我們自己錢包送出去的 message 會把一筆付款數成三筆、
// 把一筆 bounce 回來的付款數成付了；錨在收款那一步，每一筆付款剛好算一次。

// TONMessage 是鏈上一則 message 的紀錄：external message 沒有 Src、沒有 Value；bounced 的 message Bounced 為 true，
// body 開頭是 0xffffffff 接原 message body 的前 256 個 bit。
type TONMessage struct {
	// Hash 是這則 message cell 的雜湊，也是它的身分：交易靠它認「我處理的是哪一則」。
	Hash    [32]byte
	Src     boc.Address
	Dst     boc.Address
	Value   *big.Int
	Bounce  bool
	Bounced bool
	Body    *boc.Cell
}

// TONTransaction 是一個帳戶處理一則進來的 message 留下的紀錄。
type TONTransaction struct {
	Account boc.Address
	// LT 是這個帳戶自己的邏輯時間，同一個帳戶的交易照它嚴格遞增。
	LT   uint64
	Hash [32]byte
	In   *TONMessage
	Out  []*TONMessage
	// Aborted：compute phase 或 action phase 失敗。Bounce 開著的進來 message 會以 bounced message 退回。
	Aborted  bool
	ExitCode int
	// Masterchain 是引用了這筆交易所在 shard block 的 masterchain seqno；0 代表還沒有被引用，
	// 也就是還不算不可逆（「Once a transaction from a shardchain appears in a masterchain block,
	// it becomes irreversible」，https://docs.ton.org/blockchain-basics/payments/overview）。
	Masterchain uint64
}

// TONNode 是讀鏈的那一半要問節點的三件事。今天只有測試用的 fake；接真的鏈時對應的是
// 節點對帳戶交易與 masterchain 的查詢。
type TONNode interface {
	// Masterchain 回報節點目前看到的最新 masterchain seqno：listener 的 Head、recon 的 Finalized。
	Masterchain(ctx context.Context) (uint64, error)
	// TransactionByInMsg 找出處理了這則 message 的那筆交易；還沒有人處理它就回 nil。
	TransactionByInMsg(ctx context.Context, msg [32]byte) (*TONTransaction, error)
	// Transactions 回報一個帳戶被 [from, to] 這段 masterchain seqno 引用的交易，照 LT 排。
	Transactions(ctx context.Context, account boc.Address, from, to uint64) ([]*TONTransaction, error)
}

var (
	// ErrNotOurRequest：intent 身上的雜湊不是一則我們認得的 external message（讀不出 hex，或錢包交易的
	// 送出清單裡沒有帶這筆 intent 的 ref 的 message）。這是資料壞了，不是鏈的狀態，所以是 error。
	ErrNotOurRequest = errors.New("chain: the intent does not point at a request carrying its ref")
)

// TONOutcome 是追完一條 trace 之後的結論。
type TONOutcome string

const (
	// TONNotAccepted：錢包還沒收下那則 external message。可能還在路上，也可能已經過了 valid_until。
	TONNotAccepted TONOutcome = "not accepted"
	// TONInFlight：錢包收下了，但某一步的 message 還沒有被對面處理。
	TONInFlight TONOutcome = "in flight"
	// TONDelivered：merchant 的 jetton wallet 把 internal_transfer 執行成功，餘額加上去了。
	TONDelivered TONOutcome = "delivered"
	// TONBounced：merchant 的 jetton wallet 拒收，internal_transfer 以 bounced message 退回，
	// 我們的 jetton wallet 把餘額加回去了。jetton 出去又回來，TON 燒掉一點。
	TONBounced TONOutcome = "bounced"
	// TONRejected：我們自己的 jetton wallet 就拒收了 transfer（jetton 不夠、不是 owner），
	// 附上的 TON 以 bounced message 退回錢包。jetton 沒有離開過。
	TONRejected TONOutcome = "rejected"
)

// TONStep 是 trace 上的一步：哪個角色、處理了哪一種 message、留下哪筆交易。Tx 為 nil 代表這一步還沒發生。
type TONStep struct {
	Role string
	Op   string
	Tx   *TONTransaction
}

// String 印一步：角色、masterchain seqno（還沒被引用印 -，還沒發生印 …）、失敗的話帶 exit code。
func (s TONStep) String() string {
	if s.Tx == nil {
		return s.Role + " …"
	}
	where := "-"
	if s.Tx.Masterchain != 0 {
		where = fmt.Sprint(s.Tx.Masterchain)
	}
	if s.Tx.Aborted {
		return fmt.Sprintf("%s %s aborted (exit %d)", s.Role, where, s.Tx.ExitCode)
	}
	return s.Role + " " + where
}

// TONTrace 是一筆付款從 external message 出發追到的每一步，以及最後的結論。
type TONTrace struct {
	Steps    []TONStep
	Outcome  TONOutcome
	Terminal *TONTransaction
	Received *big.Int
}

// String 印整條 trace 成一行，箭頭串起每一步，最後是結論。
func (t TONTrace) String() string {
	parts := make([]string, 0, len(t.Steps))
	for _, s := range t.Steps {
		parts = append(parts, s.String())
	}
	line := strings.Join(parts, " -> ")
	if t.Received != nil {
		return fmt.Sprintf("%s   %s %s", line, t.Outcome, t.Received)
	}
	return fmt.Sprintf("%s   %s", line, t.Outcome)
}

// TONReader 是 TON adapter 讀鏈的那一半。它認得我們的錢包與 jetton wallet（追 trace 的起點），
// 以及一份 merchant 對 jetton wallet 的名單（recon 掃的帳戶）。
type TONReader struct {
	node    TONNode
	acc     TONAccounts
	token   string
	watched map[boc.Address]string
}

// NewTONReader 建立讀鏈的那一半。token 是這顆 jetton 在 intent 上寫的名字，回報的 Transfer 帶著它。
func NewTONReader(node TONNode, acc TONAccounts, token string) *TONReader {
	return &TONReader{node: node, acc: acc, token: token, watched: make(map[boc.Address]string)}
}

// Watch 把一個 merchant 的 jetton wallet 加進 recon 要掃的名單。merchant 的 jetton wallet 是
// 從 jetton master 算出來的（get_wallet_address），鏈下組 message 時不需要它，對帳時需要：
// 錢入帳的那筆交易發生在這個帳戶上，不在我們任何一個帳戶上。
func (r *TONReader) Watch(merchant string, jettonWallet boc.Address) {
	r.watched[jettonWallet] = merchant
}

// Trace 從一則 external message 的雜湊追一筆付款：錢包收了沒、我們的 jetton wallet 送出去了沒、
// merchant 的 jetton wallet 收了沒，任何一步 bounce 回來就追那則 bounced message 落地的交易。
func (r *TONReader) Trace(ctx context.Context, external [32]byte, ref paymentref.Ref) (TONTrace, error) {
	tr := TONTrace{Outcome: TONNotAccepted}
	wallet, err := r.node.TransactionByInMsg(ctx, external)
	if err != nil {
		return tr, err
	}
	if wallet == nil {
		return tr, nil
	}
	tr.Steps = append(tr.Steps, TONStep{Role: "wallet", Op: "external", Tx: wallet})
	transfer := findTransfer(wallet.Out, ref)
	if transfer == nil {
		return tr, fmt.Errorf("%w: %s", ErrNotOurRequest, ref)
	}

	// 第一步：我們的 jetton wallet 處理 transfer。
	tr.Outcome = TONInFlight
	ours, err := r.node.TransactionByInMsg(ctx, transfer.Hash)
	if err != nil {
		return tr, err
	}
	tr.Steps = append(tr.Steps, TONStep{Role: "our jetton wallet", Op: "transfer", Tx: ours})
	if ours == nil {
		return tr, nil
	}
	if ours.Aborted {
		return r.bounceBack(ctx, tr, ours, "wallet", TONRejected)
	}

	// 第二步：merchant 的 jetton wallet 處理 internal_transfer。它成功，錢就到了。
	internal := findOp(ours.Out, TONOpInternalTransfer)
	if internal == nil {
		return tr, fmt.Errorf("%w: our jetton wallet emitted no internal_transfer", ErrNotOurRequest)
	}
	theirs, err := r.node.TransactionByInMsg(ctx, internal.Hash)
	if err != nil {
		return tr, err
	}
	tr.Steps = append(tr.Steps, TONStep{Role: "merchant's jetton wallet", Op: "internal_transfer", Tx: theirs})
	if theirs == nil {
		return tr, nil
	}
	if theirs.Aborted {
		return r.bounceBack(ctx, tr, theirs, "our jetton wallet", TONBounced)
	}
	amount, _, _, err := tonInternalTransfer(internal.Body)
	if err != nil {
		return tr, err
	}
	tr.Outcome, tr.Terminal, tr.Received = TONDelivered, theirs, amount
	return tr, nil
}

// bounceBack 追一則失敗 message 的退路：失敗的那筆交易會送出一則 bounced message，它落地的那筆交易
// 才是這筆付款的終點（錢在那裡回到原位）。
func (r *TONReader) bounceBack(ctx context.Context, tr TONTrace, failed *TONTransaction, role string, outcome TONOutcome) (TONTrace, error) {
	var bounced *TONMessage
	for _, m := range failed.Out {
		if m.Bounced {
			bounced = m
		}
	}
	if bounced == nil {
		// 沒有東西退回來（附的 TON 不夠付退信）：失敗那筆交易自己就是終點。
		tr.Outcome, tr.Terminal = outcome, failed
		return tr, nil
	}
	landed, err := r.node.TransactionByInMsg(ctx, bounced.Hash)
	if err != nil {
		return tr, err
	}
	tr.Steps = append(tr.Steps, TONStep{Role: role, Op: "bounced", Tx: landed})
	if landed == nil {
		return tr, nil
	}
	tr.Outcome, tr.Terminal = outcome, landed
	return tr, nil
}

// Lookup 實作 listener.Watcher：追一條 trace，翻譯成 Observation。
func (r *TONReader) Lookup(ctx context.Context, it *intent.Intent) (listener.Sighting, error) {
	var s listener.Sighting
	head, err := r.node.Masterchain(ctx)
	if err != nil {
		return s, err
	}
	s.Head = head
	external, err := parseHash(it.TxHash)
	if err != nil {
		return s, fmt.Errorf("%w: %v", ErrNotOurRequest, err)
	}
	tr, err := r.Trace(ctx, external, it.Ref)
	if err != nil {
		return s, err
	}
	if tr.Outcome == TONNotAccepted {
		return s, nil
	}
	s.Included = true
	// Height 是「目前追到的最後一步」被引用的 masterchain seqno；還在路上就先報最後一筆看得到的交易。
	for _, st := range tr.Steps {
		if st.Tx != nil && st.Tx.Masterchain > s.Height {
			s.Height = st.Tx.Masterchain
		}
	}
	if tr.Terminal == nil {
		return s, nil
	}
	s.Height = tr.Terminal.Masterchain
	s.Final = tr.Terminal.Masterchain != 0
	s.Succeeded = tr.Outcome == TONDelivered
	if s.Succeeded {
		s.Received = new(big.Int).Set(tr.Received)
	}
	return s, nil
}

// Finalized 實作 recon.Source：最新的 masterchain seqno。被它引用的交易都不可逆了。
func (r *TONReader) Finalized(ctx context.Context) (uint64, error) {
	return r.node.Masterchain(ctx)
}

// Transfers 實作 recon.Source：掃名單上每個 merchant 的 jetton wallet，只收「internal_transfer
// 執行成功」的那筆交易。收款人是誰由帳戶決定（這個 jetton wallet 屬於哪個 merchant），
// 付款人是 internal_transfer 裡的 from，ref 從 forward_payload 讀，讀不到就是一筆沒帶 ref 的入帳。
func (r *TONReader) Transfers(ctx context.Context, from, to uint64) ([]recon.Transfer, error) {
	accounts := make([]boc.Address, 0, len(r.watched))
	for a := range r.watched {
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].String() < accounts[j].String() })

	var out []recon.Transfer
	for _, a := range accounts {
		txs, err := r.node.Transactions(ctx, a, from, to)
		if err != nil {
			return nil, err
		}
		for _, tx := range txs {
			if tx.Aborted || tx.In == nil || tx.In.Bounced || tx.In.Body == nil || tx.Masterchain == 0 {
				continue
			}
			if op := tx.In.Body.Begin().Uint(32); op != TONOpInternalTransfer {
				continue
			}
			amount, sender, ref, err := tonInternalTransfer(tx.In.Body)
			if err != nil {
				return nil, err
			}
			out = append(out, recon.Transfer{
				TxHash: hex.EncodeToString(tx.Hash[:]),
				Height: tx.Masterchain,
				Ref:    ref,
				Token:  r.token,
				From:   sender.String(),
				To:     r.watched[a],
				Amount: amount,
			})
		}
	}
	return out, nil
}

// findTransfer 在錢包送出去的 message 裡找帶著這把 ref 的 transfer。
func findTransfer(out []*TONMessage, ref paymentref.Ref) *TONMessage {
	for _, m := range out {
		if m.Body == nil || m.Bounced {
			continue
		}
		s := m.Body.Begin()
		if s.Uint(32) != TONOpTransfer {
			continue
		}
		s.Uint(64)
		s.Coins()
		s.Address()
		s.Address()
		s.MaybeRef()
		s.Coins()
		if got, ok := tonForwardRef(s); ok && got == ref {
			return m
		}
	}
	return nil
}

// findOp 在一筆交易送出去的 message 裡找第一則帶這個 op 的、不是 bounced 的 message。
func findOp(out []*TONMessage, op uint64) *TONMessage {
	for _, m := range out {
		if m.Body == nil || m.Bounced {
			continue
		}
		if m.Body.Begin().Uint(32) == op {
			return m
		}
	}
	return nil
}

// tonInternalTransfer 讀一則 internal_transfer：金額、from（付款人的錢包）、forward_payload 裡的 ref
// （不是我們的 payload 就回零值 ref）。
func tonInternalTransfer(body *boc.Cell) (*big.Int, boc.Address, paymentref.Ref, error) {
	s := body.Begin()
	if s.Uint(32) != TONOpInternalTransfer {
		return nil, boc.Address{}, paymentref.Ref{}, fmt.Errorf("chain: not an internal_transfer body")
	}
	s.Uint(64)
	amount := s.Coins()
	from := s.Address()
	s.Address()
	s.Coins()
	ref, _ := tonForwardRef(s)
	if s.Err() != nil {
		return nil, boc.Address{}, paymentref.Ref{}, s.Err()
	}
	return amount, from, ref, nil
}

// tonForwardRef 從 forward_payload 讀我們的 ref：Either 走 ref 那一邊、op 對得上、後面剛好 32 bytes。
// 任何一點不符就是「沒帶 ref」，不是錯誤：別人的 payload 長什麼樣不歸我們管。
func tonForwardRef(s *boc.Slice) (paymentref.Ref, bool) {
	var ref paymentref.Ref
	if !s.Bit() {
		return ref, false
	}
	p := s.Ref()
	if s.Err() != nil || p == nil {
		return ref, false
	}
	ps := p.Begin()
	if ps.Uint(32) != TONPayloadOp {
		return ref, false
	}
	copy(ref[:], ps.Bytes(32))
	if ps.Err() != nil || ps.Remaining() != 0 {
		return paymentref.Ref{}, false
	}
	return ref, true
}

// parseHash 讀 intent 上那串 hex 的雜湊：64 個 hex 字元，可以帶 0x。
func parseHash(s string) ([32]byte, error) {
	var h [32]byte
	s = strings.TrimPrefix(s, "0x")
	if len(s) != 64 {
		return h, fmt.Errorf("not a 32-byte hex hash: %q", s)
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return h, err
	}
	return h, nil
}

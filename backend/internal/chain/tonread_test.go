package chain_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/boc"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/ledger"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/listener"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// fakeTON 是測試用的一小段 TON：一個錢包、我們的 jetton wallet、每個 merchant 各一個 jetton wallet，
// message 照參考實作 jetton-wallet.fc 的規則處理（https://github.com/ton-blockchain/token-contract/blob/main/ft/jetton-wallet.fc）：
// transfer 讓我們的 jetton wallet 扣餘額、送 internal_transfer；internal_transfer 讓對面加餘額、
// 送 transfer_notification 與 excesses；失敗的那一步把 message 以 bounced message 退回，
// 退回 internal_transfer 的那一步把餘額加回去（on_bounce）。
//
// 它一次處理一輪：queue 裡現在有的 message 全部各自變成一筆交易，新產生的 message 留到下一輪；
// seal 把這一輪的交易標成被某個 masterchain block 引用。哪一則要失敗、哪一則要卡在路上，
// 用 query_id（也就是 ref 的前 8 個 bytes）指名。
type fakeTON struct {
	acc      chain.TONAccounts
	mc       uint64
	lt       uint64
	txs      []*chain.TONTransaction
	byIn     map[[32]byte]*chain.TONTransaction
	queue    []*chain.TONMessage
	balances map[boc.Address]*big.Int
	owner    map[boc.Address]boc.Address
	rejectAt map[uint64]int
	failAt   map[uint64]int
	delay    map[uint64]bool
}

func newFakeTON(acc chain.TONAccounts, funded *big.Int) *fakeTON {
	f := &fakeTON{
		acc:      acc,
		mc:       100,
		byIn:     make(map[[32]byte]*chain.TONTransaction),
		balances: map[boc.Address]*big.Int{acc.JettonWallet: new(big.Int).Set(funded)},
		owner:    map[boc.Address]boc.Address{acc.JettonWallet: acc.Wallet},
		rejectAt: make(map[uint64]int),
		failAt:   make(map[uint64]int),
		delay:    make(map[uint64]bool),
	}
	return f
}

// jettonWalletOf 是 merchant 的 jetton wallet：真的鏈上由 jetton master 的程式碼算，這裡用雜湊代替。
func jettonWalletOf(owner boc.Address) boc.Address {
	return boc.Address{Hash: sha256.Sum256(append([]byte("jetton-wallet:"), owner.Hash[:]...))}
}

func (f *fakeTON) Masterchain(context.Context) (uint64, error) { return f.mc, nil }

func (f *fakeTON) TransactionByInMsg(_ context.Context, msg [32]byte) (*chain.TONTransaction, error) {
	return f.byIn[msg], nil
}

func (f *fakeTON) Transactions(_ context.Context, account boc.Address, from, to uint64) ([]*chain.TONTransaction, error) {
	var out []*chain.TONTransaction
	for _, tx := range f.txs {
		if tx.Account == account && tx.Masterchain >= from && tx.Masterchain <= to {
			out = append(out, tx)
		}
	}
	return out, nil
}

// send 把一則簽好的請求交給錢包：錢包那筆交易只做一件事，把 N 則付款 message 放進 queue。
// external message 的雜湊在真的鏈上是簽完之後那個 cell 的雜湊，這裡用 signing hash 加一個前綴代替。
func (f *fakeTON) send(req *chain.TONRequest) [32]byte {
	h := req.SigningHash()
	external := sha256.Sum256(append([]byte("external:"), h[:]...))
	tx := f.newTx(f.acc.Wallet, &chain.TONMessage{Hash: external, Dst: f.acc.Wallet, Value: big.NewInt(0)})
	for i := 0; i < req.Messages(); i++ {
		m := req.Message(i)
		s := m.Begin()
		s.Uint(6)
		dst := s.Address()
		value := s.Coins()
		out := &chain.TONMessage{Hash: m.Hash(), Src: f.acc.Wallet, Dst: dst, Value: value, Bounce: true, Body: req.Body(i)}
		tx.Out = append(tx.Out, out)
		f.queue = append(f.queue, out)
	}
	return external
}

// step 處理 queue 裡現在有的每一則 message，各自變成一筆交易；被指名延後的留在 queue 裡。
func (f *fakeTON) step() {
	pending := f.queue
	f.queue = nil
	for _, m := range pending {
		if q, ok := queryID(m); ok && f.delay[q] && m.Dst != f.acc.JettonWallet {
			f.queue = append(f.queue, m)
			continue
		}
		f.process(m)
	}
}

// seal 把還沒被引用的交易全部標成被 mc 這個 masterchain block 引用。
func (f *fakeTON) seal(mc uint64) {
	for _, tx := range f.txs {
		if tx.Masterchain == 0 {
			tx.Masterchain = mc
		}
	}
	f.mc = mc
}

func (f *fakeTON) newTx(account boc.Address, in *chain.TONMessage) *chain.TONTransaction {
	f.lt++
	var seed []byte
	seed = append(seed, account.Hash[:]...)
	seed = binary.BigEndian.AppendUint64(seed, f.lt)
	tx := &chain.TONTransaction{Account: account, LT: f.lt, Hash: sha256.Sum256(seed), In: in}
	f.txs = append(f.txs, tx)
	f.byIn[in.Hash] = tx
	return tx
}

func (f *fakeTON) emit(tx *chain.TONTransaction, dst boc.Address, value *big.Int, bounce, bounced bool, body *boc.Cell) {
	seed := append([]byte("msg:"), tx.Hash[:]...)
	seed = append(seed, dst.Hash[:]...)
	seed = binary.BigEndian.AppendUint64(seed, uint64(len(tx.Out)))
	m := &chain.TONMessage{Hash: sha256.Sum256(seed), Src: tx.Account, Dst: dst, Value: value, Bounce: bounce, Bounced: bounced, Body: body}
	tx.Out = append(tx.Out, m)
	f.queue = append(f.queue, m)
}

// process 是一個帳戶處理一則 message：照 TEP-74 與參考實作決定這筆交易做什麼。
func (f *fakeTON) process(m *chain.TONMessage) {
	tx := f.newTx(m.Dst, m)
	if m.Body == nil {
		return
	}
	s := m.Body.Begin()
	op := s.Uint(32)
	switch {
	case m.Bounced:
		// 退回來的 internal_transfer：on_bounce 把餘額加回去（0xffffffff 之後是原 body 的開頭）。
		if op == 0xffffffff && s.Uint(32) == chain.TONOpInternalTransfer {
			s.Uint(64)
			f.balances[m.Dst].Add(f.balances[m.Dst], s.Coins())
		}
	case op == chain.TONOpTransfer && m.Dst == f.acc.JettonWallet:
		q := s.Uint(64)
		amount := s.Coins()
		to := s.Address()
		response := s.Address()
		s.MaybeRef()
		fwd := s.Coins()
		payload := s.Ref()
		if code, ok := f.rejectAt[q]; ok || f.balances[m.Dst].Cmp(amount) < 0 {
			if !ok {
				code = 706
			}
			f.abort(tx, m, code, chain.TONOpTransfer, q, amount)
			return
		}
		f.balances[m.Dst].Sub(f.balances[m.Dst], amount)
		dst := jettonWalletOf(to)
		f.owner[dst] = to
		if _, ok := f.balances[dst]; !ok {
			f.balances[dst] = new(big.Int)
		}
		body, _ := boc.NewBuilder().Uint(chain.TONOpInternalTransfer, 32).Uint(q, 64).Coins(amount).
			Address(f.owner[m.Dst]).Address(response).Coins(fwd).Bit(true).Ref(payload).Build()
		f.emit(tx, dst, new(big.Int).Set(m.Value), true, false, body)
	case op == chain.TONOpInternalTransfer:
		q := s.Uint(64)
		amount := s.Coins()
		from := s.Address()
		response := s.Address()
		fwd := s.Coins()
		s.Bit()
		payload := s.Ref()
		if code, ok := f.failAt[q]; ok {
			f.abort(tx, m, code, chain.TONOpInternalTransfer, q, amount)
			return
		}
		f.balances[m.Dst].Add(f.balances[m.Dst], amount)
		note, _ := boc.NewBuilder().Uint(chain.TONOpTransferNotification, 32).Uint(q, 64).Coins(amount).
			Address(from).Bit(true).Ref(payload).Build()
		f.emit(tx, f.owner[m.Dst], fwd, false, false, note)
		excess, _ := boc.NewBuilder().Uint(chain.TONOpExcesses, 32).Uint(q, 64).Build()
		f.emit(tx, response, new(big.Int).Sub(m.Value, fwd), false, false, excess)
	}
}

// abort 讓這筆交易失敗，並把進來的 message 以 bounced message 退回給寄件人：
// 0xffffffff 接原 body 的開頭（op、query_id、金額）。
func (f *fakeTON) abort(tx *chain.TONTransaction, m *chain.TONMessage, code int, op, q uint64, amount *big.Int) {
	tx.Aborted, tx.ExitCode = true, code
	if !m.Bounce {
		return
	}
	body, _ := boc.NewBuilder().Uint(0xffffffff, 32).Uint(op, 32).Uint(q, 64).Coins(amount).Build()
	f.emit(tx, m.Src, new(big.Int).Set(m.Value), false, true, body)
}

// queryID 讀一則 message body 的 query_id（我們把 ref 的前 8 個 bytes 放在這裡）。
func queryID(m *chain.TONMessage) (uint64, bool) {
	if m.Body == nil || m.Body.Bits() < 96 {
		return 0, false
	}
	s := m.Body.Begin()
	if s.Uint(32) == 0xffffffff {
		s.Uint(32)
	}
	return s.Uint(64), true
}

func qid(ref paymentref.Ref) uint64 { return binary.BigEndian.Uint64(ref[:8]) }

// tonScenario 送三筆付款，讓它們各走各的：第一筆到帳、第二筆在 merchant 的 jetton wallet 失敗
// 而退回來、第三筆的 internal_transfer 卡在路上。回傳 external message 的雜湊與名單。
func tonScenario(t *testing.T) (*fakeTON, *chain.TONReader, [32]byte, []bulk.Payout) {
	t.Helper()
	return tonScenarioFor(t, tonRun(3))
}

// tonScenarioFor 是同一個劇本，名單由呼叫端給（listener 的測試要用 intent 算出來的 ref）。
func tonScenarioFor(t *testing.T, items []bulk.Payout) (*fakeTON, *chain.TONReader, [32]byte, []bulk.Payout) {
	t.Helper()
	f, reader, external, err := playTON(items)
	if err != nil {
		t.Fatalf("playTON: %v", err)
	}
	return f, reader, external, items
}

// playTON 把三筆付款的劇本演一遍，四輪、四個 masterchain block：101 錢包收下請求，
// 102 我們的 jetton wallet 送出三則 internal_transfer，103 第一筆入帳、第二筆在 merchant 的
// jetton wallet 失敗、第三筆卡在路上，104 第二筆的 bounce 落回我們的 jetton wallet。
func playTON(items []bulk.Payout) (*fakeTON, *chain.TONReader, [32]byte, error) {
	acc := tonAccounts()
	f := newFakeTON(acc, big.NewInt(1_000_000_000))
	f.failAt[qid(items[1].Ref)] = 13
	f.delay[qid(items[2].Ref)] = true
	req, err := chain.NewTON().TransferRequest(acc, 41, 1_800_000_300, items)
	if err != nil {
		return nil, nil, [32]byte{}, err
	}
	external := f.send(req)
	f.seal(101)
	f.step()
	f.seal(102)
	f.step()
	f.seal(103)
	f.step()
	f.seal(104)
	reader := chain.NewTONReader(f, acc, "USDC")
	for _, it := range items {
		owner, _ := boc.ParseAddress(it.Merchant)
		reader.Watch(it.Merchant, jettonWalletOf(owner))
	}
	return f, reader, external, nil
}

// 防的情境：把錢包那筆交易當成「付款的交易」。錢包收下請求只代表 message 送出去了；
// 到帳的證據是三步之後 merchant 的 jetton wallet 那筆交易，trace 要一路追到它。
func TestTONReader_TraceFollowsAPaymentToTheMerchantJettonWallet(t *testing.T) {
	ctx := context.Background()
	_, reader, external, items := tonScenario(t)
	tr, err := reader.Trace(ctx, external, items[0].Ref)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if tr.Outcome != chain.TONDelivered || len(tr.Steps) != 3 {
		t.Fatalf("trace = %s", tr)
	}
	if tr.Terminal == nil || tr.Terminal.Account != jettonWalletOf(mustAddr(items[0].Merchant)) {
		t.Fatalf("terminal should be the merchant's jetton wallet transaction: %+v", tr.Terminal)
	}
	if tr.Received == nil || tr.Received.Cmp(items[0].Amount) != 0 {
		t.Fatalf("received = %s, want %s", tr.Received, items[0].Amount)
	}
	if tr.Steps[0].Tx.Masterchain != 101 || tr.Steps[1].Tx.Masterchain != 102 || tr.Steps[2].Tx.Masterchain != 103 {
		t.Fatalf("hops should land in 101, 102, 103: %s", tr)
	}
}

func mustAddr(s string) boc.Address {
	a, err := boc.ParseAddress(s)
	if err != nil {
		panic(err)
	}
	return a
}

// 防的情境：一步還沒發生就被當成失敗或成功。卡在路上的 message 會晚到，不會消失，
// 所以 trace 停在那一步、Observation 是「收下了但還沒不可逆」，listener 只能等。
func TestTONReader_TraceStopsAtTheHopThatHasNotHappened(t *testing.T) {
	ctx := context.Background()
	_, reader, external, items := tonScenario(t)
	tr, err := reader.Trace(ctx, external, items[2].Ref)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if tr.Outcome != chain.TONInFlight || len(tr.Steps) != 3 || tr.Steps[2].Tx != nil || tr.Terminal != nil {
		t.Fatalf("trace = %s", tr)
	}
	it := &intent.Intent{Ref: items[2].Ref, TxHash: hex.EncodeToString(external[:])}
	seen, err := reader.Lookup(ctx, it)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !seen.Included || seen.Final || seen.Succeeded || seen.Received != nil || seen.Height != 102 || seen.Head != 104 {
		t.Fatalf("observation = %+v, want included at 102, not final, head 104", seen.Observation)
	}
	v := finality.Defaults()["ton"].Judge(seen.Observation, time.Hour)
	if v.Kind != finality.KindPending {
		t.Fatalf("an accepted request never goes lost, got %s", v)
	}
}

// 防的情境：bounce 被當成沒事。merchant 的 jetton wallet 失敗，internal_transfer 退回來，
// 我們的 jetton wallet 把餘額加回去：終點是那筆 on_bounce 交易，結論是失敗，錢一毛沒少。
func TestTONReader_ABounceIsAFailureAndItsLandingIsTheTerminal(t *testing.T) {
	ctx := context.Background()
	f, reader, external, items := tonScenario(t)
	tr, err := reader.Trace(ctx, external, items[1].Ref)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if tr.Outcome != chain.TONBounced || len(tr.Steps) != 4 {
		t.Fatalf("trace = %s", tr)
	}
	if !tr.Steps[2].Tx.Aborted || tr.Steps[2].Tx.ExitCode != 13 {
		t.Fatalf("the merchant hop should be the aborted one: %s", tr)
	}
	if tr.Terminal == nil || tr.Terminal.Account != f.acc.JettonWallet || tr.Terminal.Aborted {
		t.Fatalf("terminal should be our jetton wallet's on_bounce transaction: %s", tr)
	}
	// 三筆付款各 100，一筆到帳、一筆退回、一筆在路上：我們的餘額是 1000 減 200，退回的那 100 回來了。
	if got := f.balances[f.acc.JettonWallet]; got.Int64() != 800_000_000 {
		t.Fatalf("our jetton wallet balance = %s, want 800000000 (the bounced 100 came back)", got)
	}
	it := &intent.Intent{Ref: items[1].Ref, TxHash: hex.EncodeToString(external[:])}
	seen, _ := reader.Lookup(ctx, it)
	if !seen.Included || !seen.Final || seen.Succeeded || seen.Height != 104 {
		t.Fatalf("observation = %+v, want final at 104 and not succeeded", seen.Observation)
	}
	if v := finality.Defaults()["ton"].Judge(seen.Observation, time.Minute); v.Kind != finality.KindFailed {
		t.Fatalf("verdict = %s, want failed", v)
	}
}

// 防的情境：我們自己的 jetton wallet 拒收（jetton 不夠、不是 owner）被當成錢出去了。
// transfer 退回錢包，jetton 從頭到尾沒離開，終點是錢包收到 bounced message 那筆交易。
func TestTONReader_ARejectedTransferNeverLeavesOurJettonWallet(t *testing.T) {
	ctx := context.Background()
	items := tonRun(1)
	acc := tonAccounts()
	f := newFakeTON(acc, big.NewInt(50_000_000)) // 餘額只有 50，付不出 100
	req, _ := chain.NewTON().TransferRequest(acc, 41, 1_800_000_300, items)
	external := f.send(req)
	f.seal(101)
	f.step()
	f.step()
	f.seal(102)
	reader := chain.NewTONReader(f, acc, "USDC")
	tr, err := reader.Trace(ctx, external, items[0].Ref)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if tr.Outcome != chain.TONRejected || len(tr.Steps) != 3 || tr.Steps[1].Tx.ExitCode != 706 {
		t.Fatalf("trace = %s", tr)
	}
	if tr.Terminal == nil || tr.Terminal.Account != acc.Wallet {
		t.Fatalf("terminal should be the wallet receiving the bounce: %s", tr)
	}
	if got := f.balances[acc.JettonWallet]; got.Int64() != 50_000_000 {
		t.Fatalf("balance = %s, want untouched 50000000", got)
	}
}

// 防的情境：一則沒被錢包收下的請求被當成「進區塊了」。找不到錢包交易就是 Included 為 false，
// 過了 LostAfter 交回 relayer 重簽，跟 EVM 上一筆從 mempool 消失的交易走同一條路。
func TestTONReader_NotAcceptedIsNotIncluded(t *testing.T) {
	ctx := context.Background()
	_, reader, _, items := tonScenario(t)
	unknown := sha256.Sum256([]byte("a request the wallet never saw"))
	it := &intent.Intent{Ref: items[0].Ref, TxHash: hex.EncodeToString(unknown[:])}
	seen, err := reader.Lookup(ctx, it)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if seen.Included || seen.Head != 104 {
		t.Fatalf("observation = %+v, want not included", seen.Observation)
	}
	p := finality.Defaults()["ton"]
	if v := p.Judge(seen.Observation, time.Minute); v.Kind != finality.KindPending {
		t.Fatalf("young: %s", v)
	}
	if v := p.Judge(seen.Observation, p.LostAfter); v.Kind != finality.KindLost {
		t.Fatalf("after LostAfter: %s", v)
	}
}

// 防的情境：Height 取錢包那筆交易的 masterchain seqno。錢包在 101 就不可逆了，錢在 103 才到帳，
// 在 103 被引用之前這筆付款不算 final；引用之後 Height 是 103 不是 101。
func TestTONReader_HeightIsTheLastHopNotTheWallet(t *testing.T) {
	ctx := context.Background()
	items := tonRun(1)
	acc := tonAccounts()
	f := newFakeTON(acc, big.NewInt(1_000_000_000))
	req, _ := chain.NewTON().TransferRequest(acc, 41, 1_800_000_300, items)
	external := f.send(req)
	f.seal(101)
	f.step()
	f.seal(102)
	f.step() // merchant 的 jetton wallet 收了，但這一輪還沒被 masterchain 引用
	reader := chain.NewTONReader(f, acc, "USDC")
	it := &intent.Intent{Ref: items[0].Ref, TxHash: hex.EncodeToString(external[:])}
	seen, _ := reader.Lookup(ctx, it)
	if !seen.Included || seen.Final || !seen.Succeeded || seen.Height != 0 {
		t.Fatalf("before the seal: %+v, want included, not final", seen.Observation)
	}
	f.seal(103)
	seen, _ = reader.Lookup(ctx, it)
	if !seen.Final || seen.Height != 103 || seen.Head != 103 {
		t.Fatalf("after the seal: %+v, want final at 103", seen.Observation)
	}
	if v := finality.Defaults()["ton"].Judge(seen.Observation, time.Minute); v.Kind != finality.KindFinal {
		t.Fatalf("verdict = %s, want final", v)
	}
}

// 防的情境：對帳把一筆付款數成三筆、或把退回來的那筆數成付了。同一把 ref 在 window 裡的 message 上
// 出現三次（transfer、internal_transfer、transfer_notification），Source 只在 merchant 的
// jetton wallet 加餘額那一筆交易上回報它；bounce 回來的那筆一次都不回報。
func TestTONReader_TransfersCountEachPaymentOnceAtTheReceiver(t *testing.T) {
	ctx := context.Background()
	f, reader, _, items := tonScenario(t)
	transfers, err := reader.Transfers(ctx, 101, 104)
	if err != nil {
		t.Fatalf("Transfers: %v", err)
	}
	if len(transfers) != 1 {
		t.Fatalf("%d transfers, want exactly the one that landed: %+v", len(transfers), transfers)
	}
	got := transfers[0]
	if got.Ref != items[0].Ref || got.To != items[0].Merchant || got.From != f.acc.Wallet.String() || got.Amount.Cmp(items[0].Amount) != 0 || got.Height != 103 || got.Token != "USDC" {
		t.Fatalf("transfer = %+v", got)
	}
	// 那把 ref 在鏈上其實出現了三次：數一數 window 裡帶著它的 message。
	seen := 0
	for _, tx := range f.txs {
		for _, m := range append([]*chain.TONMessage{tx.In}, tx.Out...) {
			if m != nil && m.Body != nil && carriesRef(m.Body, items[0].Ref) {
				seen++
			}
		}
	}
	if seen != 6 { // 三則 message，各是一筆交易的 In、也是另一筆交易的 Out，所以數到六次
		t.Fatalf("the ref rides %d message slots, want 6 (three messages, each seen from both ends)", seen)
	}
}

// carriesRef 找一則 message 的 body 有沒有帶我們的 forward_payload（不管它是哪一種 op）。
func carriesRef(body *boc.Cell, ref paymentref.Ref) bool {
	for i := 0; i < body.Refs(); i++ {
		p := body.Ref(i).Begin()
		if p.Uint(32) == chain.TONPayloadOp {
			var got paymentref.Ref
			copy(got[:], p.Bytes(32))
			return got == ref
		}
	}
	return false
}

// 防的情境：別人拿我們的 ref 付了一筆錢，Source 沒看見。merchant 的 jetton wallet 上任何一筆成功的
// internal_transfer 都要回報，不管是誰送的：對帳引擎才有機會把第二筆列成 paid_twice。
func TestTONReader_TransfersSeeAPaymentWeDidNotSend(t *testing.T) {
	ctx := context.Background()
	f, reader, _, items := tonScenario(t)
	stranger := boc.Address{Hash: sha256.Sum256([]byte("someone else's jetton wallet"))}
	strangerOwner := boc.Address{Hash: sha256.Sum256([]byte("someone else"))}
	f.owner[stranger] = strangerOwner
	payload, _ := boc.NewBuilder().Uint(chain.TONPayloadOp, 32).Bytes(items[0].Ref[:]).Build()
	body, _ := boc.NewBuilder().Uint(chain.TONOpInternalTransfer, 32).Uint(qid(items[0].Ref), 64).Coins(items[0].Amount).
		Address(strangerOwner).Address(strangerOwner).Coins(big.NewInt(1)).Bit(true).Ref(payload).Build()
	tx := f.newTx(stranger, &chain.TONMessage{Hash: sha256.Sum256([]byte("stranger in")), Dst: stranger, Value: big.NewInt(0)})
	f.emit(tx, jettonWalletOf(mustAddr(items[0].Merchant)), big.NewInt(50_000_000), true, false, body)
	f.step()
	f.seal(105)

	transfers, err := reader.Transfers(ctx, 101, 105)
	if err != nil {
		t.Fatalf("Transfers: %v", err)
	}
	if len(transfers) != 2 {
		t.Fatalf("%d transfers, want 2 (ours and the stranger's)", len(transfers))
	}
	if transfers[1].From != strangerOwner.String() || transfers[1].Ref != items[0].Ref || transfers[1].Height != 105 {
		t.Fatalf("second transfer = %+v", transfers[1])
	}
}

// 防的情境：一筆 intent 指著一則不是我們送的請求，或請求裡根本沒有它。這是資料壞了，
// 要回 error 而不是回一個看起來像「還沒進區塊」的 Observation。
func TestTONReader_RefusesAnIntentThatIsNotOurs(t *testing.T) {
	ctx := context.Background()
	_, reader, external, items := tonScenario(t)
	if _, err := reader.Lookup(ctx, &intent.Intent{Ref: items[0].Ref, TxHash: "not-a-hash"}); !errors.Is(err, chain.ErrNotOurRequest) {
		t.Fatalf("bad hash: %v", err)
	}
	other := tonRun(5)[4].Ref
	if _, err := reader.Lookup(ctx, &intent.Intent{Ref: other, TxHash: hex.EncodeToString(external[:])}); !errors.Is(err, chain.ErrNotOurRequest) {
		t.Fatalf("a ref the request does not carry: %v", err)
	}
}

// 防的情境：整條路接進 listener 之後判錯格。三筆 confirming 的 intent 交給 listener.Check，
// 到帳的 settled、退回來的 needs_review、在路上的 wait；第二次看同一筆是 no-op。
func TestTONReader_ThroughTheListener(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	intents := intent.NewMemoryStore()
	journal := ledger.NewMemoryJournal()
	// 名單從 intent 長出來：ref 是 intent 算的，鏈名是 ton:mainnet，listener 才查得到不可逆規則。
	items := tonRun(3)
	for i := range items {
		id := "pi_000" + string(rune('1'+i))
		in, err := intent.New(intent.Spec{ID: id, Chain: "ton:mainnet", Token: "USDC", Payer: tonAccounts().Wallet.String(),
			Merchant: items[i].Merchant, Amount: items[i].Amount}, now)
		if err != nil {
			t.Fatalf("intent.New: %v", err)
		}
		items[i].Ref = in.Ref
		_ = intents.Save(ctx, in, 0)
	}
	_, reader, external, _ := tonScenarioFor(t, items)
	for i, it := range items {
		id := "pi_000" + string(rune('1'+i))
		in, _ := intents.Get(ctx, id)
		_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateAuthorized, By: intent.ActorAPI, At: now})
		_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateSettling, By: intent.ActorRelayer, At: now})
		_, _, _ = journal.Append(ctx, ledger.Entry{ID: id + "/hold", Ref: in.Ref, Kind: ledger.KindHold,
			Asset: ledger.Asset{Chain: in.Chain, Token: in.Token},
			Legs: []ledger.Leg{{Account: ledger.PayerAccount(in.Payer), Amount: new(big.Int).Neg(it.Amount)},
				{Account: ledger.MerchantAccount(in.Merchant), Amount: new(big.Int).Set(it.Amount)}},
			By: "relayer", At: now})
		_, _, _ = intent.Advance(ctx, intents, id, intent.Request{To: intent.StateConfirming, By: intent.ActorRelayer, TxHash: hex.EncodeToString(external[:]), At: now})
	}
	l := listener.New(intents, journal, reader, listener.WithClock(func() time.Time { return now.Add(time.Minute) }))
	want := map[string]listener.Outcome{"pi_0001": listener.OutcomeSettled, "pi_0002": listener.OutcomeReview, "pi_0003": listener.OutcomeWait}
	for id, w := range want {
		rep, err := l.Check(ctx, id)
		if err != nil {
			t.Fatalf("Check(%s): %v", id, err)
		}
		if rep.Outcome != w {
			t.Fatalf("Check(%s) = %s, want %s", id, rep, w)
		}
	}
	if rep, _ := l.Check(ctx, "pi_0001"); rep.Outcome != listener.OutcomeNoop {
		t.Fatalf("second look = %s, want no-op", rep)
	}
}

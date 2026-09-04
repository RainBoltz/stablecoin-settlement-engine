package chain_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/boc"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/finality"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intent"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// 這個檔案讓 TONReader 讀的不是 tonread_test.go 裡那段照 .fc 寫出來的假鏈，而是 @ton/sandbox 錄下來的真交易：
// contracts/ton/e2e.mjs 把 TransferRequest 組出來的 request 簽好、送進真的 W5 合約與真的 jetton 合約
// （TEP-74 參考實作與 USDT 的 stablecoin-contract），再把每一筆交易、每一則 message 的 bits 與雜湊寫進
// testdata/tonsandbox/（make ton-test 會重錄）。這裡把 message body 用 boc 逐 bit 重建、對一次雜湊，
// 然後追 trace、翻 Observation、對帳。假鏈跟被測的程式出自同一雙手，只有真合約的輸出才能證明
// bounced message 的版面、internal_transfer 的欄位、on_bounce 落在哪一筆，是我們以為的那樣。
//
// masterchain seqno 在錄的時候照 message 的深度給：錢包 101、我們的 jetton wallet 102、merchant 的
// jetton wallet 103、退回來落地的那筆 104，跟 tonread_test.go 的劇本同一種座標。

const sandboxDir = "testdata/tonsandbox"

type sandboxCell struct {
	Bits string
	Refs []*sandboxCell
	Hash string
}

type sandboxMsg struct {
	Hash, Src, Dst, Value string
	Bounce, Bounced       bool
	Body                  *sandboxCell
}

type sandboxTx struct {
	Account, Role, LT, Hash string
	In                      *sandboxMsg
	Out                     []*sandboxMsg
	Aborted                 bool
	ExitCode                int
	Masterchain             uint64
}

type sandboxPayout struct{ IntentID, Merchant, Amount, Ref string }

type sandboxRecording struct {
	Txs       []*sandboxTx
	Externals []string
	Payouts   [][]sandboxPayout
}

// cell 用 boc 的 Builder 逐 bit 重建一個 cell，並且要求 Go 算出來的雜湊跟 @ton/core 算的一樣。
func (c *sandboxCell) cell(t *testing.T) *boc.Cell {
	t.Helper()
	b := boc.NewBuilder()
	for _, ch := range c.Bits {
		b.Bit(ch == '1')
	}
	for _, r := range c.Refs {
		b.Ref(r.cell(t))
	}
	cell, err := b.Build()
	if err != nil {
		t.Fatalf("rebuild cell: %v", err)
	}
	h := cell.Hash()
	if got := hex.EncodeToString(h[:]); got != c.Hash {
		t.Fatalf("boc hashes the cell to %s, @ton/core says %s", got, c.Hash)
	}
	return cell
}

func sandboxHash(t *testing.T, s string) [32]byte {
	t.Helper()
	var h [32]byte
	if len(s) != 64 {
		t.Fatalf("bad hash %q", s)
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		t.Fatalf("bad hash %q: %v", s, err)
	}
	return h
}

func sandboxAddr(t *testing.T, s string) boc.Address {
	t.Helper()
	if s == "" {
		return boc.Address{}
	}
	a, err := boc.ParseAddress(s)
	if err != nil {
		t.Fatalf("bad address %q: %v", s, err)
	}
	return a
}

// sandboxNode 是一個 TONNode，答案全部來自錄下來的交易。
type sandboxNode struct {
	txs    []*chain.TONTransaction
	byIn   map[[32]byte]*chain.TONTransaction
	role   map[boc.Address]string
	byRole map[string]boc.Address
	head   uint64
}

func (n *sandboxNode) Masterchain(context.Context) (uint64, error) { return n.head, nil }

func (n *sandboxNode) TransactionByInMsg(_ context.Context, m [32]byte) (*chain.TONTransaction, error) {
	return n.byIn[m], nil
}

func (n *sandboxNode) Transactions(_ context.Context, a boc.Address, from, to uint64) ([]*chain.TONTransaction, error) {
	var out []*chain.TONTransaction
	for _, tx := range n.txs {
		if tx.Account == a && tx.Masterchain >= from && tx.Masterchain <= to {
			out = append(out, tx)
		}
	}
	return out, nil
}

func loadSandbox(t *testing.T, name string) (*sandboxRecording, *sandboxNode) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sandboxDir, name+".json"))
	if err != nil {
		t.Fatalf("read recording: %v", err)
	}
	var rec sandboxRecording
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decode recording: %v", err)
	}
	n := &sandboxNode{byIn: map[[32]byte]*chain.TONTransaction{}, role: map[boc.Address]string{}, byRole: map[string]boc.Address{}}
	msg := func(m *sandboxMsg) *chain.TONMessage {
		if m == nil {
			return nil
		}
		v, ok := new(big.Int).SetString(m.Value, 10)
		if !ok {
			t.Fatalf("bad value %q", m.Value)
		}
		out := &chain.TONMessage{Hash: sandboxHash(t, m.Hash), Src: sandboxAddr(t, m.Src), Dst: sandboxAddr(t, m.Dst), Value: v, Bounce: m.Bounce, Bounced: m.Bounced}
		if m.Body != nil {
			out.Body = m.Body.cell(t)
		}
		return out
	}
	for _, x := range rec.Txs {
		var lt uint64
		if _, err := fmt.Sscan(x.LT, &lt); err != nil {
			t.Fatalf("bad lt %q", x.LT)
		}
		tx := &chain.TONTransaction{Account: sandboxAddr(t, x.Account), LT: lt, Hash: sandboxHash(t, x.Hash), In: msg(x.In), Aborted: x.Aborted, ExitCode: x.ExitCode, Masterchain: x.Masterchain}
		for _, o := range x.Out {
			tx.Out = append(tx.Out, msg(o))
		}
		n.txs = append(n.txs, tx)
		n.byIn[tx.In.Hash] = tx
		n.role[tx.Account] = x.Role
		n.byRole[x.Role] = tx.Account
		if tx.Masterchain > n.head {
			n.head = tx.Masterchain
		}
	}
	return &rec, n
}

// sandboxReader 接一個 TONReader，Watch 錄音裡每一個 merchant 的 jetton wallet。
func sandboxReader(t *testing.T, rec *sandboxRecording, n *sandboxNode) *chain.TONReader {
	t.Helper()
	acc := chain.TONAccounts{Wallet: n.byRole["wallet"], JettonWallet: n.byRole["our jetton wallet"]}
	if acc.Wallet.IsZero() || acc.JettonWallet.IsZero() {
		t.Fatalf("the recording names no wallet or jetton wallet")
	}
	r := chain.NewTONReader(n, acc, "USDT")
	for a, role := range n.role {
		if strings.HasSuffix(role, "'s jetton wallet") {
			var i int
			if _, err := fmt.Sscanf(role, "merchant %d's jetton wallet", &i); err != nil {
				t.Fatalf("role %q", role)
			}
			r.Watch(rec.Payouts[0][i].Merchant, a)
		}
	}
	return r
}

func sandboxRef(t *testing.T, p sandboxPayout) paymentref.Ref {
	t.Helper()
	return paymentref.Ref(sandboxHash(t, p.Ref))
}

// 防的情境：真的 jetton 合約送出的 internal_transfer 版面跟 tonInternalTransfer 讀的對不上，或 ref 在
// forward_payload 裡被真合約動過。三筆與 255 筆（後者只在 make ton-test 之後存在）的每一筆都要追到
// merchant 的 jetton wallet、金額正確、在 103 不可逆，對帳時剛好各算一次、付款人是我們的錢包。
func TestTONSandbox_DeliveredPayoutsTraceToTheMerchantAndReconcileOnce(t *testing.T) {
	names, _ := filepath.Glob(filepath.Join(sandboxDir, "*-delivered-*.json"))
	if len(names) < 2 {
		t.Fatalf("expected the ft and usdt recordings in %s, found %v", sandboxDir, names)
	}
	for _, path := range names {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			rec, n := loadSandbox(t, name)
			r := sandboxReader(t, rec, n)
			ext := sandboxHash(t, rec.Externals[0])
			for i, p := range rec.Payouts[0] {
				tr, err := r.Trace(ctx, ext, sandboxRef(t, p))
				if err != nil {
					t.Fatalf("payout %d: Trace: %v", i, err)
				}
				if tr.Outcome != chain.TONDelivered || len(tr.Steps) != 3 || tr.Terminal == nil {
					t.Fatalf("payout %d: %s", i, tr)
				}
				if want := fmt.Sprintf("merchant %d's jetton wallet", i); n.role[tr.Terminal.Account] != want {
					t.Fatalf("payout %d: terminal is %q, want %q", i, n.role[tr.Terminal.Account], want)
				}
				if tr.Received.String() != p.Amount || tr.Terminal.Masterchain != 103 {
					t.Fatalf("payout %d: received %s at %d: %s", i, tr.Received, tr.Terminal.Masterchain, tr)
				}
				seen, err := r.Lookup(ctx, &intent.Intent{Ref: sandboxRef(t, p), TxHash: rec.Externals[0]})
				if err != nil {
					t.Fatalf("payout %d: Lookup: %v", i, err)
				}
				if !seen.Included || !seen.Final || !seen.Succeeded || seen.Height != 103 || seen.Received.String() != p.Amount {
					t.Fatalf("payout %d: observation %+v", i, seen.Observation)
				}
				if v := finality.Defaults()["ton"].Judge(seen.Observation, time.Minute); v.Kind != finality.KindFinal {
					t.Fatalf("payout %d: verdict %s", i, v)
				}
			}
			transfers, err := r.Transfers(ctx, 101, 110)
			if err != nil {
				t.Fatalf("Transfers: %v", err)
			}
			if len(transfers) != len(rec.Payouts[0]) {
				t.Fatalf("%d transfers, want %d", len(transfers), len(rec.Payouts[0]))
			}
			for i, p := range rec.Payouts[0] {
				matches := 0
				for _, tf := range transfers {
					if tf.Ref != sandboxRef(t, p) {
						continue
					}
					matches++
					if tf.To != p.Merchant || tf.Amount.String() != p.Amount || tf.From != n.byRole["wallet"].String() || tf.Height != 103 || tf.Token != "USDT" {
						t.Fatalf("payout %d: transfer %+v", i, tf)
					}
				}
				if matches != 1 {
					t.Fatalf("payout %d reconciled %d times", i, matches)
				}
			}
			t.Logf("%d payouts delivered and reconciled across %d recorded transactions", len(rec.Payouts[0]), len(n.txs))
		})
	}
}

// 防的情境：我們自己的 jetton wallet 拒收之後，真的 TVM 退回來的 bounced message 長得跟假鏈不一樣，
// trace 追不到終點。兩份合約各一個 exit code（參考實作 706、USDT 47），終點都是錢包收下 bounce 那筆，
// listener 判 failed，對帳一筆都不該看到。
func TestTONSandbox_ARefusedTransferBouncesToTheWallet(t *testing.T) {
	for _, tc := range []struct {
		name string
		exit int
	}{{"ft-rejected", 706}, {"usdt-rejected", 47}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rec, n := loadSandbox(t, tc.name)
			r := sandboxReader(t, rec, n)
			p := rec.Payouts[0][0]
			tr, err := r.Trace(ctx, sandboxHash(t, rec.Externals[0]), sandboxRef(t, p))
			if err != nil {
				t.Fatalf("Trace: %v", err)
			}
			if tr.Outcome != chain.TONRejected || len(tr.Steps) != 3 || !tr.Steps[1].Tx.Aborted || tr.Steps[1].Tx.ExitCode != tc.exit {
				t.Fatalf("trace = %s", tr)
			}
			if n.role[tr.Terminal.Account] != "wallet" || tr.Terminal.Aborted || tr.Terminal.Masterchain != 103 {
				t.Fatalf("terminal = %s %+v", n.role[tr.Terminal.Account], tr.Terminal)
			}
			seen, err := r.Lookup(ctx, &intent.Intent{Ref: sandboxRef(t, p), TxHash: rec.Externals[0]})
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if !seen.Included || !seen.Final || seen.Succeeded || seen.Height != 103 {
				t.Fatalf("observation %+v", seen.Observation)
			}
			if v := finality.Defaults()["ton"].Judge(seen.Observation, time.Minute); v.Kind != finality.KindFailed {
				t.Fatalf("verdict %s", v)
			}
			if transfers, _ := r.Transfers(ctx, 101, 110); len(transfers) != 0 {
				t.Fatalf("%d transfers, want none", len(transfers))
			}
			t.Logf("trace: %s", tr)
		})
	}
}

// 防的情境：merchant 那一步失敗（USDT 的 wallet 被 admin 鎖住收款，exit 45）被當成到帳，或退回來的
// 那筆 on_bounce 沒被當成終點。第一筆先到帳、把 wallet 部署出來，鎖住之後第二筆退回：終點是我們的
// jetton wallet 在 104 那筆沒失敗的交易，對帳只看到第一筆。
func TestTONSandbox_ALockedMerchantWalletBouncesBackToOurs(t *testing.T) {
	ctx := context.Background()
	rec, n := loadSandbox(t, "usdt-bounced")
	r := sandboxReader(t, rec, n)
	first, second := rec.Payouts[0][0], rec.Payouts[1][0]
	tr, err := r.Trace(ctx, sandboxHash(t, rec.Externals[0]), sandboxRef(t, first))
	if err != nil || tr.Outcome != chain.TONDelivered {
		t.Fatalf("first payout: %v %s", err, tr)
	}
	tr, err = r.Trace(ctx, sandboxHash(t, rec.Externals[1]), sandboxRef(t, second))
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if tr.Outcome != chain.TONBounced || len(tr.Steps) != 4 || !tr.Steps[2].Tx.Aborted || tr.Steps[2].Tx.ExitCode != 45 {
		t.Fatalf("trace = %s", tr)
	}
	if n.role[tr.Terminal.Account] != "our jetton wallet" || tr.Terminal.Aborted || tr.Terminal.Masterchain != 104 {
		t.Fatalf("terminal = %s %+v", n.role[tr.Terminal.Account], tr.Terminal)
	}
	seen, err := r.Lookup(ctx, &intent.Intent{Ref: sandboxRef(t, second), TxHash: rec.Externals[1]})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !seen.Included || !seen.Final || seen.Succeeded || seen.Height != 104 {
		t.Fatalf("observation %+v", seen.Observation)
	}
	if v := finality.Defaults()["ton"].Judge(seen.Observation, time.Minute); v.Kind != finality.KindFailed {
		t.Fatalf("verdict %s", v)
	}
	transfers, err := r.Transfers(ctx, 101, 110)
	if err != nil {
		t.Fatalf("Transfers: %v", err)
	}
	if len(transfers) != 1 || transfers[0].Ref != sandboxRef(t, first) || transfers[0].Amount.String() != first.Amount {
		t.Fatalf("transfers = %+v, want exactly the first payout", transfers)
	}
	t.Logf("trace: %s", tr)
}

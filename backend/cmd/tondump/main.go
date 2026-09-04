// tondump 是 contracts/ton 那套 sandbox 測試的 Go 端：從 stdin 讀一份 JSON 的付款名單，用 chain.TON 組成
// 一則 W5 request，把 signing cell 的 BoC、它的雜湊、每一筆的 ref / body / message 印成 JSON。
// TypeScript 那邊拿這份輸出去簽名、送進 @ton/sandbox 裡真的 W5 合約（make ton-test）。
//
// 輸入：
//
//	{"Wallet": "0:…", "JettonWallet": "0:…", "WalletID": 0, "Seqno": 41, "ValidUntil": 1800000300,
//	 "Payouts": [{"IntentID": "pi_0001", "Merchant": "0:…", "Amount": "100000000"}, …]}
//
// Golden 大於 0 的時候不看 Payouts，改組 tonmsg_test.go 裡 tonRun(n) 那份名單，golden.mjs 靠它重算被釘住的雜湊。
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/boc"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

type spec struct {
	Wallet, JettonWallet string
	WalletID             int32
	Seqno, ValidUntil    uint32
	Golden               int
	Payouts              []payout
}

type payout struct {
	IntentID, Merchant, Amount string
	Ref                        string `json:",omitempty"`
}

type result struct {
	SigningBoc, SigningHash string
	Size, Cells, Depth      int
	Payouts                 []payout
	Bodies, Messages        []string
}

// tonRun 跟 tonmsg_test.go 的 tonRun 一字不差：golden.mjs 比的三個雜湊就是這份名單組出來的。
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tondump:", err)
		os.Exit(1)
	}
}

func run() error {
	var in spec
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		return fmt.Errorf("decode spec: %w", err)
	}
	acc := chain.TONAccounts{WalletID: in.WalletID}
	var err error
	if acc.Wallet, err = boc.ParseAddress(in.Wallet); err != nil {
		return fmt.Errorf("wallet: %w", err)
	}
	if acc.JettonWallet, err = boc.ParseAddress(in.JettonWallet); err != nil {
		return fmt.Errorf("jetton wallet: %w", err)
	}
	var items []bulk.Payout
	if in.Golden > 0 {
		items = tonRun(in.Golden)
	} else {
		for i, p := range in.Payouts {
			amt, ok := new(big.Int).SetString(p.Amount, 10)
			if !ok {
				return fmt.Errorf("payout %d: amount %q", i, p.Amount)
			}
			items = append(items, bulk.Payout{
				Ref: paymentref.Derive(paymentref.Terms{
					IntentID: p.IntentID, Chain: "ton:sandbox", Token: "USDT",
					Payer: in.Wallet, Merchant: p.Merchant, Amount: p.Amount,
				}),
				Merchant: p.Merchant, Amount: amt,
			})
		}
	}
	req, err := chain.NewTON().TransferRequest(acc, in.Seqno, in.ValidUntil, items)
	if err != nil {
		return err
	}
	h := req.SigningHash()
	st := req.Stats()
	out := result{
		SigningBoc:  hex.EncodeToString(req.Cell().ToBoC()),
		SigningHash: hex.EncodeToString(h[:]),
		Size:        req.Size(),
		Cells:       st.Cells,
		Depth:       st.Depth,
	}
	for i, it := range items {
		out.Payouts = append(out.Payouts, payout{
			IntentID: fmt.Sprintf("pi_%04d", i+1), Merchant: it.Merchant, Amount: it.Amount.String(),
			Ref: hex.EncodeToString(it.Ref[:]),
		})
		out.Bodies = append(out.Bodies, hex.EncodeToString(req.Body(i).ToBoC()))
		out.Messages = append(out.Messages, hex.EncodeToString(req.Message(i).ToBoC()))
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

package chain_test

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/boc"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
)

// tonCoins 把 nanoton 印成 TON，小數點後最多兩位，給報告用。
func tonCoins(nano uint64) string {
	s := fmt.Sprintf("%d.%02d", nano/1_000_000_000, nano%1_000_000_000/10_000_000)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".") + " TON"
}

// tonRefsAboard 數名單上的 ref 有幾把原封不動躺在 BoC 裡。
func tonRefsAboard(req *chain.TONRequest, items []bulk.Payout) int {
	out := req.Cell().ToBoC()
	n := 0
	for _, it := range items {
		n += bytes.Count(out, it.Ref[:])
	}
	return n
}

// Example_buildATONRequest 把同一份 12 筆的名單組成 TON 上 relayer 要簽的那一則 external message，
// 再把一份 300 筆的名單照 TON 的上限切一次。前兩條鏈組出來的都是「一筆交易」，這裡組出來的
// 是「一則讓錢包送出 12 則 message 的請求」：簽一次、佔一個 seqno，錢在這一步一毛都還沒動；
// 每一筆付款接下來要自己走完四步，任何一步失敗都不影響另外 11 筆。
func Example_buildATONRequest() {
	items := tonRun(12)
	fmt.Printf("run     %d payouts, 100 USDC each\n", len(items))

	acc := tonAccounts()
	req, err := chain.NewTON().TransferRequest(acc, 41, 1_800_000_300, items)
	if err != nil {
		fmt.Println(err)
		return
	}
	st := req.Stats()
	fmt.Printf("ton     seqno %d  valid until %d  %d messages behind one signature  signing hash %s\n",
		req.Seqno, req.ValidUntil, req.Messages(), boc.Short(req.SigningHash()))
	fmt.Printf("ton     request %s bytes  %d cells  %d deep  refs %d/%d aboard\n",
		commas(uint64(req.Size())), st.Cells, st.Depth, tonRefsAboard(req, items), len(items))
	fmt.Printf("ton     each message carries %s, forwards 1 nanoton; the wallet fronts %s until the excesses come back\n",
		tonCoins(chain.TONAttach), tonCoins(uint64(len(items))*chain.TONAttach))

	hops := make([]string, 0, 4)
	for _, h := range chain.TONHops() {
		hops = append(hops, fmt.Sprintf("%s -%s-> %s", h.From, h.Name, h.To))
	}
	fmt.Printf("ton     one payout is %d hops: %s\n", len(hops), strings.Join(hops, "; "))

	// 一份 300 筆的名單：EVM 上是一筆交易，Solana 上是 38 筆，TON 上是兩則請求、兩個 seqno，
	// 而且切的依據不是交易多大，是一則請求裝幾則 message。
	plan, err := bulk.Pack(tonRun(300), bulk.Defaults()["ton"])
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(plan)
	for _, b := range plan.Batches {
		fmt.Println(b)
		r, err := chain.NewTON().TransferRequest(acc, 41+uint32(b.Index), 1_800_000_300, b.Items)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Printf("ton     batch #%d built: %s bytes before the signature, %d deep  (bulk estimated %s for the signed message)\n",
			b.Index, commas(uint64(r.Size())), r.Stats().Depth, commas(b.Used[1].Used))
	}

	// Output:
	// run     12 payouts, 100 USDC each
	// ton     seqno 41  valid until 1800000300  12 messages behind one signature  signing hash ea0b3151…
	// ton     request 2,318 bytes  50 cells  15 deep  refs 12/12 aboard
	// ton     each message carries 0.05 TON, forwards 1 nanoton; the wallet fronts 0.6 TON until the excesses come back
	// ton     one payout is 4 hops: wallet -transfer-> our jetton wallet; our jetton wallet -internal_transfer-> merchant's jetton wallet; merchant's jetton wallet -transfer_notification-> merchant; merchant's jetton wallet -excesses-> wallet
	// plan    ton      300 payouts  2 batches  0 new accounts  rent 0 nanoton
	// batch   #1       255 items  messages 255/255  bytes 49,612/65,535  depth 258/512
	// ton     batch #1 built: 49,513 bytes before the signature, 258 deep  (bulk estimated 49,612 for the signed message)
	// batch   #2       45 items  messages 45/255  bytes 8,872/65,535  depth 48/512
	// ton     batch #2 built: 8,588 bytes before the signature, 48 deep  (bulk estimated 8,872 for the signed message)
}

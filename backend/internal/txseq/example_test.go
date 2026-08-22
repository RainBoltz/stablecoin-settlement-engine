package txseq_test

import (
	"context"
	"fmt"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/txseq"
)

// Example_counter 是一個發送帳戶的序號在四種情況下怎麼走：送出去了、確定沒出門、不知道、以及跟鏈上對齊。
// 帳戶是 devnet 的 relayer（Anvil 助記詞 index 3）。
func Example_counter() {
	ctx := context.Background()
	c := txseq.NewCounter()
	const relayer = "0x90F79bf6EB2c4f870365E785982E1f101E93b906"

	step := func(s txseq.Sent) {
		r, err := c.Reserve(ctx, relayer)
		if err != nil {
			fmt.Printf("reserve  %v\n", err)
			return
		}
		fmt.Printf("reserve  %s\n", r)
		_ = c.Resolve(ctx, r, s)
		fmt.Printf("resolve  %-9s %s\n", s, c.Status(relayer))
	}

	step(txseq.SentYes)     // 節點收下了：號用掉，計數器往前
	step(txseq.SentNo)      // 確定沒出門：號退回去，下一筆重用
	step(txseq.SentUnknown) // 不知道：號當成用掉，這一格變成洞
	step(txseq.SentYes)     // 帳戶停發號，取號直接被擋下來

	// 鏈上說下一個可用的號是 2，代表那筆下落不明的交易其實上鏈了，洞不見了。
	_ = c.Sync(ctx, relayer, 2)
	fmt.Printf("sync     %s\n", c.Status(relayer))
	step(txseq.SentYes)

	// Output:
	// reserve  0x90F7…b906 #0
	// resolve  sent      0x90F7…b906  next 1    in-flight -    gap -
	// reserve  0x90F7…b906 #1
	// resolve  not-sent  0x90F7…b906  next 1    in-flight -    gap -
	// reserve  0x90F7…b906 #1
	// resolve  unknown   0x90F7…b906  next 2    in-flight -    gap 1
	// reserve  txseq: account has an unfilled gap at 1
	// sync     0x90F7…b906  next 2    in-flight -    gap -
	// reserve  0x90F7…b906 #2
	// resolve  sent      0x90F7…b906  next 3    in-flight -    gap -
}

package intake_test

import (
	"fmt"
	"strings"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intake"
)

// payoutFile 造一份 300 行的月底撥款檔案，裡面混了四行過不了關的資料。
// 四行各自是一個層級的問題：欄位數、空白的欄位、算不出金額的字串、重複的 intent。
func payoutFile() string {
	var b strings.Builder
	b.WriteString("intent_id,merchant,amount\n")
	for i := 1; i <= 300; i++ {
		switch i {
		case 40:
			// 有人在試算表裡把金額改成讀得懂的樣子。
			fmt.Fprintf(&b, "pi_%04d,mch-%03d,100.00\n", i, i)
		case 88:
			// merchant 那一欄空著。
			fmt.Fprintf(&b, "pi_%04d,,100000000\n", i)
		case 145:
			// 上游把同一筆 intent 寫了第二次。
			fmt.Fprintf(&b, "pi_%04d,mch-%03d,100000000\n", 144, 144)
		case 212:
			// 少了一欄。
			fmt.Fprintf(&b, "pi_%04d,mch-%03d\n", i, i)
		default:
			fmt.Fprintf(&b, "pi_%04d,mch-%03d,100000000\n", i, i)
		}
	}
	return b.String()
}

// Example_readAPayoutFile 走一遍檔案到批次的路：先用預設的 Policy 讀一次（整份退回），
// 再讓 operator 看過報告之後打開跳過，然後把收下來的名單交給 bulk 切批，
// 最後假設第 8 批在鏈上失敗，把它翻譯回檔案的行號。
func Example_readAPayoutFile() {
	terms := intake.Terms{Chain: "solana", Token: "USDC", Payer: "platform"}

	strict, err := intake.Read(strings.NewReader(payoutFile()), terms, intake.Policy{})
	fmt.Println(strict)
	fmt.Println(err)
	for _, rj := range strict.Rejected {
		fmt.Println(rj)
	}

	// 報告有人看過了，四行都確定是那份檔案自己的問題，其餘 296 筆照送。
	run, err := intake.Read(strings.NewReader(payoutFile()), terms, intake.Policy{Skip: true, MaxRejects: 10})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(run)

	plan, err := bulk.Pack(run.Accepted, bulk.Defaults()["solana"])
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(plan)

	// 第 8 批在鏈上回滾了。它裝的十二筆對到檔案的哪幾行？
	trace, err := run.Trace(plan, 8)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(trace)

	// Output:
	// intake  solana   300 rows  0 accepted  4 rejected
	// intake: rejected rows and the policy does not skip them: 4 of 300 rows
	// reject  line 41    amount     "100.00" is not a whole number of minor units
	// reject  line 89    merchant   "" is not a merchant
	// reject  line 146   duplicate  pi_0144 already appears on line 145
	// reject  line 213   fields     the row has 2 fields, want 3
	// intake  solana   300 rows  296 accepted  4 rejected
	// plan    solana   296 payouts  25 batches  0 new accounts  rent 0 lamports
	// trace   batch #8   12 items  csv lines 87-88, 90-99
}

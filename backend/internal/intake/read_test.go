package intake_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intake"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// terms 是這一包測試共用的整份檔案屬性。
var terms = intake.Terms{Chain: "solana", Token: "USDC", Payer: "platform"}

// file 把幾行資料接上表頭，省得每條測試都自己寫一次表頭。
func file(rows ...string) string {
	return "intent_id,merchant,amount\n" + strings.Join(rows, "\n") + "\n"
}

// 一行壞掉整份退回是預設值，而且退回的時候 Accepted 要是空的：漏看 error 的呼叫端
// 最糟只會送出零筆，不會送出一半。
func TestRead_RejectsTheWholeFileByDefault(t *testing.T) {
	run, err := intake.Read(strings.NewReader(file(
		"pi_0001,mch-001,100000000",
		"pi_0002,mch-002,-1",
		"pi_0003,mch-003,100000000",
	)), terms, intake.Policy{})
	if !errors.Is(err, intake.ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if len(run.Accepted) != 0 {
		t.Fatalf("accepted %d payouts, want 0", len(run.Accepted))
	}
	if len(run.Rejected) != 1 {
		t.Fatalf("rejected %d rows, want 1", len(run.Rejected))
	}
	if run.Rows != 3 {
		t.Fatalf("rows = %d, want 3", run.Rows)
	}
}

// 有人看過報告、確認那幾行是檔案自己的問題之後，剩下的照送。這是 operator 的決定，
// 不是預設值，所以要由 Policy 明講。
func TestRead_SkipsRejectedRowsWhenThePolicySaysSo(t *testing.T) {
	run, err := intake.Read(strings.NewReader(file(
		"pi_0001,mch-001,100000000",
		"pi_0002,mch-002,-1",
		"pi_0003,mch-003,100000000",
	)), terms, intake.Policy{Skip: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(run.Accepted) != 2 {
		t.Fatalf("accepted %d payouts, want 2", len(run.Accepted))
	}
	if len(run.Rejected) != 1 || run.Rejected[0].Line != 3 {
		t.Fatalf("rejected = %v, want one row on line 3", run.Rejected)
	}
}

// 跳過是有上限的：壞掉的行多到一個程度，比較像產生這份檔案的系統壞了，
// 那就不該由「跳過」這個機制默默吸收掉。
func TestRead_StopsWhenThereAreTooManyRejectedRows(t *testing.T) {
	run, err := intake.Read(strings.NewReader(file(
		"pi_0001,mch-001,-1",
		"pi_0002,mch-002,-1",
		"pi_0003,mch-003,100000000",
	)), terms, intake.Policy{Skip: true, MaxRejects: 1})
	if !errors.Is(err, intake.ErrTooManyRejects) {
		t.Fatalf("err = %v, want ErrTooManyRejects", err)
	}
	if len(run.Accepted) != 0 {
		t.Fatalf("accepted %d payouts, want 0", len(run.Accepted))
	}
	if len(run.Rejected) != 2 {
		t.Fatalf("rejected %d rows, want 2", len(run.Rejected))
	}
}

// 報告一次要交完。只回報第一行的話，編這份檔案的人要修四次才修得完，
// 而每修一次就是一輪重跑。
func TestRead_ReportsEveryBadRowNotJustTheFirst(t *testing.T) {
	run, _ := intake.Read(strings.NewReader(file(
		"pi_0001,mch-001,100000000",
		"pi_0002,mch-002,100.00",
		"pi_0003,,100000000",
		"pi_0003,mch-004,100000000",
		"pi_0005,mch-005",
	)), terms, intake.Policy{Skip: true})
	want := []struct {
		line   int
		reason intake.Reason
	}{
		{3, intake.ReasonAmount},
		{4, intake.ReasonMerchant},
		{5, intake.ReasonDuplicate},
		{6, intake.ReasonFields},
	}
	if len(run.Rejected) != len(want) {
		t.Fatalf("rejected %d rows, want %d: %v", len(run.Rejected), len(want), run.Rejected)
	}
	for i, w := range want {
		if run.Rejected[i].Line != w.line || run.Rejected[i].Reason != w.reason {
			t.Fatalf("reject %d = %v, want line %d %s", i, run.Rejected[i], w.line, w.reason)
		}
	}
}

// 金額只收最小單位的正整數。「100.00」看起來最無害，卻是最貴的一種：要換算得先知道
// 這顆 token 有幾位小數，而那件事不寫在這一行裡。
func TestRead_RejectsAnAmountThatIsNotWholeMinorUnits(t *testing.T) {
	for _, amount := range []string{"100.00", "-1", "0", "1e6", `"1,000"`, "", "  ", "0x64", "+1"} {
		run, _ := intake.Read(strings.NewReader(file("pi_0001,mch-001,"+amount)), terms, intake.Policy{Skip: true})
		if len(run.Accepted) != 0 {
			t.Fatalf("amount %q was accepted", amount)
		}
		if len(run.Rejected) != 1 || run.Rejected[0].Reason != intake.ReasonAmount {
			t.Fatalf("amount %q: rejected = %v, want one amount reject", amount, run.Rejected)
		}
	}
}

// 同一筆 intent 寫兩次，兩行的金額只要不一樣就會算出兩把不同的 ref，合約那一關全部放行。
// 所以這一關擋的東西合約擋不住，理由要寫在報告裡：第二行指回第一行在哪。
func TestRead_RejectsTheSecondRowWithTheSameIntentID(t *testing.T) {
	run, _ := intake.Read(strings.NewReader(file(
		"pi_0001,mch-001,100000000",
		"pi_0002,mch-002,100000000",
		"pi_0001,mch-003,250000000",
	)), terms, intake.Policy{Skip: true})
	if len(run.Accepted) != 2 {
		t.Fatalf("accepted %d payouts, want 2", len(run.Accepted))
	}
	if len(run.Rejected) != 1 {
		t.Fatalf("rejected = %v, want one row", run.Rejected)
	}
	rj := run.Rejected[0]
	if rj.Line != 4 || rj.Reason != intake.ReasonDuplicate {
		t.Fatalf("reject = %v, want a duplicate on line 4", rj)
	}
	if !strings.Contains(rj.Detail, "line 2") {
		t.Fatalf("detail = %q, want it to point at line 2", rj.Detail)
	}
}

// 表頭對不上的時候，每一行的欄位意義都是猜的，猜錯的下場是把金額付給一個看起來像
// merchant 的字串。所以這是整份檔案的問題，不記成某一行的問題。
func TestRead_RejectsAFileWithAnUnknownHeader(t *testing.T) {
	run, err := intake.Read(strings.NewReader(
		"merchant,intent_id,amount\nmch-001,pi_0001,100000000\n"), terms, intake.Policy{Skip: true})
	if !errors.Is(err, intake.ErrBadHeader) {
		t.Fatalf("err = %v, want ErrBadHeader", err)
	}
	if len(run.Accepted) != 0 || len(run.Rejected) != 0 {
		t.Fatalf("run = %v, want nothing read at all", run)
	}
}

// 只有表頭的檔案不是「零筆付款的計畫」，是有人送錯檔案了。
func TestRead_RejectsAFileWithNoRows(t *testing.T) {
	for _, content := range []string{"", "intent_id,merchant,amount\n"} {
		if _, err := intake.Read(strings.NewReader(content), terms, intake.Policy{Skip: true}); !errors.Is(err, intake.ErrNoRows) {
			t.Fatalf("content %q: err = %v, want ErrNoRows", content, err)
		}
	}
}

// 試算表存出來的 CSV 開頭常常帶一個 BOM。那不是表頭寫錯，卻會讓整份檔案被退回，
// 而退回的理由看起來完全正確，最難查。
func TestRead_AcceptsAHeaderWithAByteOrderMark(t *testing.T) {
	run, err := intake.Read(strings.NewReader(
		"\ufeffintent_id,merchant,amount\npi_0001,mch-001,100000000\n"), terms, intake.Policy{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(run.Accepted) != 1 {
		t.Fatalf("accepted %d payouts, want 1", len(run.Accepted))
	}
}

// 前後空白修掉，空白以外的東西一個字都不改。修掉的理由是試算表很會加空白；
// 不多改的理由是這一欄接下來會被拿去算 ref，改了就對不回上游那筆 intent。
func TestRead_TrimsSurroundingSpacesAndNothingElse(t *testing.T) {
	run, err := intake.Read(strings.NewReader(file("  pi_0001 , mch-001 , 100000000 ")), terms, intake.Policy{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := run.Accepted[0].Merchant; got != "mch-001" {
		t.Fatalf("merchant = %q, want mch-001", got)
	}
	run, _ = intake.Read(strings.NewReader(file("pi 0001,mch-001,100000000")), terms, intake.Policy{Skip: true})
	if len(run.Rejected) != 1 || run.Rejected[0].Reason != intake.ReasonIntentID {
		t.Fatalf("rejected = %v, want an intent_id reject", run.Rejected)
	}
}

// 這一行算出來的 ref 就是合約那一關會檢查的那一把。兩邊算的方式一旦分岔，
// 檔案這一關的去重就防不到合約那一關的重複，而那要等到整批回滾才會發現。
func TestRead_DerivesTheRefTheContractWillCheck(t *testing.T) {
	run, err := intake.Read(strings.NewReader(file("pi_0001,mch-001,100000000")), terms, intake.Policy{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := paymentref.Derive(paymentref.Terms{
		IntentID: "pi_0001",
		Chain:    "solana",
		Token:    "USDC",
		Payer:    "platform",
		Merchant: "mch-001",
		Amount:   "100000000",
	})
	if run.Accepted[0].Ref != want {
		t.Fatalf("ref = %x, want %x", run.Accepted[0].Ref, want)
	}
}

// 每一項付款要記得自己來自第幾行。中間被剔掉幾行之後這個對應算不出來，
// 而 Trace 整個功能都靠它。
func TestRead_KeepsTheFileLineOfEveryAcceptedRow(t *testing.T) {
	run, err := intake.Read(strings.NewReader(file(
		"pi_0001,mch-001,100000000",
		"pi_0002,mch-002,100.00",
		"pi_0003,mch-003,100000000",
		"pi_0004,mch-004,100000000",
	)), terms, intake.Policy{Skip: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []int{2, 4, 5}
	if len(run.Lines) != len(run.Accepted) {
		t.Fatalf("%d lines for %d payouts", len(run.Lines), len(run.Accepted))
	}
	for i, w := range want {
		if run.Lines[i] != w {
			t.Fatalf("payout %d came from line %d, want %d", i, run.Lines[i], w)
		}
	}
}

// 跳過是開的、每一行都不合格，讀檔這件事本身還是成功了。這時候回一份零筆的 Run 而讓
// 空名單在組批那一側被擋下來，比在這裡多發明一種錯誤好：呼叫端只要處理一種「沒東西可送」。
func TestRead_AcceptsNothingWhenEveryRowIsSkipped(t *testing.T) {
	run, err := intake.Read(strings.NewReader(file(
		"pi_0001,mch-001,-1",
		"pi_0002,mch-002,-1",
	)), terms, intake.Policy{Skip: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(run.Accepted) != 0 || len(run.Rejected) != 2 {
		t.Fatalf("run = %v, want nothing accepted and two rejects", run)
	}
}

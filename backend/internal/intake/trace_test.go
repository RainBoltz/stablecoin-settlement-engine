package intake_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/intake"
)

// runFile 造一份 n 行的檔案，第 bad 行的金額寫成一個算不出來的字串。bad 是 0 就整份都好。
func runFile(n, bad int) string {
	var b strings.Builder
	b.WriteString("intent_id,merchant,amount\n")
	for i := 1; i <= n; i++ {
		amount := "100000000"
		if i == bad {
			amount = "100.00"
		}
		fmt.Fprintf(&b, "pi_%04d,mch-%03d,%s\n", i, i, amount)
	}
	return b.String()
}

// read 讀一份會跳過壞行的檔案，測試裡不再重複寫 Policy。
func read(t *testing.T, n, bad int) intake.Run {
	t.Helper()
	run, err := intake.Read(strings.NewReader(runFile(n, bad)), terms, intake.Policy{Skip: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return run
}

// 一批在鏈上回滾就是「這幾行沒付出去」。中間被剔掉一行之後，那一批的行號會有缺口，
// 而缺口正是這個功能存在的理由：算不出來，只能記著。
func TestTrace_TranslatesABatchBackToFileLinesWithItsGaps(t *testing.T) {
	run := read(t, 30, 14)
	plan, err := bulk.Pack(run.Accepted, bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	trace, err := run.Trace(plan, 2)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	// 第 14 行的資料被剔掉，所以第二批的十二項跨過了它。
	want := "trace   batch #2   12 items  csv lines 14, 16-26"
	if got := trace.String(); got != want {
		t.Fatalf("trace = %q, want %q", got, want)
	}
}

// 每一批各自對回幾行，全部串起來要剛好等於這份 Run 收下來的每一行，一行不多一行不少。
// 少一行的意思是某一筆付款沒有人認領它上鏈了沒有。
func TestTrace_CoversEveryAcceptedLineExactlyOnce(t *testing.T) {
	run := read(t, 300, 145)
	plan, err := bulk.Pack(run.Accepted, bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	var flat []int
	for i := 1; i <= len(plan.Batches); i++ {
		trace, err := run.Trace(plan, i)
		if err != nil {
			t.Fatalf("Trace #%d: %v", i, err)
		}
		flat = append(flat, trace.Lines...)
	}
	if len(flat) != len(run.Lines) {
		t.Fatalf("the batches cover %d lines, the run accepted %d", len(flat), len(run.Lines))
	}
	for i := range flat {
		if flat[i] != run.Lines[i] {
			t.Fatalf("position %d is line %d, want %d", i, flat[i], run.Lines[i])
		}
	}
}

// 拿另一份 Run 的計畫來問行號，答案會是一串看起來很正常的錯誤行號，
// 而沒有人有辦法一眼看出它錯了。所以這裡寧可報錯。
func TestTrace_RejectsAPlanThatWasNotPackedFromThisRun(t *testing.T) {
	run := read(t, 30, 0)
	other := read(t, 24, 0)
	plan, err := bulk.Pack(other.Accepted, bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if _, err := run.Trace(plan, 1); !errors.Is(err, intake.ErrPlanMismatch) {
		t.Fatalf("err = %v, want ErrPlanMismatch", err)
	}
}

// 筆數一樣但順序被動過的話，數量的檢查放行、行號卻全錯。這是把 Pack 換成裝箱演算法
// 最可能造成的後果，所以要有一條測試直接製造它。
func TestTrace_RejectsAPlanWhoseOrderWasChanged(t *testing.T) {
	run := read(t, 30, 0)
	plan, err := bulk.Pack(run.Accepted, bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	plan.Batches[0].Items[0], plan.Batches[1].Items[0] = plan.Batches[1].Items[0], plan.Batches[0].Items[0]
	if _, err := run.Trace(plan, 1); !errors.Is(err, intake.ErrPlanMismatch) {
		t.Fatalf("err = %v, want ErrPlanMismatch", err)
	}
}

// 批號從 1 開始數，因為報告與錯誤訊息都是給人看的。問一個不存在的批號要報錯，
// 不是回一份空的 Trace，不然「這一批沒有任何一行」讀起來像是真的沒有東西要付。
func TestTrace_RejectsABatchThePlanDoesNotHave(t *testing.T) {
	run := read(t, 30, 0)
	plan, err := bulk.Pack(run.Accepted, bulk.Defaults()["solana"])
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	for _, index := range []int{0, -1, len(plan.Batches) + 1} {
		if _, err := run.Trace(plan, index); !errors.Is(err, intake.ErrNoSuchBatch) {
			t.Fatalf("batch #%d: err = %v, want ErrNoSuchBatch", index, err)
		}
	}
}

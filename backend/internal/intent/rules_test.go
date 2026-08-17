package intent

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/transitions.golden from the current Rules()")

// TestRules_MatchGolden 把整張轉移表釘死。改規則要連 golden 一起改（go test -run Golden -update），
// 而且 diff 會出現在 code review 裡；沒有「不小心多開一條路」這種事。
func TestRules_MatchGolden(t *testing.T) {
	path := filepath.Join("testdata", "transitions.golden")
	got := Table()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Fatalf("transition table changed; if intended, run `go test ./internal/intent -run Golden -update`\n--- want\n%s\n--- got\n%s", want, got)
	}
}

// TestRules_TerminalStatesHaveNoExit：終態沒有出口。修正靠新 intent，不靠改舊的。
func TestRules_TerminalStatesHaveNoExit(t *testing.T) {
	for _, r := range Rules() {
		if r.From.Terminal() {
			t.Errorf("rule %s -> %s leaves a terminal state", r.From, r.To)
		}
	}
}

// TestRules_EveryStateIsReachableFromCreated：表上沒有孤島；每個狀態都從 created 走得到。
func TestRules_EveryStateIsReachableFromCreated(t *testing.T) {
	seen := map[State]bool{StateCreated: true}
	queue := []State{StateCreated}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, r := range Rules() {
			if r.From == cur && !seen[r.To] {
				seen[r.To] = true
				queue = append(queue, r.To)
			}
		}
	}
	for _, s := range States() {
		if !seen[s] {
			t.Errorf("state %s is unreachable from created", s)
		}
	}
}

// TestRules_OnlyBackEdgeIsReorg：唯一往回走的路是 confirming -> settling（reorg），而且只有 listener 能走。
// 「往回」的定義：To 在 States() 裡的順位比 From 前面。
func TestRules_OnlyBackEdgeIsReorg(t *testing.T) {
	order := map[State]int{}
	for i, s := range States() {
		order[s] = i
	}
	var back []Rule
	for _, r := range Rules() {
		if order[r.To] < order[r.From] {
			back = append(back, r)
		}
	}
	if len(back) != 1 {
		t.Fatalf("expected exactly one back edge, got %d: %+v", len(back), back)
	}
	r := back[0]
	if r.From != StateConfirming || r.To != StateSettling {
		t.Fatalf("unexpected back edge %s -> %s", r.From, r.To)
	}
	if len(r.By) != 1 || r.By[0] != ActorListener {
		t.Fatalf("reorg back edge must be listener-only, got %v", r.By)
	}
	if !r.NeedsReason {
		t.Fatal("reorg back edge must record a reason")
	}
}

// TestRules_NeedsReviewCannotRetry：needs_review 出去只能是 settled 或 failed，不能回頭重送。
// 交易下落不明時再送一次就可能付兩次；想再付就開新 intent。
func TestRules_NeedsReviewCannotRetry(t *testing.T) {
	for _, r := range Rules() {
		if r.From != StateNeedsReview {
			continue
		}
		if r.To != StateSettled && r.To != StateFailed {
			t.Errorf("needs_review -> %s must not exist", r.To)
		}
		if len(r.By) != 1 || r.By[0] != ActorOperator {
			t.Errorf("needs_review -> %s must be operator-only, got %v", r.To, r.By)
		}
	}
}

// TestRules_OnChainStatesNeedTxHash：進入「有一筆交易在鏈上」的狀態一定要帶雜湊，沒有例外。
func TestRules_OnChainStatesNeedTxHash(t *testing.T) {
	for _, r := range Rules() {
		onChain := r.To == StateConfirming || r.To == StateSettled
		if onChain && !r.NeedsTxHash {
			t.Errorf("%s -> %s must require a tx hash", r.From, r.To)
		}
		if !onChain && r.NeedsTxHash {
			t.Errorf("%s -> %s should not require a tx hash", r.From, r.To)
		}
	}
}

// TestRules_OnlyListenerSettles：只有從鏈上讀事實的元件（與人）能宣告 settled。
// API 與 relayer 看到的都是「交易送出去了」，不是「錢動了」。
func TestRules_OnlyListenerSettles(t *testing.T) {
	for _, r := range Rules() {
		if r.To != StateSettled {
			continue
		}
		for _, a := range r.By {
			if a != ActorListener && a != ActorOperator {
				t.Errorf("%s may not settle (%s -> settled)", a, r.From)
			}
		}
	}
}

// TestRules_NoDuplicateEdges：同一組 (from, to) 只能出現一列，否則 Lookup 會靜靜地拿到第一列。
func TestRules_NoDuplicateEdges(t *testing.T) {
	seen := map[[2]State]bool{}
	for _, r := range Rules() {
		k := [2]State{r.From, r.To}
		if seen[k] {
			t.Errorf("duplicate rule %s -> %s", r.From, r.To)
		}
		seen[k] = true
		if !r.From.Valid() || !r.To.Valid() {
			t.Errorf("rule %s -> %s references an unknown state", r.From, r.To)
		}
		if len(r.By) == 0 {
			t.Errorf("rule %s -> %s has no actor", r.From, r.To)
		}
	}
}

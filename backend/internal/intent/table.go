package intent

import (
	"fmt"
	"strings"
)

// Table 把整張轉移表印成固定格式的純文字。testdata/transitions.golden 存的就是這份輸出，
// 測試會逐字比對：想改任何一條轉移規則，就得連 golden file 一起改，diff 裡一眼看得到。
//
// 為什麼用文字而不用 JSON：這張表是給人 review 的，不是給程式讀的。
func Table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-13s %-13s %-24s %s\n", "FROM", "TO", "BY", "NEEDS")
	for _, r := range Rules() {
		bys := make([]string, len(r.By))
		for i, a := range r.By {
			bys[i] = string(a)
		}
		var needs []string
		if r.NeedsTxHash {
			needs = append(needs, "tx_hash")
		}
		if r.NeedsReason {
			needs = append(needs, "reason")
		}
		if len(needs) == 0 {
			needs = []string{"-"}
		}
		fmt.Fprintf(&b, "%-13s %-13s %-24s %s\n", r.From, r.To, strings.Join(bys, ","), strings.Join(needs, ","))
	}
	fmt.Fprintf(&b, "\nterminal: ")
	var terms []string
	for _, s := range States() {
		if s.Terminal() {
			terms = append(terms, string(s))
		}
	}
	b.WriteString(strings.Join(terms, ", "))
	b.WriteString("\n")
	return b.String()
}

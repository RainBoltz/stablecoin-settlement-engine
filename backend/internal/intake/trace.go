package intake

import (
	"fmt"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
)

// Trace 回報第 index 批的那幾項各自來自檔案第幾行。批次在鏈上是全成或全敗，
// 所以一批失敗就等於「這幾行沒有付出去」，而 operator 手上只有那份檔案。
//
// 這個對應只是把兩份順序疊在一起數過去，能這樣數是因為 bulk.Pack 不重排也不丟項；
// 哪天有人把 Pack 換成裝箱演算法，這裡會安靜地給錯答案，所以下面那兩個檢查不能省。
func (r Run) Trace(p bulk.Plan, index int) (Trace, error) {
	if p.Payouts != len(r.Accepted) {
		return Trace{}, fmt.Errorf("%w: the plan holds %d payouts, this run accepted %d",
			ErrPlanMismatch, p.Payouts, len(r.Accepted))
	}
	if index < 1 || index > len(p.Batches) {
		return Trace{}, fmt.Errorf("%w: batch #%d of %d", ErrNoSuchBatch, index, len(p.Batches))
	}

	at := 0
	for _, b := range p.Batches[:index-1] {
		at += len(b.Items)
	}
	batch := p.Batches[index-1]
	if at+len(batch.Items) > len(r.Lines) {
		return Trace{}, fmt.Errorf("%w: batch #%d ends past the run", ErrPlanMismatch, index)
	}

	// 這裡再對一次 merchant，是因為上面那兩個檢查擋得住「數量不一樣」，
	// 擋不住「數量一樣但順序變了」，而順序變了正是最難發現的那一種。
	lines := make([]int, len(batch.Items))
	for i, it := range batch.Items {
		if it.Merchant != r.Accepted[at+i].Merchant {
			return Trace{}, fmt.Errorf("%w: batch #%d item %d is %q, the run has %q",
				ErrPlanMismatch, index, i+1, it.Merchant, r.Accepted[at+i].Merchant)
		}
		lines[i] = r.Lines[at+i]
	}
	return Trace{Batch: index, Lines: lines}, nil
}

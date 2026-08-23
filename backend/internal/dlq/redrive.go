package dlq

import (
	"context"
	"fmt"
	"time"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/queue"
)

// Redrive 把一份停著的 job 原封不動放回 queue，然後把那筆紀錄標成 redriven。
//
// 順序是「先放回、再標記」，跟系列裡其他地方的「帳先動、狀態後走」剛好相反，因為這兩步重放起來的行為相反：
// 同 ID 的 job 還在 queue 裡的話 Enqueue 是 no-op，而 Resolve 是 CAS，只有第一次會成功。所以
// 先放回、死在中間的話紀錄還是 parked，人再按一次，Enqueue 撞回 no-op、Resolve 成功，結果一樣；
// 先標記、死在中間的話紀錄說已經放回去了，job 卻不在 queue 上，那份工作從此沒有人記得。
// 兩種壞法比起來，多一張便條比少一份工作便宜。
//
// 兩個人同時按下去也是同一條路：兩邊都 Enqueue（其中一次是 no-op，或者那份 job 早就做完了、
// 於是變成新的一份工作），然後只有一個人的 Resolve 會成功，另一個拿到 ErrNotParked。多出來的那一份工作不危險，
// 因為 worker 重讀 intent 之後只會換到一次 no-op。
//
// 所以「放回去不會讓錢多動一次」不是這個函式保證的，是 worker 保證的：那份 job 只帶 intent id 與 ref，
// 領到它的 worker 會重讀 intent、照它現在的狀態決定要做什麼（見 relayer.Worker.process）。
func Redrive(ctx context.Context, s Store, q queue.Queue, jobID, by string, now time.Time) (Record, error) {
	r, err := s.Get(ctx, jobID)
	if err != nil {
		return Record{}, err
	}
	// 先問一次「它還停著嗎」，才不會把一份已經被 Drop 掉的 job 放回 queue。真正的把關是下面的 Resolve，
	// 這裡只是不要白做一次 Enqueue。
	if r.Status != StatusParked {
		return Record{}, fmt.Errorf("%w: %s is %s", ErrNotParked, jobID, r.Status)
	}
	if _, err := q.Enqueue(ctx, r.Job, now); err != nil {
		return Record{}, err
	}
	return s.Resolve(ctx, jobID, StatusRedriven, by, now)
}

// Drop 承認一份停著的 job 沒有用了，不放回 queue。
//
// 它跟 Redrive 一樣不碰那筆 intent：丟掉的是便條，不是付款。一筆停在 needs_review 的付款不會因為便條被丟掉就結案，
// 它還在等人走轉移表上那條路，而那條路只有 operator 走得動。
func Drop(ctx context.Context, s Store, jobID, by string, now time.Time) (Record, error) {
	return s.Resolve(ctx, jobID, StatusDropped, by, now)
}

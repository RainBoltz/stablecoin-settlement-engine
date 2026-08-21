package relayer

import (
	"context"
	"sync"
	"time"
)

// Limiter 是 worker 在對一筆 intent 動手之前要過的關：Acquire 拿到一個「送出的名額」才能寫 settling、記 hold、送出，
// Release 在這份 job 處理完（送成、送失敗都算）之後歸還。
//
// 它擋的位置是刻意的：在任何副作用之前。worker 數是「同時有幾份 job 在手上」，名額數是「同時有幾筆交易在往 RPC 送」，
// 兩個數字要能分開調（八個 worker、兩條連線到 RPC 是很正常的組合）；而被擋住的那一刻什麼都還沒寫，放手最便宜。
// 名額若擋在 Sender.Send 裡面，被擋住的 job 已經是 settling 加 hold，重來的 worker 在 settling 那一格又不重送，
// 一個單純的限流等待就會把一筆好好的 intent 送進 needs_review。
type Limiter interface {
	// Acquire 等到一個名額，或 ctx 結束。回錯代表沒拿到，呼叫端不可以 Release。
	Acquire(ctx context.Context) error
	// Release 歸還 Acquire 拿到的名額。
	Release()
}

// Unlimited 是不限流的 Limiter，Worker 的預設值。
type Unlimited struct{}

// Acquire 實作 Limiter：永遠立刻放行。
func (Unlimited) Acquire(context.Context) error { return nil }

// Release 實作 Limiter。
func (Unlimited) Release() {}

// Throttle 是給 RPC 用的 Limiter，兩個旋鈕：
//
//   - MaxInFlight：同時在往外送的筆數上限。實作是一個容量固定的 channel 當 semaphore
//     （Effective Go 的寫法，https://go.dev/doc/effective_go#channels）。
//   - PerSecond 與 Burst：每秒最多送幾筆、一開始可以連送幾筆。實作是一個 token bucket：桶子容量 Burst、
//     每秒補 PerSecond 個 token，拿不到 token 就照缺口算要等多久。跟 golang.org/x/time/rate 的 Reserve 同一個概念
//     （https://pkg.go.dev/golang.org/x/time/rate），這裡自己寫四十行是為了讓 backend 維持零外部依賴，
//     也為了讓時鐘與睡眠可以被測試換掉。
//
// 為什麼兩個旋鈕都要：公開的 RPC 服務商限的是「每秒多少請求」（例如 Alchemy 的 compute units per second，
// https://www.alchemy.com/docs/reference/throughput，超過就回 429），而節點本身怕的是「同時多少條連線」。
// 只限其中一個，另一個遲早被打爆。
//
// 任一個旋鈕設 0 代表那一項不限。
type Throttle struct {
	sem chan struct{} // nil 代表不限同時筆數

	mu     sync.Mutex
	rate   float64 // 每秒補幾個 token，0 代表不限速
	burst  float64
	tokens float64 // 可以是負的：代表已經有人預支、正在等
	last   time.Time

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// ThrottleOption 調整 Throttle 的預設值。
type ThrottleOption func(*Throttle)

// WithThrottleClock 換掉時鐘與睡眠，測試用：sleep 收到「要等多久」之後可以直接撥時鐘，不用真的等。
func WithThrottleClock(now func() time.Time, sleep func(ctx context.Context, d time.Duration) error) ThrottleOption {
	return func(t *Throttle) { t.now, t.sleep = now, sleep }
}

// NewThrottle 建立一個 Throttle。maxInFlight 與 perSecond 任一個是 0 代表那一項不限；burst 小於 1 時視為 1
// （至少要能送一筆，不然永遠拿不到 token）。桶子一開始是滿的。
func NewThrottle(maxInFlight int, perSecond float64, burst int, opts ...ThrottleOption) *Throttle {
	t := &Throttle{rate: perSecond, burst: float64(max(burst, 1)), now: time.Now, sleep: sleepCtx}
	t.tokens = t.burst
	if maxInFlight > 0 {
		t.sem = make(chan struct{}, maxInFlight)
	}
	for _, o := range opts {
		o(t)
	}
	t.last = t.now()
	return t
}

// Acquire 實作 Limiter。先拿同時名額、再等 token：等 token 的時候占著名額，是為了讓 PerSecond 真的限的是「送出」
// 而不是「排隊」。ctx 在等 token 時結束，名額要還回去，不然名額會越漏越少。
func (t *Throttle) Acquire(ctx context.Context) error {
	if t.sem != nil {
		// 先試不阻塞的一次：ctx 已經結束但名額剛好有空時，select 會隨機挑一邊，這裡不想靠運氣。
		select {
		case t.sem <- struct{}{}:
		default:
			select {
			case t.sem <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := t.waitToken(ctx); err != nil {
		if t.sem != nil {
			<-t.sem
		}
		return err
	}
	return nil
}

// Release 實作 Limiter。
func (t *Throttle) Release() {
	if t.sem != nil {
		<-t.sem
	}
}

// waitToken 從桶子拿一個 token；沒有就預支（tokens 變負）並照缺口算出要等多久，等完才算拿到。
// 預支而不是排隊的好處：不需要條件變數，同時來的幾個等待者各自算出不同的等待時間，自然就錯開了。
// 等到一半 ctx 結束，把預支的還回去。
func (t *Throttle) waitToken(ctx context.Context) error {
	if t.rate <= 0 {
		return nil
	}
	t.mu.Lock()
	now := t.now()
	t.tokens = min(t.burst, t.tokens+now.Sub(t.last).Seconds()*t.rate)
	t.last = now
	t.tokens--
	var wait time.Duration
	if t.tokens < 0 {
		wait = time.Duration(-t.tokens / t.rate * float64(time.Second))
	}
	t.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	if err := t.sleep(ctx, wait); err != nil {
		t.mu.Lock()
		t.tokens++
		t.mu.Unlock()
		return err
	}
	return nil
}

// sleepCtx 睡 d，或 ctx 先結束。
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

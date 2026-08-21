package relayer

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeClock 給 Throttle 用：sleep 不真的等，直接把時鐘撥過去，並記下每一次被要求等多久。
type fakeClock struct {
	t      time.Time
	sleeps []time.Duration
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return nil
}

// TestThrottle_MaxInFlightBlocksTheNextAcquire：兩個名額，第三個 Acquire 要等到有人 Release；等不到就回 ctx 的錯。
// 這是「八個 worker 不等於八條連線打在 RPC 上」的來源。
func TestThrottle_MaxInFlightBlocksTheNextAcquire(t *testing.T) {
	th := NewThrottle(2, 0, 0)
	ctx := context.Background()
	if err := th.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := th.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := th.Acquire(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third acquire: want deadline exceeded, got %v", err)
	}
	th.Release()
	if err := th.Acquire(ctx); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// TestThrottle_PerSecondSpacesSends：每秒 2 筆、burst 1。第一筆不用等，之後每一筆都得等 500ms，等待是照缺口算的，
// 不是固定睡一段。
func TestThrottle_PerSecondSpacesSends(t *testing.T) {
	clock := &fakeClock{t: t0}
	th := NewThrottle(0, 2, 1, WithThrottleClock(clock.now, clock.sleep))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := th.Acquire(ctx); err != nil {
			t.Fatal(err)
		}
		th.Release()
	}
	if len(clock.sleeps) != 2 || clock.sleeps[0] != 500*time.Millisecond || clock.sleeps[1] != 500*time.Millisecond {
		t.Fatalf("sleeps: %v", clock.sleeps)
	}
	// 閒置兩秒之後桶子又滿了（但最多 burst 個），下一筆不用等。
	clock.t = clock.t.Add(2 * time.Second)
	if err := th.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if len(clock.sleeps) != 2 {
		t.Fatalf("a full bucket should not sleep: %v", clock.sleeps)
	}
}

// TestThrottle_BurstLetsTheFirstFewThrough：burst 3、每秒 1 筆：前三筆連送不用等，第四筆等一整秒。
func TestThrottle_BurstLetsTheFirstFewThrough(t *testing.T) {
	clock := &fakeClock{t: t0}
	th := NewThrottle(0, 1, 3, WithThrottleClock(clock.now, clock.sleep))
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := th.Acquire(ctx); err != nil {
			t.Fatal(err)
		}
		th.Release()
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != time.Second {
		t.Fatalf("sleeps: %v", clock.sleeps)
	}
}

// TestThrottle_CanceledWaitGivesBackSlotAndToken：等 token 等到一半 ctx 結束，同時名額與預支的 token 都要還回去，
// 不然每一次放棄都會讓 pool 永久少一個名額。
func TestThrottle_CanceledWaitGivesBackSlotAndToken(t *testing.T) {
	clock := &fakeClock{t: t0}
	th := NewThrottle(1, 1, 1, WithThrottleClock(clock.now, clock.sleep))
	ctx := context.Background()
	if err := th.Acquire(ctx); err != nil { // 吃掉唯一的 token
		t.Fatal(err)
	}
	th.Release()

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := th.Acquire(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if len(th.sem) != 0 {
		t.Fatalf("slot leaked: %d in use", len(th.sem))
	}
	if th.tokens != 0 {
		t.Fatalf("token not refunded: %v", th.tokens)
	}
	// 一秒後正常拿得到，代表桶子沒有被放棄的那次弄壞。
	clock.t = clock.t.Add(time.Second)
	if err := th.Acquire(ctx); err != nil || len(clock.sleeps) != 0 {
		t.Fatalf("after refund: err=%v sleeps=%v", err, clock.sleeps)
	}
}

// TestThrottle_ZeroMeansUnlimited：兩個旋鈕都是 0 就是 Unlimited 的行為，連續拿一百個名額都不等。
func TestThrottle_ZeroMeansUnlimited(t *testing.T) {
	clock := &fakeClock{t: t0}
	th := NewThrottle(0, 0, 0, WithThrottleClock(clock.now, clock.sleep))
	for i := 0; i < 100; i++ {
		if err := th.Acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("unexpected sleeps: %v", clock.sleeps)
	}
}

package intent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestMemoryStore_SaveIsCompareAndSwap：兩個 worker 各自讀到同一個版本、各自推進，只有一個寫得回去。
// 這就是「兩個 relayer worker 同時搶到同一筆 intent」的長相；輸的那個拿到 ErrVersionConflict，
// 重讀之後會發現它已經在 settling，於是放手。
func TestMemoryStore_SaveIsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	it := newTestIntent(t)
	if err := s.Save(ctx, it, 0); err != nil {
		t.Fatal(err)
	}
	drive(t, it, StateAuthorized)
	if err := s.Save(ctx, it, 1); err != nil {
		t.Fatal(err)
	}

	w1, _ := s.Get(ctx, it.ID)
	w2, _ := s.Get(ctx, it.ID)
	if w1.Version != 2 || w2.Version != 2 {
		t.Fatalf("both workers should see version 2, got %d and %d", w1.Version, w2.Version)
	}
	mustApply(t, w1, Request{To: StateSettling, By: ActorRelayer, At: t0})
	mustApply(t, w2, Request{To: StateSettling, By: ActorRelayer, At: t0})

	if err := s.Save(ctx, w1, 2); err != nil {
		t.Fatalf("first writer should win: %v", err)
	}
	if err := s.Save(ctx, w2, 2); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("second writer should lose with ErrVersionConflict, got %v", err)
	}
	stored, _ := s.Get(ctx, it.ID)
	if stored.Version != 3 || stored.State != StateSettling || len(stored.History) != 2 {
		t.Fatalf("stored intent is wrong: %+v", stored)
	}
}

// TestMemoryStore_GetReturnsACopy：拿到的是拷貝，改它不會繞過 Apply 動到存的那份。
func TestMemoryStore_GetReturnsACopy(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	it := newTestIntent(t)
	_ = s.Save(ctx, it, 0)

	got, _ := s.Get(ctx, it.ID)
	got.State = StateSettled
	got.History = append(got.History, Transition{From: StateCreated, To: StateSettled})

	again, _ := s.Get(ctx, it.ID)
	if again.State != StateCreated || len(again.History) != 0 {
		t.Fatalf("store leaked its internal pointer: %+v", again)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.Save(ctx, &Intent{ID: "new"}, 5); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("saving a new intent with expectedVersion != 0 must conflict, got %v", err)
	}
}

// TestAdvance_ReadApplySave：標準寫法的三種結果：真的推進了、重放沒動、CAS 輸了。
func TestAdvance_ReadApplySave(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	it := newTestIntent(t)
	_ = s.Save(ctx, it, 0)

	got, applied, err := Advance(ctx, s, it.ID, Request{To: StateAuthorized, By: ActorAPI, At: t0.Add(time.Minute)})
	if err != nil || !applied || got.State != StateAuthorized || got.Version != 2 {
		t.Fatalf("first transition: applied=%v err=%v got=%+v", applied, err, got)
	}

	got, applied, err = Advance(ctx, s, it.ID, Request{To: StateAuthorized, By: ActorAPI, At: t0.Add(2 * time.Minute)})
	if err != nil || applied || got.Version != 2 {
		t.Fatalf("replay through store: applied=%v err=%v version=%d", applied, err, got.Version)
	}

	// 模擬別人在我們讀完之後先寫了一版
	other, _ := s.Get(ctx, it.ID)
	mustApply(t, other, Request{To: StateCanceled, By: ActorAPI, Reason: "merchant canceled", At: t0})
	_ = s.Save(ctx, other, 2)

	// 一個 relayer 手上還拿著舊的讀值想推 settling：Advance 會重新 Get，看到已 canceled，拿到 ErrTerminal
	_, _, err = Advance(ctx, s, it.ID, Request{To: StateSettling, By: ActorRelayer, At: t0})
	if !errors.Is(err, ErrTerminal) {
		t.Fatalf("want ErrTerminal after cancel, got %v", err)
	}
	if _, _, err := Advance(ctx, s, "nope", Request{To: StateAuthorized, By: ActorAPI}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

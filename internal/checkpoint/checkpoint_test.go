package checkpoint

import (
	"context"
	"errors"
	"testing"
	"time"
)

type state struct{ Tick int }

// A cold game leaves memory but not the store, so whoever comes back finds it intact.
func TestColdGamesLeaveMemoryAndComeBackFromTheStore(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(1_000_000, 0)
	k := NewKeeper[state](NewMemoryStore(), "test")
	k.now = func() time.Time { return clock }

	if err := k.Create(ctx, "abandoned", &state{Tick: 7}); err != nil {
		t.Fatal(err)
	}
	if err := k.Create(ctx, "active", &state{Tick: 1}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(coldAfter - time.Minute)
	if err := k.Update(ctx, "active", func(s *state) error { s.Tick++; return nil }); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(coldAfter/2 + time.Minute) // sweeps are throttled, so wait for the next one
	if err := k.Update(ctx, "active", func(s *state) error { s.Tick++; return nil }); err != nil {
		t.Fatal(err)
	}

	k.mu.Lock()
	_, abandonedInMemory := k.games["abandoned"]
	_, activeInMemory := k.games["active"]
	k.mu.Unlock()
	if abandonedInMemory || !activeInMemory {
		t.Fatalf("in memory: abandoned=%v active=%v", abandonedInMemory, activeInMemory)
	}

	var tick int
	if err := k.View(ctx, "abandoned", func(s *state) error { tick = s.Tick; return nil }); err != nil {
		t.Fatal(err)
	}
	if tick != 7 {
		t.Fatalf("came back at tick %d, want 7", tick)
	}
}

// Dropping an entry somebody holds would give the game two diverging copies.
func TestSweepLeavesAnEntryInUse(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(1_000_000, 0)
	k := NewKeeper[state](NewMemoryStore(), "test")
	k.now = func() time.Time { return clock }
	if err := k.Create(ctx, "busy", &state{}); err != nil {
		t.Fatal(err)
	}
	k.games["busy"].mu.Lock()
	defer k.games["busy"].mu.Unlock()

	clock = clock.Add(coldAfter)
	k.mu.Lock()
	k.sweep()
	_, ok := k.games["busy"]
	k.mu.Unlock()
	if !ok {
		t.Fatal("swept an entry that was locked")
	}
}

// `lgs dev` on a small public box has no Redis TTL to rely on.
func TestMemoryStoreExpiresLikeRedis(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(1_000_000, 0)
	m := NewMemoryStore()
	m.now = func() time.Time { return clock }
	if err := m.Save(ctx, "old", []byte("x")); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(retention - time.Second)
	if raw, _ := m.Load(ctx, "old"); raw == nil {
		t.Fatal("expired too soon")
	}
	clock = clock.Add(time.Second)
	if raw, _ := m.Load(ctx, "old"); raw != nil {
		t.Fatal("still there after retention")
	}

	k := NewKeeper[state](m, "test")
	err := k.View(ctx, "old", func(*state) error { return nil })
	if !errors.Is(err, ErrNoGame) {
		t.Fatalf("got %v, want ErrNoGame", err)
	}

	if err := m.Save(ctx, "new", []byte("y")); err != nil { // the sweep runs on save
		t.Fatal(err)
	}
	if _, ok := m.data["old"]; ok {
		t.Fatal("expired data is still taking up memory")
	}
}

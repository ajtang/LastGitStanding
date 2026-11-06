// Package checkpoint gives each service per-game state that survives a pod
// restart without giving each service its own persistence layer.
//
// State is plain JSON in Redis, written through on every mutation. That makes
// Redis a hard dependency of the tick loop, which is ironic given what the game
// teaches about Redis. See ADR-0003 before you "fix" it.
package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrNoGame = errors.New("no such game")

const (
	// retention is how long an untouched game stays in the store. Nobody has ever come
	// back to a tab that has been open for a week, whatever they tell themselves.
	retention = 7 * 24 * time.Hour
	// coldAfter is when a Keeper drops its in-memory copy. The store still has the game,
	// so a late return costs one load.
	coldAfter = 30 * time.Minute
)

type Store interface {
	Load(ctx context.Context, key string) ([]byte, error) // nil, nil when absent
	Save(ctx context.Context, key string, val []byte) error
}

// Keeper serializes access per game and hides the load/save dance. Mutating
// handlers are short and run sequentially for a given game, so one lock is fine.
type Keeper[T any] struct {
	store Store
	ns    string
	now   func() time.Time
	mu    sync.Mutex
	games map[string]*entry[T]
	swept time.Time
}

type entry[T any] struct {
	mu   sync.Mutex
	val  *T
	used time.Time // guarded by the Keeper's mu, not this one
}

func NewKeeper[T any](store Store, namespace string) *Keeper[T] {
	return &Keeper[T]{store: store, ns: namespace, now: time.Now, games: map[string]*entry[T]{}}
}

func (k *Keeper[T]) key(id string) string { return "lgs:" + k.ns + ":" + id }

func (k *Keeper[T]) Create(ctx context.Context, id string, val *T) error {
	k.mu.Lock()
	k.games[id] = &entry[T]{val: val, used: k.now()}
	k.mu.Unlock()
	return k.save(ctx, id, val)
}

// Update runs fn with exclusive access and writes through if it succeeds.
func (k *Keeper[T]) Update(ctx context.Context, id string, fn func(*T) error) error {
	e, err := k.entry(ctx, id)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := fn(e.val); err != nil {
		return err
	}
	return k.save(ctx, id, e.val)
}

// View is Update without the write. The state must not be retained or mutated.
func (k *Keeper[T]) View(ctx context.Context, id string, fn func(*T) error) error {
	e, err := k.entry(ctx, id)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return fn(e.val)
}

// Forget drops the in-memory copy. Tests use it to play the part of a restarted
// pod; the next access has to come back from the store.
func (k *Keeper[T]) Forget(id string) {
	k.mu.Lock()
	delete(k.games, id)
	k.mu.Unlock()
}

func (k *Keeper[T]) entry(ctx context.Context, id string) (*entry[T], error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.sweep()
	if e, ok := k.games[id]; ok {
		e.used = k.now()
		return e, nil
	}
	raw, err := k.store.Load(ctx, k.key(id))
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", k.key(id), err)
	}
	if raw == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoGame, id)
	}
	var val T
	if err := json.Unmarshal(raw, &val); err != nil {
		return nil, fmt.Errorf("decode %s: %w", k.key(id), err)
	}
	e := &entry[T]{val: &val, used: k.now()}
	k.games[id] = e
	return e, nil
}

// sweep must be called with k.mu held. It runs on access rather than on a timer so
// that every service gets it for free, and an idle Keeper isn't growing anyway.
func (k *Keeper[T]) sweep() {
	now := k.now()
	if now.Sub(k.swept) < coldAfter/2 {
		return
	}
	k.swept = now
	for id, e := range k.games {
		// A holder of e.mu fetched it recently enough not to be cold, unless it has been
		// stuck for half an hour. Dropping that entry would fork the game, so leave it.
		if now.Sub(e.used) >= coldAfter && e.mu.TryLock() {
			delete(k.games, id)
			e.mu.Unlock()
		}
	}
}

func (k *Keeper[T]) save(ctx context.Context, id string, val *T) error {
	raw, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("encode %s: %w", k.key(id), err)
	}
	return k.store.Save(ctx, k.key(id), raw)
}

// MemoryStore is for tests and `lgs dev`. It survives Forget and nothing else.
type MemoryStore struct {
	now   func() time.Time
	mu    sync.Mutex
	data  map[string]stored
	swept time.Time
}

type stored struct {
	val   []byte
	saved time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{now: time.Now, data: map[string]stored{}}
}

func (m *MemoryStore) Load(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.data[key]
	if !ok || m.now().Sub(s.saved) >= retention {
		return nil, nil
	}
	return s.val, nil
}

func (m *MemoryStore) Save(_ context.Context, key string, val []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if now.Sub(m.swept) >= time.Hour {
		m.swept = now
		for k, s := range m.data {
			if now.Sub(s.saved) >= retention {
				delete(m.data, k)
			}
		}
	}
	m.data[key] = stored{val: val, saved: now}
	return nil
}

type RedisStore struct{ client *redis.Client }

func NewRedisStore(client *redis.Client) *RedisStore { return &RedisStore{client: client} }

func (r *RedisStore) Load(ctx context.Context, key string) ([]byte, error) {
	raw, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return raw, err
}

func (r *RedisStore) Save(ctx context.Context, key string, val []byte) error {
	return r.client.Set(ctx, key, val, retention).Err()
}

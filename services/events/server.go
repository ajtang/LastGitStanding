// Package events is the slow path: the durable timeline and the "what changed?"
// correlator. It exists as its own service so that writing history to Postgres
// can lag, fail or be redeployed without a single tick noticing.
//
// If this is down, the game keeps playing and the timeline catches up afterwards.
// That property is the reason for the service boundary. Protect it.
package events

import (
	"context"
	"fmt"
	"slices"
	"sync"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

// correlationWindow is how far back we look when something starts going wrong.
// Long enough to catch a rollout's slow burn, short enough to not blame last week.
const correlationWindow = 24

var changeKinds = []string{"flux_applied", "rollout_started", "canary_promoted", "db_resize", "node_pool_replace", "db_promote"}

type Store interface {
	Append(ctx context.Context, gameID string, ev *lgsv1.Event) error
	Recent(ctx context.Context, gameID string, limit int) ([]*lgsv1.Event, error) // newest first
}

type Server struct {
	lgsv1.UnimplementedEventProcessingServer
	store Store
}

func NewServer(store Store) *Server { return &Server{store: store} }

// Handle is the bus handler. Events can arrive twice; a duplicated line in a
// timeline is untidy, not incorrect, so the Postgres store dedupes and that's all.
func (s *Server) Handle(ctx context.Context, gameID string, ev *lgsv1.Event) error {
	if err := s.store.Append(ctx, gameID, ev); err != nil {
		return err
	}
	if ev.GetKind() != "incident_detected" {
		return nil
	}
	recent, err := s.store.Recent(ctx, gameID, 200)
	if err != nil {
		return err
	}
	suspect := findSuspect(recent, ev.GetTick())
	if suspect == nil {
		return nil
	}
	// This is a correlation and it's phrased as one. It will sometimes point at an
	// innocent commit, the same way your real change-event overlay does.
	return s.store.Append(ctx, gameID, &lgsv1.Event{
		Tick: ev.GetTick(), Source: "events", Kind: "suspect_change", Severity: "info", Ref: ev.GetRef(),
		Message: fmt.Sprintf("%d ticks before %s: %s", ev.GetTick()-suspect.GetTick(), ev.GetRef(), suspect.GetMessage()),
	})
}

func findSuspect(recent []*lgsv1.Event, tick int32) *lgsv1.Event {
	for _, ev := range recent { // newest first, so the closest change wins
		if ev.GetTick() > tick || tick-ev.GetTick() > correlationWindow {
			continue
		}
		if slices.Contains(changeKinds, ev.GetKind()) {
			return ev
		}
	}
	return nil
}

func (s *Server) Timeline(ctx context.Context, req *lgsv1.TimelineRequest) (*lgsv1.TimelineResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	evs, err := s.store.Recent(ctx, req.GetGameId(), limit)
	if err != nil {
		return nil, err
	}
	return &lgsv1.TimelineResponse{Events: evs}, nil
}

type MemoryStore struct {
	mu    sync.Mutex
	games map[string][]*lgsv1.Event
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{games: map[string][]*lgsv1.Event{}} }

func (m *MemoryStore) Append(_ context.Context, gameID string, ev *lgsv1.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.games[gameID] = append(m.games[gameID], ev)
	return nil
}

func (m *MemoryStore) Recent(_ context.Context, gameID string, limit int) ([]*lgsv1.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs := m.games[gameID]
	out := make([]*lgsv1.Event, 0, min(limit, len(evs)))
	for i := len(evs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, evs[i])
	}
	return out, nil
}

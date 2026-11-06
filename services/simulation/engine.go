// Package simulation is the conductor. It owns the clock and the order of
// operations, plus the flow model, and it holds no one else's data.
//
// A tick is a synchronous barrier across services (ADR-0002): infrastructure,
// then incidents, then flow, then economy, then observability. If any of them
// fails, the tick is abandoned and retried, and the world stands still. That's
// a correct failure mode for a game, and the owning services make it safe by
// treating a repeated tick number as a replay.
package simulation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/bus"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
	"github.com/ajtang/LastGitStanding/services/simulation/flow"
)

// stepTimeout is the budget for one service in the barrier. On a healthy cluster
// these calls take a millisecond. If one takes 800, something is wrong and pausing
// is better than letting ticks stack up behind it.
const stepTimeout = 800 * time.Millisecond

// idleAfter is how long a game's clock keeps running with nobody watching. Long enough
// to survive a refresh or a dropped connection, and short enough that a closed tab
// isn't burning a slot, or quietly losing the player's company while they're at lunch.
const idleAfter = 2 * time.Minute

type Deps struct {
	Infra   lgsv1.InfrastructureClient
	Incid   lgsv1.IncidentsClient
	Econ    lgsv1.EconomyClient
	Obs     lgsv1.ObservabilityClient
	Players lgsv1.PlayerStateClient
	Bus     bus.Bus
	Store   checkpoint.Store
	// MaxGames limits how many clocks can run at once. Zero means no limit.
	MaxGames int
}

// runState is the part of a game that belongs to this service.
type runState struct {
	Seed   int64               `json:"seed"`
	Tick   int32               `json:"tick"`
	Flow   *flow.State         `json:"flow"`
	Frame  *lgsv1.Frame        `json:"frame"`
	Firing []*lgsv1.AlertState `json:"firing"`
	Over   bool                `json:"over"`
}

type game struct {
	id       string
	speed    int32
	cancel   context.CancelFunc
	wake     chan struct{}
	mu       sync.Mutex
	watchers map[chan *lgsv1.Frame]struct{}
	lastSeen time.Time
}

type Engine struct {
	lgsv1.UnimplementedSimulationServer
	deps  Deps
	runs  *checkpoint.Keeper[runState]
	mu    sync.Mutex
	games map[string]*game
}

func New(deps Deps) *Engine {
	return &Engine{
		deps:  deps,
		runs:  checkpoint.NewKeeper[runState](deps.Store, "sim"),
		games: map[string]*game{},
	}
}

func (e *Engine) StartGame(ctx context.Context, req *lgsv1.StartGameRequest) (*lgsv1.StartGameResponse, error) {
	id := req.GetGameId()
	if e.full() {
		return nil, status.Error(codes.ResourceExhausted, "Too many games are running right now. Try again in a few minutes.")
	}
	init := &lgsv1.InitGame{GameId: id, Seed: req.GetSeed()}
	infra, err := e.deps.Infra.Init(ctx, init)
	if err != nil {
		return nil, fmt.Errorf("init infrastructure: %w", err)
	}
	if _, err := e.deps.Incid.Init(ctx, init); err != nil {
		return nil, fmt.Errorf("init incidents: %w", err)
	}
	econ, err := e.deps.Econ.Init(ctx, init)
	if err != nil {
		return nil, fmt.Errorf("init economy: %w", err)
	}
	frame := &lgsv1.Frame{GameId: id, Topology: infra.GetTopology(), Ledger: econ.GetLedger(), Health: &lgsv1.Health{}, Speed: req.GetSpeed()}
	if err := e.runs.Create(ctx, id, &runState{Seed: req.GetSeed(), Flow: flow.NewState(), Frame: frame}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "checkpoint: %v", err)
	}
	e.attach(id, req.GetSpeed())
	return &lgsv1.StartGameResponse{Frame: frame}, nil
}

// attach starts the game's clock. Stopping a game means cancelling this loop;
// it holds nothing that isn't already in the checkpoint.
func (e *Engine) attach(id string, speed int32) *game {
	e.mu.Lock()
	defer e.mu.Unlock()
	if g, ok := e.games[id]; ok {
		g.mu.Lock()
		g.lastSeen = time.Now()
		g.mu.Unlock()
		return g
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &game{id: id, speed: speed, cancel: cancel, wake: make(chan struct{}, 1), watchers: map[chan *lgsv1.Frame]struct{}{}, lastSeen: time.Now()}
	e.games[id] = g
	go e.loop(ctx, g)
	return g
}

func (e *Engine) full() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deps.MaxGames > 0 && len(e.games) >= e.deps.MaxGames
}

// ReapIdle stops abandoned clocks until ctx ends.
func (e *Engine) ReapIdle(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if n := e.Reap(now); n > 0 {
				slog.Info("stopped idle games", "count", n)
			}
		}
	}
}

// Reap stops the clock of every game nobody has watched or touched for idleAfter, and
// reports how many it stopped. Only the clock goes: the state is in the checkpoint, so
// coming back finds the game exactly where it was, paused.
func (e *Engine) Reap(now time.Time) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	reaped := 0
	for id, g := range e.games {
		g.mu.Lock()
		idle := len(g.watchers) == 0 && now.Sub(g.lastSeen) >= idleAfter
		g.mu.Unlock()
		if idle {
			g.cancel()
			delete(e.games, id)
			reaped++
		}
	}
	return reaped
}

// touch counts a command as attention, for anyone playing without a stream open.
func (e *Engine) touch(id string) {
	e.mu.Lock()
	g := e.games[id]
	e.mu.Unlock()
	if g == nil {
		return
	}
	g.mu.Lock()
	g.lastSeen = time.Now()
	g.mu.Unlock()
}

func (e *Engine) loop(ctx context.Context, g *game) {
	for {
		g.mu.Lock()
		speed := g.speed
		g.mu.Unlock()
		if speed <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-g.wake:
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-g.wake:
			continue
		case <-time.After(time.Second / time.Duration(speed)):
		}
		frame, err := e.Step(ctx, g.id)
		if err != nil {
			// Whichever service failed is having a bad day. The tick didn't happen, so
			// the only thing to do is try the same tick again next interval.
			slog.Warn("tick stalled", "game", g.id, "err", err)
			continue
		}
		if frame.GetLedger().GetGameOverReason() != "" {
			e.setSpeed(g, 0)
		}
	}
}

// Step runs one tick. Public so headless players and tests can drive the world
// without a clock, through the same real service calls as a live game.
func (e *Engine) Step(ctx context.Context, id string) (*lgsv1.Frame, error) {
	var frame *lgsv1.Frame
	var events []*lgsv1.Event
	var finish *lgsv1.FinishRunRequest

	err := e.runs.Update(ctx, id, func(run *runState) error {
		if run.Over {
			frame = run.Frame
			return nil
		}
		tick := run.Tick + 1
		last := run.Frame

		call := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, stepTimeout) }

		cctx, cancel := call()
		var regionsDown []string
		if last.GetEffects() != nil {
			regionsDown = last.GetEffects().GetRegionsDown()
		}
		infra, err := e.deps.Infra.Reconcile(cctx, &lgsv1.ReconcileRequest{
			GameId: id, Tick: tick, RegionsDown: regionsDown,
			OomKilledPods:         last.GetHealth().GetOomKilledPods(),
			WorstVersionErrorRate: last.GetHealth().GetWorstVersionErrorRate(),
		})
		cancel()
		if err != nil {
			return fmt.Errorf("infrastructure: %w", err)
		}

		cctx, cancel = call()
		inc, err := e.deps.Incid.Advance(cctx, &lgsv1.AdvanceRequest{
			GameId: id, Tick: tick, Topology: infra.GetTopology(),
			LastHealth: last.GetHealth(), LastLedger: last.GetLedger(), FiringAlerts: run.Firing,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("incidents: %w", err)
		}

		// Flow mutates its state in place, so a failure further down the barrier
		// must not re-apply it on retry. Work on a copy until the tick commits.
		flowState := cloneFlow(run.Flow)
		health := flow.Step(flowState, flow.Input{
			Seed: run.Seed, Tick: tick, Users: last.GetLedger().GetUsers(),
			Topology: infra.GetTopology(), Effects: inc.GetEffects(),
		})

		open := int32(0)
		for _, i := range inc.GetIncidents() {
			if i.GetResolvedTick() == 0 && i.GetDetectedTick() != 0 {
				open++
			}
		}
		cctx, cancel = call()
		econ, err := e.deps.Econ.Settle(cctx, &lgsv1.SettleRequest{
			GameId: id, Tick: tick, Topology: infra.GetTopology(), Health: health,
			Effects: inc.GetEffects(), OpenIncidents: open,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("economy: %w", err)
		}

		events = append(events, infra.GetEvents()...)
		events = append(events, inc.GetEvents()...)
		events = append(events, econ.GetEvents()...)

		next := &lgsv1.Frame{
			GameId: id, Tick: tick, Speed: e.speedOf(id), Health: health, Ledger: econ.GetLedger(),
			Topology: infra.GetTopology(), Effects: inc.GetEffects(), Incidents: inc.GetIncidents(),
			Gitops: infra.GetGitops(),
		}

		cctx, cancel = call()
		obs, err := e.deps.Obs.Ingest(cctx, next)
		cancel()
		if err != nil {
			return fmt.Errorf("observability: %w", err)
		}
		events = append(events, obs.GetEvents()...)
		next.Alerts = obs.GetFiring()
		next.Events = events

		run.Tick, run.Flow, run.Frame, run.Firing = tick, flowState, next, obs.GetFiring()
		frame = next

		if reason := econ.GetLedger().GetGameOverReason(); reason != "" {
			run.Over = true
			finish = &lgsv1.FinishRunRequest{
				GameId: id, Tick: tick, Score: econ.GetLedger().GetScore(), PeakUsers: econ.GetLedger().GetUsers(),
				Outcome: reason, Won: econ.GetLedger().GetWon(),
			}
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	// Past here the tick has happened. Everything below is best-effort: a Redis blip
	// costs a line in the timeline, and it must never cost a tick.
	if len(events) > 0 {
		if err := e.deps.Bus.Publish(ctx, id, events); err != nil {
			slog.Warn("publishing events", "game", id, "err", err)
		}
	}
	if finish != nil {
		if _, err := e.deps.Players.FinishRun(ctx, finish); err != nil {
			slog.Warn("recording finished run", "game", id, "err", err)
		}
	}
	e.broadcast(id, frame)
	return frame, nil
}

func cloneFlow(s *flow.State) *flow.State {
	c := *s
	c.Backlog = make(map[string]float64, len(s.Backlog))
	for k, v := range s.Backlog {
		c.Backlog[k] = v
	}
	return &c
}

func (e *Engine) speedOf(id string) int32 {
	e.mu.Lock()
	g := e.games[id]
	e.mu.Unlock()
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.speed
}

func (e *Engine) setSpeed(g *game, speed int32) {
	g.mu.Lock()
	g.speed = speed
	g.mu.Unlock()
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) broadcast(id string, frame *lgsv1.Frame) {
	e.mu.Lock()
	g := e.games[id]
	e.mu.Unlock()
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for ch := range g.watchers {
		select {
		case ch <- frame:
		default:
			// A slow browser misses a frame and catches up on the next one. It doesn't
			// get to slow the game down for itself.
		}
	}
}

func (e *Engine) Watch(req *lgsv1.WatchRequest, stream lgsv1.Simulation_WatchServer) error {
	id := req.GetGameId()
	first, err := e.GetFrame(stream.Context(), req)
	if err != nil {
		return err
	}
	g := e.attach(id, 0)
	ch := make(chan *lgsv1.Frame, 8)
	g.mu.Lock()
	g.watchers[ch] = struct{}{}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.watchers, ch)
		g.lastSeen = time.Now()
		g.mu.Unlock()
	}()

	if err := stream.Send(first); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case f := <-ch:
			if err := stream.Send(f); err != nil {
				return err
			}
		}
	}
}

func (e *Engine) GetFrame(ctx context.Context, req *lgsv1.WatchRequest) (*lgsv1.Frame, error) {
	var frame *lgsv1.Frame
	err := e.runs.View(ctx, req.GetGameId(), func(run *runState) error {
		frame = proto.Clone(run.Frame).(*lgsv1.Frame)
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	// The stored frame remembers the speed when it was made. Someone reconnecting
	// wants the speed right now, or a running game shows up looking paused.
	frame.Speed = e.speedOf(req.GetGameId())
	return frame, nil
}

func grpcErr(err error) error {
	if errors.Is(err, checkpoint.ErrNoGame) {
		return status.Error(codes.NotFound, err.Error())
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Unavailable, err.Error())
}

package simulation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/harness"
	"github.com/ajtang/LastGitStanding/services/simulation"
)

// flakyEconomy fails Settle every few calls, after Infrastructure and Incidents have
// already moved on. That's the worst moment in the barrier to lose a service.
type flakyEconomy struct {
	lgsv1.EconomyClient
	calls int
}

func (f *flakyEconomy) Settle(ctx context.Context, in *lgsv1.SettleRequest, opts ...grpc.CallOption) (*lgsv1.SettleResponse, error) {
	f.calls++
	if f.calls%4 == 0 {
		return nil, errors.New("economy is having a day")
	}
	return f.EconomyClient.Settle(ctx, in, opts...)
}

// ADR-0002 in one test: failed barriers get retried and history doesn't notice. If a
// retry double-applied anything (rollouts, flow backlog, billing) these would diverge.
func TestRetriedTicksLeaveNoTrace(t *testing.T) {
	ctx := context.Background()
	const ticks = 2000

	play := func(step func(*harness.Stack) *simulation.Engine) *lgsv1.Frame {
		s := harness.Start()
		defer s.Stop()
		engine := step(s)
		if _, err := engine.StartGame(ctx, &lgsv1.StartGameRequest{GameId: "g", Seed: 11}); err != nil {
			t.Fatal(err)
		}
		var frame *lgsv1.Frame
		for frameTick := int32(0); frameTick < ticks; {
			f, err := engine.Step(ctx, "g")
			if err != nil {
				continue // what the live loop does: same tick, next interval
			}
			frame, frameTick = f, f.GetTick()
			if f.GetLedger().GetGameOverReason() != "" {
				break // an untended game ends around day six, incidents and all
			}
		}
		return frame
	}

	deps := func(s *harness.Stack, econ lgsv1.EconomyClient) *simulation.Engine {
		return simulation.New(simulation.Deps{
			Infra: s.Infra, Incid: s.Incid, Econ: econ, Obs: s.Obs, Players: s.Players, Bus: s.Bus, Store: s.Store,
		})
	}
	clean := play(func(s *harness.Stack) *simulation.Engine { return deps(s, s.Econ) })
	flaky := play(func(s *harness.Stack) *simulation.Engine { return deps(s, &flakyEconomy{EconomyClient: s.Econ}) })

	if !proto.Equal(clean, flaky) {
		t.Fatalf("a flaky service changed history.\nclean ledger: %v\nflaky ledger: %v", clean.GetLedger(), flaky.GetLedger())
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A closed tab shouldn't burn CPU or run the player's company into the ground, and
// coming back has to find the game where it was left.
func TestAbandonedGamesPauseAndCanBeResumed(t *testing.T) {
	ctx := context.Background()
	s := harness.Start()
	defer s.Stop()
	if _, err := s.Engine.StartGame(ctx, &lgsv1.StartGameRequest{GameId: "g", Seed: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Engine.Submit(ctx, &lgsv1.Command{GameId: "g", Action: &lgsv1.Command_Speed{Speed: &lgsv1.SpeedAction{Speed: 8}}}); err != nil {
		t.Fatal(err)
	}
	tickOf := func() int32 {
		f, err := s.Engine.GetFrame(ctx, &lgsv1.WatchRequest{GameId: "g"})
		if err != nil {
			t.Fatal(err)
		}
		return f.GetTick()
	}
	waitFor(t, "the clock to run", func() bool { return tickOf() >= 2 })

	// Being watched protects a game however old its last command is.
	later := func() time.Time { return time.Now().Add(time.Hour) }
	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	stream, err := s.Sim.Watch(watchCtx, &lgsv1.WatchRequest{GameId: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if n := s.Engine.Reap(later()); n != 0 {
		t.Fatalf("reaped %d games with someone watching", n)
	}

	// The player leaving doesn't count until the watcher is actually gone.
	stopWatching()
	waitFor(t, "the reaper to stop the game", func() bool { return s.Engine.Reap(later()) == 1 })

	// Give an in-flight tick time to land, then it must stay put.
	time.Sleep(300 * time.Millisecond)
	left := tickOf()
	time.Sleep(500 * time.Millisecond)
	if now := tickOf(); now != left {
		t.Fatalf("the clock kept running: tick %d became %d", left, now)
	}
	if left < 2 {
		t.Fatalf("progress was lost: back to tick %d", left)
	}

	f, err := s.Engine.GetFrame(ctx, &lgsv1.WatchRequest{GameId: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if f.GetSpeed() != 0 {
		t.Fatalf("came back at speed %d, want paused", f.GetSpeed())
	}
	if _, err := s.Engine.Submit(ctx, &lgsv1.Command{GameId: "g", Action: &lgsv1.Command_Speed{Speed: &lgsv1.SpeedAction{Speed: 8}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the clock to resume", func() bool { return tickOf() > left })
	if _, err := s.Engine.Submit(ctx, &lgsv1.Command{GameId: "g", Action: &lgsv1.Command_Speed{Speed: &lgsv1.SpeedAction{Speed: 0}}}); err != nil {
		t.Fatal(err)
	}
}

// The cap stops new games, never existing ones: nobody gets locked out of a game they
// are already playing because the server got popular.
func TestFullEngineTurnsAwayNewGamesOnly(t *testing.T) {
	ctx := context.Background()
	s := harness.Start()
	defer s.Stop()
	engine := simulation.New(simulation.Deps{Infra: s.Infra, Incid: s.Incid, Econ: s.Econ, Obs: s.Obs, Players: s.Players, Bus: s.Bus, Store: s.Store, MaxGames: 1})

	if _, err := engine.StartGame(ctx, &lgsv1.StartGameRequest{GameId: "first", Seed: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := engine.StartGame(ctx, &lgsv1.StartGameRequest{GameId: "second", Seed: 2})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second game: got %v, want ResourceExhausted", err)
	}
	if _, err := engine.Submit(ctx, &lgsv1.Command{GameId: "first", Action: &lgsv1.Command_Speed{Speed: &lgsv1.SpeedAction{Speed: 0}}}); err != nil {
		t.Fatalf("existing game was turned away: %v", err)
	}

	// Space comes back once the abandoned game has been reaped.
	engine.Reap(time.Now().Add(time.Hour))
	if _, err := engine.StartGame(ctx, &lgsv1.StartGameRequest{GameId: "second", Seed: 2}); err != nil {
		t.Fatalf("after reaping: %v", err)
	}
}

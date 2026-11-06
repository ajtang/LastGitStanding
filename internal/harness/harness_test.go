package harness

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/services/infrastructure"
)

const maxDays = 120

// The most important test in the repo. Everything else can be correct and the
// game can still be pointless; this is what would catch that.
func TestNeglectLoses(t *testing.T) {
	s := Start()
	defer s.Stop()
	for seed := int64(1); seed <= 5; seed++ {
		r, err := Play(context.Background(), s, seed, Neglect{}, maxDays)
		if err != nil {
			t.Fatal(err)
		}
		if r.Won || r.Outcome == "" {
			t.Fatalf("seed %d: an untouched platform survived %0.f days: %+v", seed, r.Days, r)
		}
		t.Logf("seed %d: lost on day %.0f: %s", seed, r.Days, r.Outcome)
	}
}

func TestCompetencePaysOff(t *testing.T) {
	if testing.Short() {
		t.Skip("plays whole games; minutes under -race")
	}
	s := Start()
	defer s.Stop()
	var far int
	for seed := int64(1); seed <= 5; seed++ {
		r, err := Play(context.Background(), s, seed, &Competent{}, maxDays)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("seed %d: day %.0f %s users=%.0f budget=%.0f avail=%.4f debt=%.0f incidents=%v won=%v %q",
			seed, r.Days, r.Stage, r.Users, r.Budget, r.Avail, r.Debt, r.Incidents, r.Won, r.Outcome)
		if r.Stage != "Seed" {
			far++
		}
		if r.Budget < -1_000_000 {
			t.Fatalf("seed %d: budget %.0f. Nothing legitimate costs this much.", seed, r.Budget)
		}
	}
	if far < 4 {
		t.Fatalf("only %d/5 competent runs closed a funding round", far)
	}
}

// If discipline doesn't beat speed across a spread of seeds, then canaries, breakers,
// margin and postmortems are decoration. This is the game's thesis, as an assertion.
func TestDisciplineBeatsRecklessness(t *testing.T) {
	if testing.Short() {
		t.Skip("plays whole games; minutes under -race")
	}
	s := Start()
	defer s.Stop()
	var careful, reckless float64
	for seed := int64(1); seed <= 6; seed++ {
		a, err := Play(context.Background(), s, seed, &Competent{}, maxDays)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Play(context.Background(), s, seed, &Competent{Cowboy: true}, maxDays)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("seed %d: careful %s %.0f users day %.0f | cowboy %s %.0f users day %.0f", seed, a.Stage, a.Users, a.Days, b.Stage, b.Users, b.Days)
		careful += a.Score
		reckless += b.Score
	}
	if careful <= reckless*1.2 {
		t.Fatalf("careful total score %.0f vs cowboy %.0f: discipline should clearly win", careful, reckless)
	}
}

// Same seed, same commands, same world, across every service boundary. Lots of
// other things in this repo assume this holds, including anyone debugging a report.
func TestSameSeedSameGame(t *testing.T) {
	if testing.Short() {
		t.Skip("plays whole games; minutes under -race")
	}
	play := func() Result {
		s := Start()
		defer s.Stop()
		r, err := Play(context.Background(), s, 42, &Competent{}, 25)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a, b := play(), play()
	if fmt.Sprintf("%+v", a) != fmt.Sprintf("%+v", b) {
		t.Fatalf("replay diverged:\n%+v\n%+v", a, b)
	}
}

func TestGitOpsIsTheOnlyWayIn(t *testing.T) {
	ctx := context.Background()
	s := Start()
	defer s.Stop()
	if _, err := s.Engine.StartGame(ctx, &lgsv1.StartGameRequest{GameId: "g", Seed: 1}); err != nil {
		t.Fatal(err)
	}
	// Push a change, then watch how many ticks reality takes to agree.
	spec, _ := infrastructure.ParseManifest(infrastructure.StarterManifest)
	spec.Regions[0].Nodes = 5
	res, err := s.Sim.Submit(ctx, &lgsv1.Command{GameId: "g", Action: &lgsv1.Command_Commit{Commit: &lgsv1.CommitAction{Yaml: infrastructure.RenderManifest(spec), Message: "more nodes"}}})
	if err != nil || !res.GetOk() {
		t.Fatalf("commit: %v %v", res, err)
	}
	var landed int32
	for i := 0; i < 40 && landed == 0; i++ {
		f, err := s.Engine.Step(ctx, "g")
		if err != nil {
			t.Fatal(err)
		}
		if f.GetTopology().GetRegions()[0].GetNodesReady() == 5 {
			landed = f.GetTick()
		}
	}
	if landed < 8 {
		t.Fatalf("nodes appeared at tick %d: faster than pipeline + flux + boot allows", landed)
	}
}

// Browsers poll, push and watch while the clock runs. Most useful under -race, which
// is how CI runs it, but it also checks a live game keeps advancing through all this.
func TestLiveGameUnderConcurrentPlayers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s := Start()
	defer s.Stop()
	if _, err := s.Engine.StartGame(ctx, &lgsv1.StartGameRequest{GameId: "live", Seed: 5, Speed: 8}); err != nil {
		t.Fatal(err)
	}
	stream, err := s.Sim.Watch(ctx, &lgsv1.WatchRequest{GameId: "live"})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	loop := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				fn()
			}
		}()
	}
	loop(func() { s.Sim.GetFrame(ctx, &lgsv1.WatchRequest{GameId: "live"}) })
	loop(func() { s.Infra.ListCommits(ctx, &lgsv1.GameRef{GameId: "live"}) })
	loop(func() { s.Infra.Plan(ctx, &lgsv1.PlanRequest{GameId: "live", Yaml: infrastructure.StarterManifest}) })
	loop(func() { s.Obs.Metrics(ctx, &lgsv1.MetricsRequest{GameId: "live"}) })
	loop(func() { s.Obs.Logs(ctx, &lgsv1.LogsRequest{GameId: "live"}) })
	loop(func() {
		s.Sim.Submit(ctx, &lgsv1.Command{GameId: "live", Action: &lgsv1.Command_Commit{Commit: &lgsv1.CommitAction{Yaml: infrastructure.StarterManifest, Message: "again"}}})
	})

	var last int32
	for {
		f, err := stream.Recv()
		if err != nil {
			break
		}
		last = f.GetTick()
	}
	cancel()
	wg.Wait()
	if last < 5 {
		t.Fatalf("only reached tick %d in 2s at 8x: concurrent readers are starving the clock", last)
	}
}

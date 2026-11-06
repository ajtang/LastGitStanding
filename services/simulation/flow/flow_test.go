package flow

import (
	"testing"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

func fleet(pods int, version string) *lgsv1.Topology {
	r := &lgsv1.RegionState{Name: "eastus", NodeSize: "standard", NodesReady: 50, Towers: 4}
	for i := 0; i < pods; i++ {
		r.Pods = append(r.Pods, &lgsv1.Pod{Id: "p", Version: version, Phase: "ready", AgeTicks: 1})
	}
	return &lgsv1.Topology{
		Regions:  []*lgsv1.RegionState{r},
		Database: &lgsv1.DatabaseState{Tier: "gp8", StorageGb: 5000, PrimaryRegion: "eastus"},
		Cache:    &lgsv1.CacheSpec{Nodes: 2},
		Retries:  2,
	}
}

func run(st *State, topo *lgsv1.Topology, fx *lgsv1.Effects, users float64, ticks int) *lgsv1.Health {
	var h *lgsv1.Health
	for i := 0; i < ticks; i++ {
		h = Step(st, Input{Seed: 7, Tick: 40, Users: users, Topology: topo, Effects: fx})
	}
	return h
}

func TestHealthyFleetIsBoring(t *testing.T) {
	h := run(NewState(), fleet(10, "v1"), &lgsv1.Effects{}, 20000, 5)
	if h.ErrorRate > 0.001 {
		t.Fatalf("error rate = %.4f on an idle fleet", h.ErrorRate)
	}
	if h.LatencyP95Ms > 400 {
		t.Fatalf("p95 = %.0fms on an idle fleet", h.LatencyP95Ms)
	}
}

// The whole point of State: overload should outlive the spike that caused it
// when retries are on, and a breaker should be what breaks the loop.
func TestRetryStormOutlivesItsTrigger(t *testing.T) {
	spike := &lgsv1.Effects{TrafficMultiplier: 3}
	calm := &lgsv1.Effects{}
	users := 34000.0 // the fleet is comfortable at this load, about 65% utilized

	if h := run(NewState(), fleet(10, "v1"), calm, users, 20); h.ErrorRate > 0.01 {
		t.Fatalf("precondition: fleet should handle baseline, got %.3f errors", h.ErrorRate)
	}

	stormy := NewState()
	topo := fleet(10, "v1")
	topo.Retries = 4
	run(stormy, topo, spike, users, 10)
	if h := run(stormy, topo, calm, users, 15); h.ErrorRate < 0.1 {
		t.Fatalf("expected a metastable failure after the spike ended, error rate %.3f", h.ErrorRate)
	}

	braked := NewState()
	topo = fleet(10, "v1")
	topo.Retries = 4
	topo.CircuitBreaker = true
	run(braked, topo, spike, users, 10)
	if h := run(braked, topo, calm, users, 15); h.ErrorRate > 0.01 {
		t.Fatalf("breaker should have let the fleet recover, error rate %.3f", h.ErrorRate)
	}
}

func TestExpiredCertLooksHealthyFromInside(t *testing.T) {
	h := run(NewState(), fleet(10, "v1"), &lgsv1.Effects{TlsExpired: true}, 30000, 3)
	if h.ErrorRate < 0.9 {
		t.Fatalf("users should be locked out, error rate %.3f", h.ErrorRate)
	}
	if h.App.Utilization > 0.1 {
		t.Fatalf("pods should look bored during a cert outage, utilization %.2f", h.App.Utilization)
	}
}

func TestRedisOutageMovesTheProblemToTheDatabase(t *testing.T) {
	topo := fleet(20, "v1")
	topo.Database.Tier = "gp2"
	st := NewState()
	before := run(st, topo, &lgsv1.Effects{}, 40000, 3)
	during := run(st, topo, &lgsv1.Effects{CacheDown: true}, 40000, 3)
	if during.Database.Utilization < before.Database.Utilization*3 {
		t.Fatalf("db util %.2f -> %.2f: a cache outage should hammer the database",
			before.Database.Utilization, during.Database.Utilization)
	}
	// Cache is back, but it's cold. The DB keeps paying for a while.
	after := run(st, topo, &lgsv1.Effects{}, 40000, 2)
	if after.CacheHitRatio > 0.3 {
		t.Fatalf("hit ratio %.2f right after recovery: caches don't come back warm", after.CacheHitRatio)
	}
}

func TestCanaryLimitsBlastRadius(t *testing.T) {
	flaw := &lgsv1.Effects{Flaws: []*lgsv1.VersionFlaw{{Version: "bad", ErrorRate: 0.6}}}

	full := fleet(10, "bad")
	full.Retries = 0
	canary := fleet(10, "good")
	canary.Retries = 0
	canary.Regions[0].Pods[0].Version = "bad"

	fullErr := run(NewState(), full, flaw, 20000, 3).ErrorRate
	canaryErr := run(NewState(), canary, flaw, 20000, 3).ErrorRate
	if canaryErr >= fullErr/5 {
		t.Fatalf("canary %.3f vs full rollout %.3f", canaryErr, fullErr)
	}
}

func TestLeakKillsTheOldestPodsFirst(t *testing.T) {
	topo := fleet(3, "leaky")
	topo.Regions[0].Pods[0].AgeTicks = 100
	fx := &lgsv1.Effects{Flaws: []*lgsv1.VersionFlaw{{Version: "leaky", LeakMbPerTick: 40}}}
	h := Step(NewState(), Input{Seed: 1, Tick: 1, Users: 1000, Topology: topo, Effects: fx})
	if len(h.OomKilledPods) != 1 {
		t.Fatalf("oom killed %v, want exactly the old pod", h.OomKilledPods)
	}
}

func TestSingleRegionFailureIsAnOutageAndMultiRegionIsNot(t *testing.T) {
	down := &lgsv1.Effects{RegionsDown: []string{"eastus"}}

	if h := run(NewState(), fleet(10, "v1"), down, 20000, 3); h.ErrorRate < 0.99 {
		t.Fatalf("single region should be fully down, error rate %.3f", h.ErrorRate)
	}

	topo := fleet(10, "v1")
	second := fleet(10, "v1").Regions[0]
	second.Name = "westeurope"
	topo.Regions = append(topo.Regions, second)
	topo.Database.PrimaryRegion = "westeurope"
	if h := run(NewState(), topo, down, 20000, 3); h.ErrorRate > 0.05 {
		t.Fatalf("surviving region should absorb traffic, error rate %.3f", h.ErrorRate)
	}
}

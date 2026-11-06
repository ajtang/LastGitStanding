package economy

import (
	"context"
	"testing"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/catalog"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
)

func tinyTopo() *lgsv1.Topology {
	return &lgsv1.Topology{
		Regions:       []*lgsv1.RegionState{{Name: "eastus", NodeSize: "standard", NodesReady: 3, Towers: 1}},
		Database:      &lgsv1.DatabaseState{Tier: "gp2", StorageGb: 100},
		Cache:         &lgsv1.CacheSpec{Nodes: 1},
		Observability: &lgsv1.ObservabilitySpec{Metrics: "basic", Logs: "none", Traces: "none", Alerting: "none"},
	}
}

func settleFor(t *testing.T, s *Server, ticks int, errRate, p95 float64) *lgsv1.Ledger {
	t.Helper()
	var l *lgsv1.Ledger
	for i := 1; i <= ticks; i++ {
		resp, err := s.Settle(context.Background(), &lgsv1.SettleRequest{
			GameId: "g", Tick: int32(i), Topology: tinyTopo(),
			Health: &lgsv1.Health{ErrorRate: errRate, LatencyP95Ms: p95, OfferedRps: 50},
		})
		if err != nil {
			t.Fatal(err)
		}
		l = resp.GetLedger()
	}
	return l
}

func fresh(t *testing.T) *Server {
	t.Helper()
	s := NewServer(checkpoint.NewMemoryStore())
	if _, err := s.Init(context.Background(), &lgsv1.InitGame{GameId: "g"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReliabilityDrivesGrowth(t *testing.T) {
	good := settleFor(t, fresh(t), 300, 0, 120)
	bad := settleFor(t, fresh(t), 200, 0.08, 120)
	if good.GetUsers() <= startUsers {
		t.Fatalf("healthy service lost users: %.0f", good.GetUsers())
	}
	if bad.GetUsers() >= startUsers {
		t.Fatalf("8%% error rate should make people leave, users went to %.0f", bad.GetUsers())
	}
}

func TestSustainedOutageGetsYouFired(t *testing.T) {
	l := settleFor(t, fresh(t), 400, 0.5, 900)
	if l.GetGameOverReason() == "" {
		t.Fatal("three days at 50% errors and everyone still has a job?")
	}
}

func TestDebtTaxesFutureFeatures(t *testing.T) {
	s := fresh(t)
	ctx := context.Background()
	before := settleFor(t, s, 1, 0, 100).GetFeatureCostHours()
	if resp, _ := s.Spend(ctx, &lgsv1.SpendRequest{GameId: "g", Purpose: "feature"}); !resp.GetOk() {
		t.Fatalf("couldn't afford first feature: %s", resp.GetMessage())
	}
	resp, err := s.Settle(ctx, &lgsv1.SettleRequest{GameId: "g", Tick: 2, Topology: tinyTopo(), Health: &lgsv1.Health{}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetLedger().GetFeatureCostHours() <= before {
		t.Fatal("shipping should raise the cost of the next feature")
	}
}

func TestCannotSpendHoursYouDoNotHave(t *testing.T) {
	s := fresh(t)
	ctx := context.Background()
	for range 10 {
		s.Spend(ctx, &lgsv1.SpendRequest{GameId: "g", Purpose: "feature"})
	}
	resp, err := s.Spend(ctx, &lgsv1.SpendRequest{GameId: "g", Purpose: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOk() {
		t.Fatal("infinite engineers exploit")
	}
}

// Guard against a free lunch sneaking into the price list. Bigger database tiers
// should cost more per query, or nobody would ever buy the small ones.
func TestLargerDatabasesCostMorePerQuery(t *testing.T) {
	order := []string{"gp2", "gp4", "gp8", "gp16", "mem32"}
	prev := 0.0
	for _, name := range order {
		tier := catalog.DBTiers[name]
		perQuery := tier.CostPerDay / tier.QPS
		if perQuery < prev {
			t.Fatalf("%s costs %.4f per QPS, less than the tier below at %.4f", name, perQuery, prev)
		}
		prev = perQuery
	}
}

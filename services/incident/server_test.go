package incident

import (
	"context"
	"testing"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
)

func topoWith(version string) *lgsv1.Topology {
	return &lgsv1.Topology{
		Regions:  []*lgsv1.RegionState{{Name: "eastus", Pods: []*lgsv1.Pod{{Id: "a", Version: version, Phase: "ready"}}}},
		Database: &lgsv1.DatabaseState{PrimaryRegion: "eastus"},
		Cache:    &lgsv1.CacheSpec{Nodes: 1},
		Ingress:  &lgsv1.IngressState{CertExpiresAtTick: 100000},
	}
}

func newGame(t *testing.T) *Server {
	t.Helper()
	s := NewServer(checkpoint.NewMemoryStore())
	if _, err := s.Init(context.Background(), &lgsv1.InitGame{GameId: "g", Seed: 3}); err != nil {
		t.Fatal(err)
	}
	return s
}

func advance(t *testing.T, s *Server, tick int32, topo *lgsv1.Topology, h *lgsv1.Health, alerts ...*lgsv1.AlertState) *lgsv1.AdvanceResponse {
	t.Helper()
	resp, err := s.Advance(context.Background(), &lgsv1.AdvanceRequest{
		GameId: "g", Tick: tick, Topology: topo, LastHealth: h,
		LastLedger: &lgsv1.Ledger{Stage: "Seed"}, FiringAlerts: alerts,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestExpiredCertPersistsUntilRenewedInTopology(t *testing.T) {
	s := newGame(t)
	topo := topoWith("v1.0.0")
	topo.Ingress.CertExpiresAtTick = 5

	if advance(t, s, 4, topo, nil).GetEffects().GetTlsExpired() {
		t.Fatal("expired early")
	}
	for tick := int32(5); tick < 50; tick++ {
		if !advance(t, s, tick, topo, nil).GetEffects().GetTlsExpired() {
			t.Fatalf("tick %d: cert should stay expired, nobody touched it", tick)
		}
	}
	topo.Ingress.CertExpiresAtTick = 5000
	resp := advance(t, s, 50, topo, nil)
	if resp.GetEffects().GetTlsExpired() {
		t.Fatal("renewal in actual state should end the incident")
	}
	if got := resp.GetIncidents()[0].GetStatus(); got != "resolved" {
		t.Fatalf("status = %s", got)
	}
}

func TestCausesStayHiddenUntilEarned(t *testing.T) {
	s := newGame(t)
	topo := topoWith("v1.0.0")
	topo.Ingress.CertExpiresAtTick = 1
	alert := &lgsv1.AlertState{Name: "HighErrorRate", Severity: "page"}
	resp := advance(t, s, 1, topo, nil, alert)

	view := resp.GetIncidents()[0]
	if view.GetKind() != "" || view.GetRunbook() != "" {
		t.Fatalf("fresh incident leaked its cause: %q", view.GetKind())
	}

	ctx := context.Background()
	wrong, _ := s.Diagnose(ctx, &lgsv1.DiagnoseRequest{GameId: "g", IncidentId: view.GetId(), Cause: RedisFailure})
	if wrong.GetCorrect() || wrong.GetHoursPenalty() == 0 {
		t.Fatalf("wrong guess should cost hours: %+v", wrong)
	}
	right, _ := s.Diagnose(ctx, &lgsv1.DiagnoseRequest{GameId: "g", IncidentId: view.GetId(), Cause: CertExpiry})
	if !right.GetCorrect() {
		t.Fatal("right guess rejected")
	}
	view = advance(t, s, 2, topo, nil, alert).GetIncidents()[0]
	if view.GetRunbook() == "" {
		t.Fatal("correct diagnosis should unlock the runbook")
	}
}

func TestAlertsDetectFasterThanCustomers(t *testing.T) {
	hurt := &lgsv1.Health{ErrorRate: 0.5}
	measure := func(alert bool) int32 {
		s := newGame(t)
		topo := topoWith("v1.0.0")
		topo.Ingress.CertExpiresAtTick = 1
		for tick := int32(1); tick < 40; tick++ {
			var alerts []*lgsv1.AlertState
			if alert {
				alerts = append(alerts, &lgsv1.AlertState{Name: "x", Severity: "page"})
			}
			if d := advance(t, s, tick, topo, hurt, alerts...).GetIncidents()[0].GetDetectedTick(); d != 0 {
				return d
			}
		}
		t.Fatal("never detected")
		return 0
	}
	if withAlerts, without := measure(true), measure(false); withAlerts >= without {
		t.Fatalf("alerting detected at %d, customers at %d; alerting should win", withAlerts, without)
	}
}

func TestWarningsDoNotWakeAnyoneUp(t *testing.T) {
	s := newGame(t)
	topo := topoWith("v1.0.0")
	topo.Ingress.CertExpiresAtTick = 1
	warn := &lgsv1.AlertState{Name: "CertExpiringSoon", Severity: "warn"}
	if got := advance(t, s, 1, topo, nil, warn).GetIncidents()[0].GetDetectedTick(); got != 0 {
		t.Fatalf("a warn-level alert detected the incident at tick %d", got)
	}
}

func TestReplicatedRedisRecoversAlmostImmediately(t *testing.T) {
	duration := func(replicated bool) int32 {
		g := &game{}
		topo := topoWith("v1")
		topo.Cache.Replicated = replicated
		inc := g.spawn(10, RedisFailure, topo, 0.5)
		return inc.Until - 10
	}
	if fast, slow := duration(true), duration(false); fast*5 > slow {
		t.Fatalf("replicated %d vs unreplicated %d ticks", fast, slow)
	}
}

func TestReplayedTickIsIdempotent(t *testing.T) {
	s := newGame(t)
	topo := topoWith("v1.0.0")
	topo.Ingress.CertExpiresAtTick = 1
	first := advance(t, s, 1, topo, nil)
	second := advance(t, s, 1, topo, nil)
	if len(first.GetIncidents()) != 1 || len(second.GetIncidents()) != 1 {
		t.Fatalf("replay opened another incident: %d then %d", len(first.GetIncidents()), len(second.GetIncidents()))
	}
}

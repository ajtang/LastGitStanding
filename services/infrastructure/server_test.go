package infrastructure

import (
	"context"
	"strings"
	"testing"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
)

type rig struct {
	t    *testing.T
	s    *Server
	tick int32
	last *lgsv1.ReconcileResponse
}

func newRig(t *testing.T) *rig {
	t.Helper()
	s := NewServer(checkpoint.NewMemoryStore())
	if _, err := s.Init(context.Background(), &lgsv1.InitGame{GameId: "g", Seed: 1}); err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, s: s}
}

func (r *rig) advance(n int, errRate float64) *lgsv1.ReconcileResponse {
	r.t.Helper()
	for i := 0; i < n; i++ {
		r.tick++
		resp, err := r.s.Reconcile(context.Background(), &lgsv1.ReconcileRequest{GameId: "g", Tick: r.tick, WorstVersionErrorRate: errRate})
		if err != nil {
			r.t.Fatal(err)
		}
		r.last = resp
	}
	return r.last
}

func (r *rig) commit(edit func(string) string) string {
	r.t.Helper()
	resp, err := r.s.Commit(context.Background(), &lgsv1.CommitRequest{GameId: "g", Tick: r.tick, Yaml: edit(StarterManifest), Message: "test"})
	if err != nil {
		r.t.Fatal(err)
	}
	return resp.GetSha()
}

func replace(old, new string) func(string) string {
	return func(s string) string {
		if !strings.Contains(s, old) {
			panic("manifest no longer contains " + old)
		}
		return strings.Replace(s, old, new, 1)
	}
}

func podsReady(t *lgsv1.Topology, version string) int {
	n := 0
	for _, r := range t.GetRegions() {
		for _, p := range r.GetPods() {
			if p.GetPhase() == "ready" && p.GetVersion() == version {
				n++
			}
		}
	}
	return n
}

func TestNothingChangesUntilFluxHasReconciled(t *testing.T) {
	r := newRig(t)
	r.commit(replace("nodes: 3", "nodes: 5"))

	if got := r.advance(1, 0).GetTopology().GetRegions()[0].GetNodesProvisioning(); got != 0 {
		t.Fatalf("infra moved one tick after commit (%d nodes provisioning): the pipeline is the point", got)
	}
	r.advance(16, 0)
	if got := r.last.GetTopology().GetRegions()[0].GetNodesReady(); got != 5 {
		t.Fatalf("nodes = %d, want 5 after pipeline, flux and boot", got)
	}
}

func TestTypoFailsLintAndNeverReachesMain(t *testing.T) {
	r := newRig(t)
	head := r.advance(1, 0).GetGitops().GetHeadSha()
	r.commit(replace("replicas: 3", "replcas: 3"))
	r.advance(6, 0)

	run := r.last.GetGitops().GetPipelines()[0]
	if run.GetStatus() != "failed" || run.GetStage() != "lint" {
		t.Fatalf("run = %s/%s, want failed at lint", run.GetStage(), run.GetStatus())
	}
	if r.last.GetGitops().GetHeadSha() != head {
		t.Fatal("a commit that failed lint reached main")
	}
}

func TestPolicyRefusesUnpublishedImages(t *testing.T) {
	r := newRig(t)
	r.commit(replace("version: v1.0.0", "version: v9.9.9"))
	r.advance(6, 0)
	run := r.last.GetGitops().GetPipelines()[0]
	if run.GetStage() != "policy" || run.GetStatus() != "failed" {
		t.Fatalf("run = %s/%s, want failed at policy", run.GetStage(), run.GetStatus())
	}
}

func TestReplicasBeyondNodeCapacityGoPendingAndFluxStalls(t *testing.T) {
	r := newRig(t)
	r.commit(replace("replicas: 3", "replicas: 40"))
	r.advance(30, 0)
	if got := podsReady(r.last.GetTopology(), "v1.0.0"); got != 12 {
		t.Fatalf("ready pods = %d, want 12 (3 standard nodes x 4 slots)", got)
	}
	if st := r.last.GetGitops().GetFlux().GetStatus(); st != "stalled" {
		t.Fatalf("flux = %s, want stalled with pods stuck pending", st)
	}
}

func TestCanaryAbortsOnBadMetricsAndLeavesGitDisagreeing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.s.PublishVersion(ctx, &lgsv1.PublishVersionRequest{GameId: "g", Kind: "feature"}); err != nil {
		t.Fatal(err)
	}
	r.commit(func(s string) string {
		s = replace("version: v1.0.0", "version: v1.1.0")(s)
		s = replace("rollout: rolling", "rollout: canary")(s)
		return replace("metrics: basic", "metrics: full")(s)
	})
	// Far enough for the canary to be serving, not far enough to finish holding.
	r.advance(8, 0)
	if got := podsReady(r.last.GetTopology(), "v1.1.0"); got != 1 {
		t.Fatalf("canary pods = %d, want exactly 1 while holding", got)
	}
	r.advance(2, 0.2) // the canary is now on fire
	if got := podsReady(r.last.GetTopology(), "v1.1.0"); got != 0 {
		t.Fatalf("%d pods still on the bad version after abort", got)
	}
	if st := r.last.GetGitops().GetFlux().GetStatus(); st != "stalled" {
		t.Fatalf("flux = %s; spec and cluster disagree, so it should say so", st)
	}
}

func TestRetriedTickDoesNotAdvanceTwice(t *testing.T) {
	r := newRig(t)
	r.advance(1, 0)
	ctx := context.Background()
	timer := func() int32 { return r.last.GetGitops().GetFlux().GetNextSyncIn() }
	before := timer()
	resp, err := r.s.Reconcile(ctx, &lgsv1.ReconcileRequest{GameId: "g", Tick: r.tick})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetGitops().GetFlux().GetNextSyncIn() != before {
		t.Fatal("replaying a tick moved the Flux timer")
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	r := newRig(t)
	r.commit(replace("nodes: 3", "nodes: 4"))
	r.advance(3, 0)
	r.s.Keeper().Forget("g")
	r.advance(20, 0)
	if got := r.last.GetTopology().GetRegions()[0].GetNodesReady(); got != 4 {
		t.Fatalf("nodes = %d after restart mid-pipeline, want 4", got)
	}
}

// The whole argument for paying for a standby: after promotion the database stays
// put, even though git still names the old primary. Failing back into a dead region
// would flap for the length of the outage.
func TestPromotedStandbyDoesNotFailBackIntoADeadRegion(t *testing.T) {
	r := newRig(t)
	r.commit(func(s string) string {
		s = replace("regions:\n", "regions:\n  - name: westeurope\n    nodes: 3\n    nodeSize: standard\n    towers: 1\n")(s)
		return replace(`standbyRegion: ""`, "standbyRegion: westeurope")(s)
	})
	r.advance(40, 0)
	if got := r.last.GetTopology().GetDatabase().GetStandbyRegion(); got != "westeurope" {
		t.Fatalf("standby = %q; test setup didn't land", got)
	}

	down := func(n int, regions ...string) {
		for i := 0; i < n; i++ {
			r.tick++
			resp, err := r.s.Reconcile(context.Background(), &lgsv1.ReconcileRequest{GameId: "g", Tick: r.tick, RegionsDown: regions})
			if err != nil {
				t.Fatal(err)
			}
			r.last = resp
		}
	}
	down(6, "eastus")
	db := r.last.GetTopology().GetDatabase()
	if db.GetPrimaryRegion() != "westeurope" || db.GetFailoverTicks() != 0 {
		t.Fatalf("primary=%s failover=%d after 6 ticks: should be promoted and serving", db.GetPrimaryRegion(), db.GetFailoverTicks())
	}

	// The region comes back. Nothing moves until someone commits a decision.
	down(10)
	if got := r.last.GetTopology().GetDatabase().GetPrimaryRegion(); got != "westeurope" {
		t.Fatalf("primary moved to %s on its own; unplanned failback causes a second outage", got)
	}
}

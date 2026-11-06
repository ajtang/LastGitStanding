package infrastructure

import (
	"errors"
	"fmt"
	"slices"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/catalog"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
)

func errorsIsNoGame(err error) bool { return errors.Is(err, checkpoint.ErrNoGame) }

type eventLog struct {
	tick   int32
	events []*lgsv1.Event
}

func (l *eventLog) add(severity, source, kind, msg, ref string) {
	l.events = append(l.events, &lgsv1.Event{Tick: l.tick, Source: source, Kind: kind, Severity: severity, Message: msg, Ref: ref})
}
func (l *eventLog) info(source, kind, msg, ref string) { l.add("info", source, kind, msg, ref) }
func (l *eventLog) warn(source, kind, msg, ref string) { l.add("warn", source, kind, msg, ref) }
func (l *eventLog) page(source, kind, msg, ref string) { l.add("page", source, kind, msg, ref) }

// converge moves actual toward desired by as much as physics allows in one tick.
// Everything takes time here. That's not a limitation of the model; that's what
// the model is for.
func (g *game) converge(req *lgsv1.ReconcileRequest, ev *eventLog) {
	spec := g.Desired
	tick := req.GetTick()
	a := g.Actual

	// Settings that are a config reload rather than a resource.
	a.Retries = spec.GetApp().GetRetries()
	a.CircuitBreaker = spec.GetApp().GetCircuitBreaker()
	a.Rollout = spec.GetApp().GetRollout()
	a.Observability = spec.GetObservability()
	a.Cache = spec.GetCache()

	g.convergeRegions(spec, tick, ev)
	g.convergeDatabase(spec, req.GetRegionsDown(), ev)
	g.convergeCerts(spec, tick, ev)
	g.pickRolloutTarget(spec, req.GetWorstVersionErrorRate(), ev)

	for _, r := range a.GetRegions() {
		if r.GetProvisioningTicks() > 0 {
			continue
		}
		g.convergePods(r, spec, req.GetOomKilledPods(), ev)
	}
	g.finishRollout(spec, ev)
}

func (g *game) convergeRegions(spec *lgsv1.PlatformSpec, tick int32, ev *eventLog) {
	a := g.Actual
	// Regions being removed drain instantly. Generous, but nobody wants to watch it.
	a.Regions = slices.DeleteFunc(a.Regions, func(r *lgsv1.RegionState) bool {
		gone := !slices.ContainsFunc(spec.GetRegions(), func(s *lgsv1.RegionSpec) bool { return s.GetName() == r.GetName() })
		if gone {
			ev.info("infrastructure", "region_removed", "Decommissioned "+r.GetName(), r.GetName())
			delete(g.NodeETA, r.GetName())
		}
		return gone
	})

	for _, s := range spec.GetRegions() {
		idx := slices.IndexFunc(a.Regions, func(r *lgsv1.RegionState) bool { return r.GetName() == s.GetName() })
		if idx < 0 {
			a.Regions = append(a.Regions, &lgsv1.RegionState{
				Name: s.GetName(), NodeSize: s.GetNodeSize(), NodesProvisioning: s.GetNodes(),
				Towers: s.GetTowers(), ProvisioningTicks: regionBuildTicks,
			})
			ev.info("infrastructure", "region_provisioning", "Provisioning region "+s.GetName()+". This takes a while, as it should.", s.GetName())
			continue
		}
		r := a.Regions[idx]
		r.Towers = s.GetTowers()

		if r.ProvisioningTicks > 0 {
			r.ProvisioningTicks--
			if r.ProvisioningTicks == 0 {
				r.NodesReady, r.NodesProvisioning = r.NodesProvisioning, 0
				ev.info("infrastructure", "region_ready", "Region "+s.GetName()+" is online", s.GetName())
			}
			continue
		}

		// Node pool: changing SKU replaces the pool, which we model as a size swap
		// plus a boot wait on every node. Yes, that hurts. Plan it for off-peak.
		if r.GetNodeSize() != s.GetNodeSize() {
			r.NodeSize = s.GetNodeSize()
			r.NodesProvisioning = r.NodesReady
			r.NodesReady = 0
			g.NodeETA[r.GetName()] = nodeBootTicks
			ev.warn("infrastructure", "node_pool_replace", "Replacing node pool in "+r.GetName(), r.GetName())
		}

		total := r.GetNodesReady() + r.GetNodesProvisioning()
		if total < s.GetNodes() {
			r.NodesProvisioning += s.GetNodes() - total
			g.NodeETA[r.GetName()] = nodeBootTicks
		} else if total > s.GetNodes() {
			r.NodesReady = max(s.GetNodes()-r.GetNodesProvisioning(), 0)
		}

		if r.GetNodesProvisioning() > 0 {
			g.NodeETA[r.GetName()]--
			if g.NodeETA[r.GetName()] <= 0 {
				ev.info("infrastructure", "nodes_ready", fmt.Sprintf("%d nodes joined %s", r.GetNodesProvisioning(), r.GetName()), r.GetName())
				r.NodesReady += r.GetNodesProvisioning()
				r.NodesProvisioning = 0
			}
		}
	}
}

func (g *game) convergeDatabase(spec *lgsv1.PlatformSpec, regionsDown []string, ev *eventLog) {
	db, want := g.Actual.GetDatabase(), spec.GetDatabase()
	if db.GetFailoverTicks() > 0 {
		db.FailoverTicks--
	}

	// Unplanned: primary region is gone. A standby gets promoted on its own, and
	// that's the entire argument for paying for one.
	if slices.Contains(regionsDown, db.GetPrimaryRegion()) {
		if s := db.GetStandbyRegion(); s != "" && !slices.Contains(regionsDown, s) {
			ev.page("infrastructure", "db_failover", "Primary lost. Promoting standby in "+s, s)
			db.PrimaryRegion, db.StandbyRegion = s, db.GetPrimaryRegion()
			db.FailoverTicks = dbFailoverTicks
		}
		return // nothing else is changing on a database that doesn't exist right now
	}

	if db.GetTier() != want.GetTier() {
		ev.warn("infrastructure", "db_resize", fmt.Sprintf("Database failing over to %s. Connections will drop.", want.GetTier()), want.GetTier())
		db.Tier = want.GetTier()
		db.FailoverTicks = dbFailoverTicks
	}
	// React to git changing, not to git differing. After an unplanned promotion git
	// still names the old primary, and "correcting" that would fail back into a region
	// that may still be down, for as long as it's down. Failback is a human decision.
	if want.GetPrimaryRegion() != g.PrimarySpec || want.GetStandbyRegion() != g.StandbySpec {
		g.PrimarySpec, g.StandbySpec = want.GetPrimaryRegion(), want.GetStandbyRegion()
		if db.GetPrimaryRegion() != want.GetPrimaryRegion() {
			ev.warn("infrastructure", "db_promote", "Planned failover to "+want.GetPrimaryRegion(), want.GetPrimaryRegion())
			db.PrimaryRegion = want.GetPrimaryRegion()
			db.FailoverTicks = dbFailoverTicks
		}
		db.StandbyRegion = want.GetStandbyRegion()
	}
	db.StorageGb = want.GetStorageGb()

	if db.GetReadReplicas() > want.GetReadReplicas() {
		db.ReadReplicas = want.GetReadReplicas()
		g.ReplicaETA = 0
	} else if db.GetReadReplicas() < want.GetReadReplicas() {
		if g.ReplicaETA == 0 {
			g.ReplicaETA = replicaSyncTicks
		}
		g.ReplicaETA--
		if g.ReplicaETA == 0 {
			db.ReadReplicas++
			ev.info("infrastructure", "replica_ready", "Read replica caught up", "")
		}
	}

	for _, idx := range want.GetIndexes() {
		if slices.Contains(db.GetIndexes(), idx) {
			continue
		}
		if _, building := g.IndexETA[idx]; !building {
			g.IndexETA[idx] = indexBuildTicks
		}
		g.IndexETA[idx]--
		if g.IndexETA[idx] <= 0 {
			delete(g.IndexETA, idx)
			db.Indexes = append(db.Indexes, idx)
			ev.info("infrastructure", "index_ready", "CREATE INDEX CONCURRENTLY "+idx+" finished", idx)
		}
	}
}

func (g *game) convergeCerts(spec *lgsv1.PlatformSpec, tick int32, ev *eventLog) {
	in := g.Actual.GetIngress()
	in.CertManager = spec.GetIngress().GetCertManager()
	if spec.GetIngress().GetCertRenewal() != in.GetCertRenewal() {
		in.CertRenewal = spec.GetIngress().GetCertRenewal()
		in.CertExpiresAtTick = tick + certLifetimeTicks
		ev.info("infrastructure", "cert_renewed", "Certificate renewed by hand. See you in two weeks.", "")
	}
	if in.GetCertManager() && in.GetCertExpiresAtTick()-tick < certManagerLead {
		in.CertExpiresAtTick = tick + certLifetimeTicks
		ev.info("infrastructure", "cert_renewed", "cert-manager renewed the certificate. Nobody noticed, which is the idea.", "")
	}
}

// pickRolloutTarget decides which version pods should be converging to, which
// isn't always the one in the spec: a failed canary leaves git saying one thing
// and the cluster doing another. Flux reports that as stalled until someone
// commits a decision.
func (g *game) pickRolloutTarget(spec *lgsv1.PlatformSpec, errRate float64, ev *eventLog) {
	want := spec.GetApp().GetVersion()
	if want == g.Aborted {
		g.Target = g.Stable
		return
	}
	g.Aborted = ""
	if want != g.Target {
		g.Target = want
		g.CanaryPassed = false
		g.CanaryLeft = 0
		if want != g.Stable {
			if spec.GetApp().GetRollout() == "canary" {
				g.CanaryLeft = canaryHoldTicks
				ev.info("infrastructure", "canary_started", "Canary started for "+want, want)
			} else {
				ev.info("infrastructure", "rollout_started", "Rolling out "+want, want)
			}
		}
	}
	if g.CanaryLeft > 0 {
		g.CanaryLeft--
		// Without full metrics there's nothing to analyse, so the canary "passes"
		// the way a test suite with no assertions passes. It compares per-version
		// errors rather than what users saw, because retries hide one bad pod from
		// users almost completely. That's also why it can't see a slow leak.
		analysed := spec.GetObservability().GetMetrics() == "full"
		if analysed && errRate > canaryAbortErrRate {
			g.CanaryLeft = 0
			g.Aborted = want
			g.Target = g.Stable
			ev.page("infrastructure", "canary_aborted", fmt.Sprintf("Canary for %s aborted at %.1f%% errors; rolling back pods", want, errRate*100), want)
			return
		}
		if g.CanaryLeft == 0 {
			g.CanaryPassed = true
			ev.info("infrastructure", "canary_promoted", "Canary for "+want+" promoted", want)
		}
	}
}

func (g *game) convergePods(r *lgsv1.RegionState, spec *lgsv1.PlatformSpec, oom []string, ev *eventLog) {
	size := catalog.NodeSizes[r.GetNodeSize()]
	slots := r.GetNodesReady() * size.PodSlots
	want := spec.GetApp().GetReplicas()
	active := make([]*lgsv1.Pod, 0, len(r.GetPods()))
	for _, p := range r.GetPods() {
		switch p.GetPhase() {
		case "terminating":
			continue // gone this tick
		case "ready":
			if slices.Contains(oom, p.GetId()) {
				p.Phase, p.PhaseTicks, p.AgeTicks = "crashloop", crashloopTicks, 0
				ev.warn("infrastructure", "pod_oomkilled", p.GetId()+" OOMKilled", p.GetId())
			} else {
				p.AgeTicks++
			}
		case "crashloop", "starting":
			p.PhaseTicks--
			if p.PhaseTicks <= 0 {
				if p.GetPhase() == "crashloop" {
					p.Phase, p.PhaseTicks = "starting", podStartTicks
				} else {
					p.Phase = "ready"
				}
			}
		}
		active = append(active, p)
	}

	// Scale down: youngest and least useful first.
	for int32(len(active)) > want {
		active = removeLeastUseful(active)
	}
	for int32(len(active)) < want {
		active = append(active, g.newPod(r.GetName(), g.Target, "pending"))
	}

	// Rollout: replace a batch of old pods per tick, only as new ones come ready.
	canaryHold := g.CanaryLeft > 0 && !g.CanaryPassed
	replaced := 0
	batch := max(1, int(want)/4)
	onTarget := 0
	for _, p := range active {
		if p.GetVersion() == g.Target {
			onTarget++
		}
	}
	for _, p := range active {
		if p.GetVersion() == g.Target || replaced >= batch {
			continue
		}
		if canaryHold && onTarget >= 1 {
			break // one canary per region. That's the blast radius you paid for.
		}
		p.Version, p.Phase, p.PhaseTicks, p.AgeTicks = g.Target, "starting", podStartTicks, 0
		replaced++
		onTarget++
	}

	// The scheduler: pods need a slot. Overcommit isn't a thing here; a pending
	// pod with no room is exactly what `kubectl describe pod` would have told you.
	used := int32(0)
	for _, p := range active {
		if p.GetPhase() != "pending" {
			used++
		}
	}
	// A node pool scale-down evicts whoever doesn't fit.
	for _, p := range slices.Backward(active) {
		if used <= slots {
			break
		}
		if p.GetPhase() != "pending" {
			p.Phase = "pending"
			used--
		}
	}
	for _, p := range active {
		if p.GetPhase() == "pending" && used < slots {
			p.Phase, p.PhaseTicks = "starting", podStartTicks
			used++
		}
	}
	r.Pods = active
}

func removeLeastUseful(pods []*lgsv1.Pod) []*lgsv1.Pod {
	rank := map[string]int{"pending": 0, "crashloop": 1, "starting": 2, "ready": 3}
	worst := 0
	for i, p := range pods {
		if rank[p.GetPhase()] < rank[pods[worst].GetPhase()] {
			worst = i
		}
	}
	return slices.Delete(pods, worst, worst+1)
}

func (g *game) finishRollout(spec *lgsv1.PlatformSpec, ev *eventLog) {
	if g.Target == g.Stable {
		g.updateStall()
		return
	}
	for _, r := range g.Actual.GetRegions() {
		for _, p := range r.GetPods() {
			if p.GetVersion() != g.Target || p.GetPhase() != "ready" {
				g.updateStall()
				return
			}
		}
	}
	g.Stable = g.Target
	ev.info("infrastructure", "rollout_finished", g.Target+" is live everywhere", g.Target)
	g.updateStall()
}

func (g *game) updateStall() {
	stuck := false
	for _, r := range g.Actual.GetRegions() {
		for _, p := range r.GetPods() {
			if p.GetPhase() == "pending" && r.GetNodesProvisioning() == 0 {
				stuck = true
			}
		}
	}
	if stuck {
		g.StallTicks++
	} else {
		g.StallTicks = 0
	}
}

func (g *game) fluxState() string {
	switch {
	case g.Aborted != "":
		return "stalled"
	case g.StallTicks >= stallAfterTicks:
		return "stalled"
	case !g.converged() || g.HeadSHA != g.AppliedSHA:
		return "reconciling"
	}
	return "synced"
}

func (g *game) fluxDetail() string {
	switch g.fluxState() {
	case "stalled":
		if g.Aborted != "" {
			return "canary for " + g.Aborted + " failed analysis; git still asks for it. Commit a decision."
		}
		return "pods pending: 0/N nodes available, insufficient cpu. Add nodes or fewer replicas."
	case "reconciling":
		if g.HeadSHA != g.AppliedSHA {
			return fmt.Sprintf("%s on main, next sync in %d", short(g.HeadSHA), g.FluxTimer)
		}
		return "applying " + short(g.AppliedSHA)
	}
	return "applied " + short(g.AppliedSHA)
}

func (g *game) converged() bool {
	for _, r := range g.Actual.GetRegions() {
		if r.GetProvisioningTicks() > 0 || r.GetNodesProvisioning() > 0 {
			return false
		}
		for _, p := range r.GetPods() {
			if p.GetPhase() == "pending" || p.GetPhase() == "starting" || p.GetVersion() != g.Target {
				return false
			}
		}
	}
	db, want := g.Actual.GetDatabase(), g.Desired.GetDatabase()
	return db.GetFailoverTicks() == 0 && db.GetReadReplicas() == want.GetReadReplicas() && len(g.IndexETA) == 0
}

func (g *game) newPod(region, version, phase string) *lgsv1.Pod {
	g.PodSeq++
	return &lgsv1.Pod{Id: fmt.Sprintf("app-%s-%04x", region, g.PodSeq), Version: version, Phase: phase}
}

// Package flow answers one question per tick: given this topology and this much
// traffic, what do users experience?
//
// It is a capacity model, not a queueing-theory thesis. The curves are chosen to
// fail the way production fails: fine, fine, fine, then all at once, and then
// staying broken after the original cause is gone. No I/O and no clock in here,
// which is why the interesting tests in this repo are boring to write.
package flow

import (
	"math"
	"slices"

	"github.com/ajtang/LastGitStanding/internal/catalog"
	"github.com/ajtang/LastGitStanding/internal/clock"
	"github.com/ajtang/LastGitStanding/internal/dice"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

const (
	rpsPerUser     = 0.01
	readsPerReq    = 2.0
	writesPerReq   = 0.25
	maxQueueTicks  = 0.5  // past this, the app starts shedding instead of queueing
	queueMsPerTick = 2000 // how much a full tick's backlog hurts the request at the back
	appServiceMs   = 28.0
	dbServiceMs    = 9.0
	crossRegionMs  = 38.0
	distanceMs     = 70.0
	storageGBPerW  = 0.0016 // GB per write/s per tick
	warmupPerTick  = 0.04   // a cold cache takes ~25 ticks, during which the DB pays for everything
	maxHitRatio    = 0.92
	tlsFailure     = 0.95 // the 5% are clients that pinned nothing and checked nothing
	dbHoldFactor   = 0.5  // slow queries hold app threads; this is how a DB problem becomes an app problem
	partitionStall = 0.6
)

// State is what carries from one tick to the next. Every field here exists
// because some failure mode outlives its cause.
type State struct {
	Backlog        map[string]float64 `json:"backlog"`
	BackendErrRate float64            `json:"backendErrRate"`
	DBOverflow     float64            `json:"dbOverflow"`
	CacheWarmth    float64            `json:"cacheWarmth"`
	StorageUsedGB  float64            `json:"storageUsedGb"`
}

func NewState() *State {
	return &State{Backlog: map[string]float64{}, CacheWarmth: 1, StorageUsedGB: 38}
}

type Input struct {
	Seed     int64
	Tick     int32
	Users    float64
	Topology *lgsv1.Topology
	Effects  *lgsv1.Effects
}

// Diurnal peaks mid-afternoon and bottoms out when the on-call is asleep, which
// is unfortunately not when the incidents are.
func Diurnal(tick int32) float64 {
	day := float64(tick%clock.TicksPerDay) / clock.TicksPerDay
	return 1 + 0.45*math.Sin(2*math.Pi*(day-0.3))
}

func Step(st *State, in Input) *lgsv1.Health {
	topo, fx := in.Topology, in.Effects
	diurnal := Diurnal(in.Tick)
	noise := 1 + 0.05*clamp(dice.For(in.Seed, in.Tick, "traffic").NormFloat64(), -2, 2)
	demand := in.Users * rpsPerUser * diurnal * noise
	if m := fx.GetTrafficMultiplier(); m > 0 {
		demand *= m
	}

	// An expired cert fails in the handshake: the app sees *less* traffic while
	// users see an outage. Dashboards that only watch pods look lovely.
	reachable := demand
	if fx.GetTlsExpired() {
		reachable *= 1 - tlsFailure
	}

	var up []*lgsv1.RegionState
	for _, r := range topo.GetRegions() {
		if r.GetProvisioningTicks() == 0 && !slices.Contains(fx.GetRegionsDown(), r.GetName()) {
			up = append(up, r)
		}
	}

	// Retries are the feature that turns a bad minute into a bad hour. Each failed
	// attempt comes back as new load, so an overloaded fleet can sustain its own
	// overload after the trigger is gone. Only a breaker lets it recover by itself.
	amp := 1 + float64(topo.GetRetries())*math.Min(st.BackendErrRate, 0.6)
	if topo.GetCircuitBreaker() && st.BackendErrRate > 0.25 {
		amp = 1
	}

	stall := 1 - dbHoldFactor*st.DBOverflow
	if !topo.GetCircuitBreaker() {
		stall *= 1 - partitionStall*fx.GetPartitionLoss()
	}

	h := &lgsv1.Health{
		OfferedRps:         demand,
		Diurnal:            diurnal,
		RetryAmplification: amp,
	}

	var admittedTotal, capacityTotal, shedTotal, offeredTotal, queueWeighted, towerCapTotal float64
	var totalReady float64
	for _, r := range topo.GetRegions() {
		rh := regionPods(r)
		rh.Name = r.GetName()
		rh.Up = slices.Contains(up, r)
		h.Regions = append(h.Regions, rh)
		if !rh.Up {
			delete(st.Backlog, r.GetName())
			rh.ErrorRate = 1
			continue
		}

		offered := reachable * amp / float64(len(up))
		towerCap := float64(r.GetTowers()) * catalog.TowerRPS
		admitted := math.Min(offered, towerCap)
		capacity := float64(rh.PodsReady) * catalog.PodRPS * stall

		incoming := admitted + st.Backlog[r.GetName()]
		served := math.Min(incoming, capacity)
		backlog := incoming - served
		shed := math.Max(0, backlog-capacity*maxQueueTicks)
		backlog -= shed
		st.Backlog[r.GetName()] = backlog

		dropped := offered - admitted + shed
		rh.OfferedRps = offered
		rh.CapacityRps = capacity
		rh.Backlog = backlog
		rh.TowerUtilization = ratio(offered, towerCap)
		rh.Utilization = ratio(incoming, capacity)
		rh.ErrorRate = math.Min(1, ratio(dropped, offered))

		admittedTotal += served
		capacityTotal += capacity
		shedTotal += dropped
		offeredTotal += offered
		towerCapTotal += towerCap
		if capacity > 0 {
			queueWeighted += served * math.Min(backlog/capacity, 2) * queueMsPerTick
		}
		for _, p := range r.GetPods() {
			if p.GetPhase() != "ready" {
				continue
			}
			totalReady++
		}
	}

	// ---- cache

	cacheUp := topo.GetCache().GetNodes() > 0 && !fx.GetCacheDown()
	if cacheUp {
		st.CacheWarmth = math.Min(1, st.CacheWarmth+warmupPerTick)
	} else {
		st.CacheWarmth = 0
	}
	hit := 0.0
	if cacheUp && in.Users > 0 {
		coverage := float64(topo.GetCache().GetNodes()) * catalog.CacheUsers / in.Users
		hit = st.CacheWarmth * maxHitRatio * math.Min(1, math.Pow(coverage, 0.4))
	}
	h.CacheHitRatio = hit

	// ---- database. The database is innocent until proven guilty, and then it's
	// usually guilty of something we did to it.

	db := topo.GetDatabase()
	tier := catalog.DBTiers[db.GetTier()]
	costly := 1.0
	if m := fx.GetQueryCostMultiplier(); m > 0 {
		costly = m
	}
	reads := admittedTotal * readsPerReq * (1 - hit) * costly
	writes := admittedTotal * writesPerReq
	primaryLoad := writes + reads/float64(1+db.GetReadReplicas())

	dbDown := db.GetFailoverTicks() > 0 || slices.Contains(fx.GetRegionsDown(), db.GetPrimaryRegion())
	dbUtil := ratio(primaryLoad, tier.QPS)
	overflow := 0.0
	if dbUtil > 1 {
		overflow = (dbUtil - 1) / dbUtil
	}
	if dbDown {
		overflow = 1
	}
	st.DBOverflow = overflow

	// A request needs the DB if any read missed or it writes. Reads are correlated
	// per user, so this is deliberately not 1-(hit^2).
	needsDB := math.Min(1, (1-hit)*1.4+writesPerReq*0.8)
	dbFail := needsDB * overflow

	st.StorageUsedGB += writes * storageGBPerW
	storageUtil := ratio(st.StorageUsedGB, float64(db.GetStorageGb()))
	if storageUtil >= 1 {
		// Disk full: Postgres goes read-only and every write fails. Reads are fine,
		// which is why this one tends to get misdiagnosed for the first ten minutes.
		st.StorageUsedGB = float64(db.GetStorageGb())
		dbFail = math.Max(dbFail, 0.3)
	}

	// ---- errors. Retries genuinely help with transient failures, which is why
	// people turn them on and why the amplification above comes as a surprise.

	attempts := float64(topo.GetRetries() + 1)
	partition := math.Pow(fx.GetPartitionLoss()*needsDB, attempts)
	flaw := 0.0
	for _, f := range fx.GetFlaws() {
		if share := ratio(versionReady(topo, f.GetVersion()), totalReady); share > 0 {
			flaw += f.GetErrorRate() * share
			h.WorstVersionErrorRate = math.Max(h.WorstVersionErrorRate, f.GetErrorRate())
		}
	}
	flawSeen := math.Pow(math.Min(flaw, 1), attempts)
	shedRate := ratio(shedTotal, offeredTotal)
	if len(up) == 0 {
		shedRate = 1
	}

	backendOK := (1 - shedRate) * (1 - dbFail) * (1 - partition) * (1 - flawSeen)
	st.BackendErrRate = 1 - backendOK

	userOK := backendOK
	if fx.GetTlsExpired() {
		userOK *= 1 - tlsFailure
	}
	h.ErrorRate = clamp(1-userOK, 0, 1)
	h.ServedRps = demand * (1 - h.ErrorRate)

	// ---- latency

	appRho := math.Min(ratio(admittedTotal, capacityTotal), 0.97)
	appMs := appServiceMs / (1 - appRho)
	queueMs := ratio(queueWeighted, admittedTotal)

	dbRho := math.Min(dbUtil, 0.97)
	dbMs := dbServiceMs / (1 - dbRho) * costly
	if overflow > 0 {
		dbMs += 400 * overflow
	}

	// A primary on another continent is a fixed tax you can't scale your way out
	// of, and read replicas only refund the read half of it.
	remote := 0.0
	if len(up) > 0 {
		var far float64
		for _, r := range up {
			if r.GetName() != db.GetPrimaryRegion() {
				far++
			}
		}
		share := far / float64(len(up))
		if db.GetReadReplicas() > 0 {
			share *= 0.4
		}
		remote = crossRegionMs * share
	}
	userDistance := distanceMs * (1 - 1/math.Max(1, float64(len(up))))
	if len(topo.GetRegions()) == 1 {
		userDistance = distanceMs * 0.8 // the whole planet, served from Virginia
	}

	ingressMs := 4.0
	towerRho := ratio(offeredTotal, towerCapTotal)
	if towerRho > 0.8 {
		ingressMs += 60 * (towerRho - 0.8)
	}

	cacheMs := 1.2
	mean := ingressMs + appMs + queueMs + cacheMs + needsDB*(dbMs+remote) + userDistance*0.3
	mean += fx.GetPartitionLoss() * 800 // waiting out connect timeouts

	h.LatencyMeanMs = mean
	h.LatencyP50Ms = mean * 0.8
	tail := math.Max(appRho, dbRho)
	h.LatencyP95Ms = mean * (1.8 + 1.5*tail*tail)
	h.LatencyP99Ms = mean * (2.6 + 5*tail*tail)

	h.Ingress = &lgsv1.ComponentHealth{Utilization: towerRho, LatencyMs: ingressMs, ErrorRate: boolRate(fx.GetTlsExpired(), tlsFailure)}
	h.App = &lgsv1.ComponentHealth{Utilization: ratio(offeredTotal, capacityTotal), LatencyMs: appMs + queueMs, ErrorRate: math.Max(shedRate, flawSeen)}
	h.Cache = &lgsv1.ComponentHealth{Utilization: ratio(in.Users, float64(topo.GetCache().GetNodes())*catalog.CacheUsers), LatencyMs: cacheMs, ErrorRate: boolRate(!cacheUp, 1)}
	h.Database = &lgsv1.ComponentHealth{Utilization: dbUtil, LatencyMs: dbMs + remote, ErrorRate: dbFail}
	h.StorageUsedGb = st.StorageUsedGB
	h.StorageUtilization = storageUtil

	// ---- memory: the leak is a function of pod age, so a restart buys you time
	// and a graph that looks like a saw. Someone will call that "fine".

	for _, r := range up {
		for _, p := range r.GetPods() {
			if p.GetPhase() != "ready" {
				continue
			}
			mb := catalog.PodBaseMB + 500*appRho + flawFor(fx, p.GetVersion()).GetLeakMbPerTick()*float64(p.GetAgeTicks())
			h.MemoryUtilization = math.Max(h.MemoryUtilization, mb/catalog.PodMemoryMB)
			if mb >= catalog.PodMemoryMB {
				h.OomKilledPods = append(h.OomKilledPods, p.GetId())
			}
		}
	}
	return h
}

func regionPods(r *lgsv1.RegionState) *lgsv1.RegionHealth {
	rh := &lgsv1.RegionHealth{}
	for _, p := range r.GetPods() {
		switch p.GetPhase() {
		case "ready":
			rh.PodsReady++
		case "pending":
			rh.PodsPending++
		}
	}
	return rh
}

func versionReady(topo *lgsv1.Topology, version string) float64 {
	var n float64
	for _, r := range topo.GetRegions() {
		for _, p := range r.GetPods() {
			if p.GetPhase() == "ready" && p.GetVersion() == version {
				n++
			}
		}
	}
	return n
}

func flawFor(fx *lgsv1.Effects, version string) *lgsv1.VersionFlaw {
	for _, f := range fx.GetFlaws() {
		if f.GetVersion() == version {
			return f
		}
	}
	return nil
}

func ratio(a, b float64) float64 {
	if b <= 0 {
		if a > 0 {
			return 10 // "very bad", without NaNs wandering into JSON
		}
		return 0
	}
	return a / b
}

func boolRate(b bool, v float64) float64 {
	if b {
		return v
	}
	return 0
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// Package incident decides what goes wrong and whether it has stopped.
//
// Incidents never get fixed by calling this service. It only observes topology,
// and an incident resolves when reality no longer has the problem. So the only
// way to close one is to make a real change through the real pipeline, and that
// is the most important sentence in the game's design.
package incident

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/catalog"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
	"github.com/ajtang/LastGitStanding/internal/clock"
	"github.com/ajtang/LastGitStanding/internal/dice"
)

const (
	gracePeriod        = 2 * clock.TicksPerDay
	baseChancePerTick  = 0.0032 // roughly one surprise every three game days
	maxConcurrent      = 2
	customerDelay      = 10 // how long users suffer before someone tweets at you
	wrongGuessPenalty  = 2.0
	flawBaseChance     = 0.14
	flawHotfixDiscount = 0.5
	badDeployErrRate   = 0.55
	leakMBPerTick      = 70.0
	sustainedTicks     = 6
	cveWindow          = 2 * clock.TicksPerDay
	cveFine            = 15000.0
	postmortemDampen   = 0.7
	learnedFloor       = 0.3
)

type incident struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Opened     int32   `json:"opened"`
	Detected   int32   `json:"detected"`
	DetectedBy string  `json:"detectedBy"`
	Resolved   int32   `json:"resolved"`
	Diagnosed  bool    `json:"diagnosed"`
	Wrong      int32   `json:"wrong"`
	Postmortem bool    `json:"postmortem"`
	Until      int32   `json:"until"`
	Version    string  `json:"version,omitempty"`
	Region     string  `json:"region,omitempty"`
	Index      string  `json:"index,omitempty"`
	SQL        string  `json:"sql,omitempty"`
	Primary    string  `json:"primary,omitempty"`
	Magnitude  float64 `json:"magnitude,omitempty"`
	Breached   bool    `json:"breached,omitempty"`
	CVEVersion int     `json:"cveVersion,omitempty"`
}

type game struct {
	Seed      int64                  `json:"seed"`
	Seq       int                    `json:"seq"`
	Incidents []*incident            `json:"incidents"`
	Flaws     map[string]string      `json:"flaws"` // version -> kind
	Seen      []string               `json:"seen"`
	Learned   map[string]float64     `json:"learned"`
	Overload  int32                  `json:"overload"`
	LastTick  int32                  `json:"lastTick"`
	LastResp  *lgsv1.AdvanceResponse `json:"lastResp"`
}

type Server struct {
	lgsv1.UnimplementedIncidentsServer
	games *checkpoint.Keeper[game]
}

func NewServer(store checkpoint.Store) *Server {
	return &Server{games: checkpoint.NewKeeper[game](store, "incident")}
}

func (s *Server) Init(ctx context.Context, req *lgsv1.InitGame) (*lgsv1.InitIncidentsResponse, error) {
	g := &game{Seed: req.GetSeed(), Flaws: map[string]string{}, Learned: map[string]float64{}, Seen: []string{"v1.0.0"}}
	if err := s.games.Create(ctx, req.GetGameId(), g); err != nil {
		return nil, status.Errorf(codes.Unavailable, "checkpoint: %v", err)
	}
	return &lgsv1.InitIncidentsResponse{}, nil
}

func (s *Server) Advance(ctx context.Context, req *lgsv1.AdvanceRequest) (*lgsv1.AdvanceResponse, error) {
	var resp *lgsv1.AdvanceResponse
	err := s.games.Update(ctx, req.GetGameId(), func(g *game) error {
		if req.GetTick() == g.LastTick && g.LastResp != nil {
			resp = g.LastResp
			return nil
		}
		resp = g.advance(req)
		g.LastTick, g.LastResp = req.GetTick(), resp
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return resp, nil
}

func (g *game) advance(req *lgsv1.AdvanceRequest) *lgsv1.AdvanceResponse {
	tick := req.GetTick()
	topo := req.GetTopology()
	debt := req.GetLastLedger().GetTechDebt()
	var events []*lgsv1.Event
	emit := func(sev, kind, msg, ref string) {
		events = append(events, &lgsv1.Event{Tick: tick, Source: "incident", Kind: kind, Severity: sev, Message: msg, Ref: ref})
	}

	g.rollDeployFlaws(tick, topo, debt)
	opened := g.openFromReality(tick, topo, req.GetLastHealth())
	if inc := g.rollSurprise(tick, topo, req.GetLastLedger()); inc != nil {
		opened = append(opened, inc)
	}
	for _, inc := range opened {
		emit("warn", "incident_opened", definitions[inc.Kind].title, inc.ID)
	}

	fx := &lgsv1.Effects{TrafficMultiplier: 1, QueryCostMultiplier: 1}
	for _, inc := range g.Incidents {
		if inc.Resolved != 0 {
			continue
		}
		if inc.stillHappening(tick, topo, req.GetLastHealth()) {
			inc.apply(fx, tick, emit)
			continue
		}
		// One that ends before anyone notices stays undetected, and so stays off the
		// player's screen for good. Lucky, and unearned.
		inc.Resolved = tick
		emit("info", "incident_resolved", fmt.Sprintf("%s resolved after %d ticks", definitions[inc.Kind].title, tick-inc.Opened), inc.ID)
	}

	g.detect(tick, req, emit)

	resp := &lgsv1.AdvanceResponse{Effects: fx, Events: events}
	for _, inc := range g.Incidents {
		if inc.Resolved == 0 || tick-inc.Resolved < 3*clock.TicksPerDay || !inc.Postmortem {
			resp.Incidents = append(resp.Incidents, inc.view())
		}
	}
	return resp
}

// rollDeployFlaws decides at first sight whether a version is cursed. Debt raises
// the odds. Hotfixes are smaller and go wrong less, but not never.
func (g *game) rollDeployFlaws(tick int32, topo *lgsv1.Topology, debt float64) {
	for _, r := range topo.GetRegions() {
		for _, p := range r.GetPods() {
			v := p.GetVersion()
			if slices.Contains(g.Seen, v) {
				continue
			}
			g.Seen = append(g.Seen, v)
			rng := dice.For(g.Seed, int32(len(g.Seen)), "flaw:"+v)
			chance := flawBaseChance + debt*0.004
			if isHotfix(v) {
				chance *= flawHotfixDiscount
			}
			if rng.Float64() < chance {
				if rng.Float64() < 0.55 {
					g.Flaws[v] = BadDeployment
				} else {
					g.Flaws[v] = MemoryLeak
				}
			}
		}
	}
	for _, v := range g.Seen { // not g.Flaws: incident IDs must not depend on map order
		kind, cursed := g.Flaws[v]
		if cursed && versionRunning(topo, v) && !g.openFor(kind, v) {
			g.open(tick, kind, func(i *incident) { i.Version = v })
		}
	}
}

// openFromReality covers the incidents nobody rolled: the ones you built yourself.
func (g *game) openFromReality(tick int32, topo *lgsv1.Topology, h *lgsv1.Health) []*incident {
	var opened []*incident
	if exp := topo.GetIngress().GetCertExpiresAtTick(); exp > 0 && tick >= exp && !g.active(CertExpiry) {
		opened = append(opened, g.open(tick, CertExpiry, nil))
	}
	if h.GetStorageUtilization() >= 1 && !g.active(StorageFull) {
		opened = append(opened, g.open(tick, StorageFull, nil))
	}
	saturated := h.GetApp().GetUtilization() > 1 || h.GetDatabase().GetUtilization() > 1 || h.GetIngress().GetUtilization() > 1
	if saturated && h.GetErrorRate() > 0.02 {
		g.Overload++
	} else {
		g.Overload = 0
	}
	if g.Overload >= sustainedTicks && g.activeCount() == 0 {
		opened = append(opened, g.open(tick, CapacityExhausted, nil))
	}
	return opened
}

func (g *game) rollSurprise(tick int32, topo *lgsv1.Topology, l *lgsv1.Ledger) *incident {
	if tick < gracePeriod || g.activeCount() >= maxConcurrent {
		return nil
	}
	rng := dice.For(g.Seed, tick, "surprise")
	stage := slices.IndexFunc(catalog.Stages, func(s catalog.Stage) bool { return s.Name == l.GetStage() })

	// definitions is a map, and map order is the classic way to lose determinism.
	kinds := []string{TrafficSpike, DatabaseHotspot, RedisFailure, RegionFailure, NetworkPartition, Vulnerability}
	var total, untrained float64
	weights := make([]float64, len(kinds))
	for i, k := range kinds {
		d := definitions[k]
		if d.stage > stage {
			continue
		}
		untrained += d.weight
		if g.active(k) {
			continue
		}
		w := d.weight
		if learned, ok := g.Learned[k]; ok {
			w *= learned
		}
		weights[i] = w
		total += w
	}
	if total == 0 {
		return nil
	}
	// Debt is why the same architecture has a worse year at some companies. Scaling
	// by trained/untrained weight is what makes postmortems reduce how often things
	// go wrong, rather than only changing which thing it is.
	chance := baseChancePerTick * (1 + l.GetTechDebt()/40) * (total / untrained)
	if rng.Float64() > chance {
		return nil
	}
	roll := rng.Float64() * total
	for i, k := range kinds {
		roll -= weights[i]
		if weights[i] > 0 && roll <= 0 {
			return g.spawn(tick, k, topo, rng.Float64())
		}
	}
	return nil
}

func (g *game) spawn(tick int32, kind string, topo *lgsv1.Topology, r float64) *incident {
	return g.open(tick, kind, func(i *incident) {
		switch kind {
		case TrafficSpike:
			i.Magnitude = 1.8 + r*1.4
			i.Until = tick + 20 + int32(r*20)
		case DatabaseHotspot:
			q := hotQueries[int(r*float64(len(hotQueries)))]
			i.Index, i.SQL = q.Index, q.SQL
			i.Magnitude = 3 + r*3
			if slices.Contains(topo.GetDatabase().GetIndexes(), q.Index) {
				i.Until = tick // already indexed; a past postmortem just paid for itself
			}
		case RedisFailure:
			i.Until = tick + 25
			if topo.GetCache().GetReplicated() {
				i.Until = tick + 3
			}
		case RegionFailure:
			regions := topo.GetRegions()
			i.Region = regions[int(r*float64(len(regions)))].GetName()
			i.Until = tick + 40 + int32(r*20)
		case NetworkPartition:
			i.Magnitude = 0.5 + r*0.4
			i.Until = tick + 25
			i.Primary = topo.GetDatabase().GetPrimaryRegion()
		case Vulnerability:
			i.Until = tick + cveWindow
			i.Version = latestRunning(topo)
		}
	})
}

func (i *incident) stillHappening(tick int32, topo *lgsv1.Topology, h *lgsv1.Health) bool {
	switch i.Kind {
	case MemoryLeak, BadDeployment:
		return versionRunning(topo, i.Version)
	case CertExpiry:
		return tick >= topo.GetIngress().GetCertExpiresAtTick()
	case StorageFull:
		return h.GetStorageUtilization() >= 1
	case CapacityExhausted:
		return h.GetErrorRate() > 0.01
	case DatabaseHotspot:
		return !slices.Contains(topo.GetDatabase().GetIndexes(), i.Index) && (i.Until == 0 || tick < i.Until)
	case NetworkPartition:
		return tick < i.Until && topo.GetDatabase().GetPrimaryRegion() == i.Primary
	case RedisFailure:
		return tick < i.Until && topo.GetCache().GetNodes() > 0
	case Vulnerability:
		// Patched means every pod moved on from the affected image.
		return versionRunning(topo, i.Version)
	default:
		return tick < i.Until
	}
}

func (i *incident) apply(fx *lgsv1.Effects, tick int32, emit func(sev, kind, msg, ref string)) {
	switch i.Kind {
	case TrafficSpike:
		fx.TrafficMultiplier *= i.Magnitude
	case DatabaseHotspot:
		fx.QueryCostMultiplier *= i.Magnitude
		fx.HotQuery = i.SQL + " -- missing index: " + i.Index
	case RedisFailure:
		fx.CacheDown = true
	case RegionFailure:
		fx.RegionsDown = append(fx.RegionsDown, i.Region)
	case NetworkPartition:
		fx.PartitionLoss = i.Magnitude
	case CertExpiry:
		fx.TlsExpired = true
	case BadDeployment:
		fx.Flaws = append(fx.Flaws, &lgsv1.VersionFlaw{Version: i.Version, ErrorRate: badDeployErrRate})
	case MemoryLeak:
		fx.Flaws = append(fx.Flaws, &lgsv1.VersionFlaw{Version: i.Version, LeakMbPerTick: leakMBPerTick})
	case Vulnerability:
		if tick >= i.Until && !i.Breached {
			i.Breached = true
			fx.FineUsd += cveFine
			fx.ChurnShock += 0.08
			emit("page", "security_breach", "The CVE was exploited. Legal would like a word, and so would 8% of your users.", i.ID)
		}
	}
}

// detect is where observability pays off. An alert closes the gap immediately. A
// player with no alerting finds out from customers, later, and in worse language.
func (g *game) detect(tick int32, req *lgsv1.AdvanceRequest, emit func(sev, kind, msg, ref string)) {
	// Only pages wake someone up. A warning that a cert expires tomorrow is exactly
	// the kind of thing that sits in a channel until it's too late.
	firing := slices.ContainsFunc(req.GetFiringAlerts(), func(a *lgsv1.AlertState) bool { return a.GetSeverity() == "page" })
	hurting := req.GetLastHealth().GetErrorRate() > 0.02
	for _, inc := range g.Incidents {
		if inc.Resolved != 0 || inc.Detected != 0 {
			continue
		}
		switch {
		case inc.Kind == Vulnerability: // an advisory arrives by email, whatever you've bought
			inc.Detected, inc.DetectedBy = tick, "advisory"
		case firing:
			inc.Detected, inc.DetectedBy = tick, "alert"
		case hurting && tick-inc.Opened >= customerDelay:
			inc.Detected, inc.DetectedBy = tick, "customers"
		}
		if inc.Detected != 0 {
			emit("page", "incident_detected", definitions[inc.Kind].title+" (via "+inc.DetectedBy+")", inc.ID)
		}
	}
}

func (s *Server) Diagnose(ctx context.Context, req *lgsv1.DiagnoseRequest) (*lgsv1.DiagnoseResponse, error) {
	resp := &lgsv1.DiagnoseResponse{}
	err := s.games.Update(ctx, req.GetGameId(), func(g *game) error {
		inc := g.find(req.GetIncidentId())
		if inc == nil {
			return status.Error(codes.NotFound, "no such incident")
		}
		if inc.Diagnosed {
			resp.Correct, resp.Message = true, "Already diagnosed."
			return nil
		}
		if req.GetCause() == inc.Kind {
			inc.Diagnosed = true
			resp.Correct = true
			resp.Message = "Confirmed. Runbook unlocked."
			return nil
		}
		// A wrong theory costs real time: someone went and looked at Redis.
		inc.Wrong++
		resp.HoursPenalty = wrongGuessPenalty
		resp.Message = "That's not it. Someone spent two hours finding out."
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return resp, nil
}

// Postmortem is how a run gets stronger: the same class of surprise becomes less
// likely, because someone actually wrote down what happened and fixed the gap.
func (s *Server) Postmortem(ctx context.Context, req *lgsv1.PostmortemRequest) (*lgsv1.PostmortemResponse, error) {
	resp := &lgsv1.PostmortemResponse{}
	err := s.games.Update(ctx, req.GetGameId(), func(g *game) error {
		inc := g.find(req.GetIncidentId())
		if inc == nil {
			return status.Error(codes.NotFound, "no such incident")
		}
		if inc.Resolved == 0 {
			resp.Message = "It's still on fire. Postmortems come after."
			return nil
		}
		if inc.Postmortem {
			resp.Message = "Already written. Don't make people read two."
			return nil
		}
		inc.Postmortem = true
		learned := g.Learned[inc.Kind]
		if learned == 0 {
			learned = 1
		}
		// Floored: you can get better at region failures, not immune to them.
		g.Learned[inc.Kind] = max(learned*postmortemDampen, learnedFloor)
		resp.Ok = true
		resp.Message = "Blameless, thorough, and two of the action items might even happen."
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return resp, nil
}

func (i *incident) view() *lgsv1.IncidentView {
	d := definitions[i.Kind]
	v := &lgsv1.IncidentView{
		Id: i.ID, Title: d.title, OpenedTick: i.Opened, DetectedTick: i.Detected, ResolvedTick: i.Resolved,
		PostmortemDone: i.Postmortem, WrongGuesses: i.Wrong, DetectedBy: i.DetectedBy, Status: "firing",
	}
	if i.Diagnosed {
		v.Status = "diagnosed"
	}
	if i.Resolved != 0 {
		v.Status = "resolved"
	}
	// You get told afterwards, the way you do in real life: in the postmortem.
	if i.Diagnosed || i.Resolved != 0 {
		v.Kind = i.Kind
		v.Runbook = d.runbook
	}
	if i.Detected == 0 {
		v.Title = "" // the gateway drops these: an unnoticed incident isn't on anyone's screen
	}
	return v
}

func (g *game) open(tick int32, kind string, init func(*incident)) *incident {
	g.Seq++
	inc := &incident{ID: fmt.Sprintf("INC-%03d", g.Seq), Kind: kind, Opened: tick}
	if init != nil {
		init(inc)
	}
	g.Incidents = append(g.Incidents, inc)
	return inc
}

func (g *game) find(id string) *incident {
	for _, inc := range g.Incidents {
		if inc.ID == id {
			return inc
		}
	}
	return nil
}

func (g *game) active(kind string) bool {
	return slices.ContainsFunc(g.Incidents, func(i *incident) bool { return i.Kind == kind && i.Resolved == 0 })
}

func (g *game) openFor(kind, version string) bool {
	return slices.ContainsFunc(g.Incidents, func(i *incident) bool {
		return i.Kind == kind && i.Version == version && i.Resolved == 0
	})
}

func (g *game) activeCount() int {
	n := 0
	for _, i := range g.Incidents {
		if i.Resolved == 0 {
			n++
		}
	}
	return n
}

func versionRunning(topo *lgsv1.Topology, v string) bool {
	for _, r := range topo.GetRegions() {
		for _, p := range r.GetPods() {
			if p.GetVersion() == v {
				return true
			}
		}
	}
	return false
}

func latestRunning(topo *lgsv1.Topology) string {
	for _, r := range topo.GetRegions() {
		for _, p := range r.GetPods() {
			return p.GetVersion()
		}
	}
	return ""
}

// isHotfix treats any non-zero patch number as a hotfix: v1.3.1 yes, v1.3.0 no.
func isHotfix(v string) bool {
	var major, minor, patch int
	if _, err := fmt.Sscanf(v, "v%d.%d.%d", &major, &minor, &patch); err != nil {
		return false
	}
	return patch > 0
}

func grpcErr(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, checkpoint.ErrNoGame) {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Error(codes.Unavailable, err.Error())
}

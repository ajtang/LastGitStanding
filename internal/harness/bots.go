package harness

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/catalog"
	"github.com/ajtang/LastGitStanding/internal/clock"
	"github.com/ajtang/LastGitStanding/services/infrastructure"
)

// Player is a headless strategy. It only uses what a human gets: frames and the
// public commands. If a bot needs to peek at Effects to win, the game is unfair.
type Player interface {
	Name() string
	Act(ctx context.Context, s *Stack, gameID string, f *lgsv1.Frame) error
}

type Result struct {
	Player    string
	Seed      int64
	Days      float64
	Users     float64
	Budget    float64
	Stage     string
	Avail     float64
	Debt      float64
	Won       bool
	Outcome   string
	Score     float64
	Incidents map[string]int // by kind, as known once each one resolved
}

// Play runs one full game. maxDays caps runaway games so a too-easy balance fails
// a test rather than hanging one.
func Play(ctx context.Context, s *Stack, seed int64, p Player, maxDays int) (Result, error) {
	run, err := s.Players.CreateRun(ctx, &lgsv1.CreateRunRequest{Player: p.Name(), Seed: seed})
	if err != nil {
		return Result{}, err
	}
	gameID := run.GetGameId()
	if _, err := s.Engine.StartGame(ctx, &lgsv1.StartGameRequest{GameId: gameID, Seed: seed, Speed: 0}); err != nil {
		return Result{}, err
	}
	var frame *lgsv1.Frame
	seen := map[string]string{}
	for tick := 0; tick < maxDays*clock.TicksPerDay; tick++ {
		f, err := s.Engine.Step(ctx, gameID)
		if err != nil {
			return Result{}, fmt.Errorf("tick %d: %w", tick, err)
		}
		frame = f
		for _, i := range f.GetIncidents() {
			seen[i.GetId()] = i.GetKind()
		}
		if f.GetLedger().GetGameOverReason() != "" {
			break
		}
		if err := p.Act(ctx, s, gameID, redact(f)); err != nil {
			return Result{}, fmt.Errorf("tick %d: %s: %w", tick, p.Name(), err)
		}
	}
	l := frame.GetLedger()
	kinds := map[string]int{}
	for _, kind := range seen {
		kinds[kind]++
	}
	return Result{
		Player: p.Name(), Seed: seed, Days: float64(frame.GetTick()) / clock.TicksPerDay, Users: l.GetUsers(),
		Budget: l.GetBudget(), Stage: l.GetStage(), Avail: l.GetRollingAvailability(), Debt: l.GetTechDebt(),
		Won: l.GetWon(), Outcome: l.GetGameOverReason(), Score: l.GetScore(), Incidents: kinds,
	}, nil
}

// redact gives bots the frame the browser gets, enforcing the Player promise.
func redact(f *lgsv1.Frame) *lgsv1.Frame {
	c := proto.Clone(f).(*lgsv1.Frame)
	c.Effects = nil
	return c
}

// Neglect does nothing, forever. It should lose: if an untouched platform can
// reach an IPO, none of the systems in this repo matter.
type Neglect struct{}

func (Neglect) Name() string { return "neglect" }
func (Neglect) Act(context.Context, *Stack, string, *lgsv1.Frame) error {
	return nil
}

// Competent plays like a decent SRE: plans capacity ahead of peak, pays for
// observability, reads logs, rolls back bad releases, writes postmortems. It
// doesn't look at anything a human couldn't. It should usually get a long way.
type Competent struct {
	// Cowboy keeps the same reflexes and drops the discipline: no margin, no canary, no
	// breaker, no debt paydown, cheapest telemetry, cert renewed only after it expires.
	// The gap between this and the default is what good engineering is worth.
	Cowboy     bool
	spec       *lgsv1.PlatformSpec
	lastCommit int32
	cursed     []string // versions we've rolled back from and won't try again
}

func (b *Competent) Name() string {
	if b.Cowboy {
		return "cowboy"
	}
	return "competent"
}

var indexHint = regexp.MustCompile(`missing index: (\w+)`)

func (b *Competent) Act(ctx context.Context, s *Stack, id string, f *lgsv1.Frame) error {
	if b.spec == nil {
		spec, err := infrastructure.ParseManifest(infrastructure.StarterManifest)
		if err != nil {
			return err
		}
		b.spec = spec
	}
	want := proto.Clone(b.spec).(*lgsv1.PlatformSpec)
	l := f.GetLedger()

	if b.Cowboy {
		if f.GetTick() >= f.GetTopology().GetIngress().GetCertExpiresAtTick() {
			want.Ingress.CertRenewal = f.GetTopology().GetIngress().GetCertRenewal() + 1
		}
	} else {
		want.Ingress.CertManager = true
		want.App.CircuitBreaker = true
		want.App.Retries = 1
		want.Observability = &lgsv1.ObservabilitySpec{Metrics: "basic", Logs: "sampled", Traces: "none", Alerting: "threshold"}
		if l.GetBudget() > 8000 {
			want.Observability = &lgsv1.ObservabilitySpec{Metrics: "full", Logs: "full", Traces: "sampled", Alerting: "slo"}
			want.App.Rollout = "canary"
		}
	}
	b.planCapacity(want, f)

	// A stalled canary means git and the cluster disagree. Leaving it is a choice,
	// and not a good one: commit the decision the canary already made.
	if flux := f.GetGitops().GetFlux(); flux.GetStatus() == "stalled" && strings.HasPrefix(flux.GetDetail(), "canary") {
		b.curse(want.App.Version)
		want.App.Version = b.safest(f)
	}
	if err := b.handleIncidents(ctx, s, id, f, want); err != nil {
		return err
	}
	if err := b.shipAndHire(ctx, s, id, f, want); err != nil {
		return err
	}

	// One commit at a time, with room to land: a real team doesn't push on every
	// tick, and the bot gets no credit for out-typing the pipeline.
	if !proto.Equal(want, b.spec) && f.GetTick()-b.lastCommit >= 6 {
		_, err := s.Sim.Submit(ctx, &lgsv1.Command{GameId: id, Action: &lgsv1.Command_Commit{Commit: &lgsv1.CommitAction{
			Yaml: infrastructure.RenderManifest(want), Message: "capacity and config adjustments",
		}}})
		if err != nil {
			return err
		}
		b.spec, b.lastCommit = want, f.GetTick()
	}
	return nil
}

func (b *Competent) planCapacity(want *lgsv1.PlatformSpec, f *lgsv1.Frame) {
	l, h := f.GetLedger(), f.GetHealth()
	users := l.GetUsers() * 1.15 // plan for tomorrow, not for this minute
	peak := users * 0.01 * 1.45 * 1.3
	if b.Cowboy {
		users = l.GetUsers()
		peak = users * 0.01 * 1.45 // exactly enough, on a good day
	}

	if !b.Cowboy && l.GetUsers() > 60000 && len(want.Regions) == 1 {
		want.Regions = append(want.Regions, &lgsv1.RegionSpec{Name: "westeurope", NodeSize: "standard", Nodes: 3, Towers: 1})
		want.Database.StandbyRegion = "westeurope"
	}

	perRegion := peak / float64(len(want.Regions))
	replicas := int32(math.Ceil(perRegion / catalog.PodRPS))
	replicas = max(replicas, 3)
	want.App.Replicas = replicas
	for _, r := range want.Regions {
		if replicas > 24 {
			r.NodeSize = "large"
		}
		slots := catalog.NodeSizes[r.NodeSize].PodSlots
		r.Nodes = int32(math.Ceil(float64(replicas) / float64(slots)))
		r.Towers = max(1, int32(math.Ceil(perRegion/catalog.TowerRPS/0.7)))
	}

	want.Cache.Nodes = max(1, int32(math.Ceil(users/catalog.CacheUsers)))
	if !b.Cowboy && l.GetBudget() > 15000 {
		want.Cache.Replicated = true
	}

	dbLoad := peak * (0.25 + 2*(1-0.9))
	for _, name := range []string{"gp2", "gp4", "gp8", "gp16", "mem32"} {
		if catalog.DBTiers[name].QPS*0.7 >= dbLoad {
			if catalog.DBTiers[name].Rank > catalog.DBTiers[want.Database.Tier].Rank {
				want.Database.Tier = name
			}
			break
		}
	}

	if h.GetStorageUtilization() > 0.65 {
		want.Database.StorageGb = int32(float64(want.Database.StorageGb) * 1.5)
	}
}

func (b *Competent) handleIncidents(ctx context.Context, s *Stack, id string, f *lgsv1.Frame, want *lgsv1.PlatformSpec) error {
	for _, inc := range f.GetIncidents() {
		if inc.GetDetectedTick() == 0 {
			continue
		}
		if inc.GetResolvedTick() != 0 {
			if !b.Cowboy && !inc.GetPostmortemDone() && f.GetLedger().GetEngineerHours() > 12 {
				_, err := s.Sim.Submit(ctx, &lgsv1.Command{GameId: id, Action: &lgsv1.Command_Postmortem{Postmortem: &lgsv1.PostmortemAction{IncidentId: inc.GetId()}}})
				if err != nil {
					return err
				}
			}
			continue
		}
		switch inc.GetTitle() {
		case "Pods keep restarting", "Elevated 5xx from app":
			// Don't guess from memory: the logs name the version that's panicking. If
			// they don't name one, it wasn't a release, and a rollback wouldn't help.
			if err := b.blameFromLogs(ctx, s, id, f, want); err != nil {
				return err
			}
		case "Database latency is climbing":
			logs, err := s.Obs.Logs(ctx, &lgsv1.LogsRequest{GameId: id, Query: "missing index", Limit: 5})
			if err != nil {
				return err
			}
			for _, line := range logs.GetLines() {
				if m := indexHint.FindStringSubmatch(line.GetMessage()); m != nil && !slices.Contains(want.Database.Indexes, m[1]) {
					want.Database.Indexes = append(want.Database.Indexes, m[1])
				}
			}
		case "Security advisory for a dependency":
			for _, v := range f.GetGitops().GetVersions() {
				want.App.Version = v // newest published; ship() cuts a hotfix if none exists
			}
		case "Writes are failing":
			// Once per change, not once per tick. Your disk bill can read this code.
			if want.Database.GetStorageGb() == f.GetTopology().GetDatabase().GetStorageGb() {
				want.Database.StorageGb = want.Database.GetStorageGb() * 2
			}
		}
	}
	return nil
}

var versionHint = regexp.MustCompile(`\[version=(v[\d.]+)\]`)

func (b *Competent) blameFromLogs(ctx context.Context, s *Stack, id string, f *lgsv1.Frame, want *lgsv1.PlatformSpec) error {
	logs, err := s.Obs.Logs(ctx, &lgsv1.LogsRequest{GameId: id, Query: "version=", Limit: 10})
	if err != nil {
		return err
	}
	for _, line := range logs.GetLines() {
		if m := versionHint.FindStringSubmatch(line.GetMessage()); m != nil {
			b.curse(m[1])
		}
	}
	if slices.Contains(b.cursed, want.App.Version) {
		want.App.Version = b.safest(f)
	}
	return nil
}

func (b *Competent) curse(v string) {
	if !slices.Contains(b.cursed, v) {
		b.cursed = append(b.cursed, v)
	}
}

// safest is the newest published version nobody has caught misbehaving.
func (b *Competent) safest(f *lgsv1.Frame) string {
	versions := f.GetGitops().GetVersions()
	for _, v := range slices.Backward(versions) {
		if !slices.Contains(b.cursed, v) {
			return v
		}
	}
	return versions[0]
}

func (b *Competent) shipAndHire(ctx context.Context, s *Stack, id string, f *lgsv1.Frame, want *lgsv1.PlatformSpec) error {
	l := f.GetLedger()
	versions := f.GetGitops().GetVersions()
	for _, inc := range f.GetIncidents() {
		if inc.GetResolvedTick() == 0 && inc.GetTitle() == "Security advisory for a dependency" && l.GetEngineerHours() > 6 {
			if _, err := s.Sim.Submit(ctx, &lgsv1.Command{GameId: id, Action: &lgsv1.Command_Ship{Ship: &lgsv1.ShipAction{Kind: "hotfix"}}}); err != nil {
				return err
			}
		}
	}

	calm := !slices.ContainsFunc(f.GetIncidents(), func(i *lgsv1.IncidentView) bool { return i.GetResolvedTick() == 0 })
	if !b.Cowboy && calm && l.GetTechDebt() > 15 && l.GetEngineerHours() > 15 {
		if _, err := s.Sim.Submit(ctx, &lgsv1.Command{GameId: id, Action: &lgsv1.Command_PayDebt{PayDebt: &lgsv1.PayDebtAction{Hours: 8}}}); err != nil {
			return err
		}
	} else if calm && l.GetEngineerHours() > l.GetFeatureCostHours()+4 && f.GetGitops().GetFlux().GetStatus() == "synced" {
		if _, err := s.Sim.Submit(ctx, &lgsv1.Command{GameId: id, Action: &lgsv1.Command_Ship{Ship: &lgsv1.ShipAction{Kind: "feature"}}}); err != nil {
			return err
		}
	}
	if calm && len(versions) > 0 {
		if latest := versions[len(versions)-1]; latest != want.App.Version && !slices.Contains(b.cursed, latest) {
			want.App.Version = latest
		}
	}

	// Hire when the bank says it's fine and the backlog says it's needed.
	if want := int32(2 + l.GetUsers()/40000); l.GetEngineers() < want && l.GetBudget() > 12000 {
		if _, err := s.Sim.Submit(ctx, &lgsv1.Command{GameId: id, Action: &lgsv1.Command_Hire{Hire: &lgsv1.HireAction{Delta: 1}}}); err != nil {
			return err
		}
	}
	return nil
}

// Package infrastructure owns the path from "someone had an idea" to "the idea
// is running": a git-shaped history, a CI pipeline, Flux, and actual state.
//
// Nothing outside this package can change a resource. That constraint is the
// game mechanic, so please don't add a convenience RPC that goes around it.
package infrastructure

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
	"github.com/ajtang/LastGitStanding/internal/clock"
)

const (
	lintTicks   = 1
	policyTicks = 1
	planTicks   = 1
	mergeTicks  = 1
	// Flux polls, it doesn't get told. That gap is the thing players feel at 3am.
	fluxInterval = 5

	nodeBootTicks      = 6
	podStartTicks      = 2
	regionBuildTicks   = 24
	dbFailoverTicks    = 3
	replicaSyncTicks   = 10
	indexBuildTicks    = 4
	canaryHoldTicks    = 6
	crashloopTicks     = 3
	stallAfterTicks    = 8
	certLifetimeTicks  = 14 * clock.TicksPerDay
	firstCertExpiry    = 5 * clock.TicksPerDay
	certManagerLead    = 2 * clock.TicksPerDay
	canaryAbortErrRate = 0.04
)

type commit struct {
	SHA     string `json:"sha"`
	Parent  string `json:"parent"`
	Message string `json:"message"`
	YAML    string `json:"yaml"`
	Tick    int32  `json:"tick"`
	Status  string `json:"status"` // queued | failed | merged | applied
}

type game struct {
	Seed     int64                `json:"seed"`
	Commits  []*commit            `json:"commits"`
	Versions []string             `json:"versions"`
	Minor    int                  `json:"minor"`
	Patch    int                  `json:"patch"`
	Runs     []*lgsv1.PipelineRun `json:"runs"`

	HeadSHA    string `json:"headSha"`    // what's on main
	AppliedSHA string `json:"appliedSha"` // what Flux last applied
	FluxTimer  int32  `json:"fluxTimer"`

	Desired *lgsv1.PlatformSpec `json:"desired"`
	Actual  *lgsv1.Topology     `json:"actual"`

	PrimarySpec string `json:"primarySpec"` // what git last asked for, as opposed to what's true
	StandbySpec string `json:"standbySpec"`

	NodeETA    map[string]int32 `json:"nodeEta"`
	ReplicaETA int32            `json:"replicaEta"`
	IndexETA   map[string]int32 `json:"indexEta"`
	PodSeq     int              `json:"podSeq"`

	Stable       string `json:"stable"`       // last version that finished a rollout
	Target       string `json:"target"`       // version the rollout is working toward
	CanaryLeft   int32  `json:"canaryLeft"`   // >0 means holding at canary weight
	CanaryPassed bool   `json:"canaryPassed"` //
	Aborted      string `json:"aborted"`      // version a canary refused to promote
	StallTicks   int32  `json:"stallTicks"`

	LastTick int32                    `json:"lastTick"`
	LastResp *lgsv1.ReconcileResponse `json:"lastResp"`
}

type Server struct {
	lgsv1.UnimplementedInfrastructureServer
	games *checkpoint.Keeper[game]
}

func NewServer(store checkpoint.Store) *Server {
	return &Server{games: checkpoint.NewKeeper[game](store, "infra")}
}

func (s *Server) Keeper() *checkpoint.Keeper[game] { return s.games }

func (s *Server) Init(ctx context.Context, req *lgsv1.InitGame) (*lgsv1.InitResponse, error) {
	spec, err := ParseManifest(StarterManifest)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "starter manifest is broken, and that one's on us: %v", err)
	}
	g := &game{
		Seed:        req.GetSeed(),
		Versions:    []string{"v1.0.0"},
		Desired:     spec,
		Stable:      spec.GetApp().GetVersion(),
		PrimarySpec: spec.GetDatabase().GetPrimaryRegion(),
		StandbySpec: spec.GetDatabase().GetStandbyRegion(),
		Target:      spec.GetApp().GetVersion(),
		NodeETA:     map[string]int32{},
		IndexETA:    map[string]int32{},
		Actual:      bootstrapTopology(spec),
	}
	for _, r := range g.Actual.GetRegions() {
		for i := int32(0); i < spec.GetApp().GetReplicas(); i++ {
			r.Pods = append(r.Pods, g.newPod(r.GetName(), spec.GetApp().GetVersion(), "ready"))
		}
	}
	root := &commit{Message: "Initial platform. It's fine.", YAML: StarterManifest, Status: "applied"}
	root.SHA = hashCommit(root)
	g.Commits = []*commit{root}
	g.HeadSHA, g.AppliedSHA = root.SHA, root.SHA
	g.Actual.Revision = root.SHA
	g.FluxTimer = fluxInterval

	if err := s.games.Create(ctx, req.GetGameId(), g); err != nil {
		return nil, status.Errorf(codes.Unavailable, "checkpoint: %v", err)
	}
	return &lgsv1.InitResponse{Topology: g.Actual}, nil
}

func bootstrapTopology(spec *lgsv1.PlatformSpec) *lgsv1.Topology {
	t := &lgsv1.Topology{
		Database: &lgsv1.DatabaseState{
			Tier: spec.GetDatabase().GetTier(), StorageGb: spec.GetDatabase().GetStorageGb(),
			PrimaryRegion: spec.GetDatabase().GetPrimaryRegion(),
		},
		Cache:          &lgsv1.CacheSpec{Nodes: spec.GetCache().GetNodes(), Replicated: spec.GetCache().GetReplicated()},
		Ingress:        &lgsv1.IngressState{CertExpiresAtTick: firstCertExpiry},
		Observability:  spec.GetObservability(),
		Retries:        spec.GetApp().GetRetries(),
		CircuitBreaker: spec.GetApp().GetCircuitBreaker(),
		Rollout:        spec.GetApp().GetRollout(),
	}
	for _, r := range spec.GetRegions() {
		t.Regions = append(t.Regions, &lgsv1.RegionState{
			Name: r.GetName(), NodeSize: r.GetNodeSize(), NodesReady: r.GetNodes(), Towers: r.GetTowers(),
		})
	}
	return t
}

func (s *Server) Commit(ctx context.Context, req *lgsv1.CommitRequest) (*lgsv1.CommitResponse, error) {
	var sha string
	err := s.games.Update(ctx, req.GetGameId(), func(g *game) error {
		msg := req.GetMessage()
		if msg == "" {
			msg = "fix" // we've all done it, and now the history will say so
		}
		c := &commit{Parent: g.Commits[len(g.Commits)-1].SHA, Message: msg, YAML: req.GetYaml(), Tick: req.GetTick(), Status: "queued"}
		c.SHA = hashCommit(c)
		g.Commits = append(g.Commits, c)
		g.Runs = append(g.Runs, &lgsv1.PipelineRun{
			Sha: c.SHA, Message: msg, Stage: "lint", Status: "running",
			StageTicksLeft: lintTicks, StartedTick: req.GetTick(),
		})
		sha = c.SHA
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return &lgsv1.CommitResponse{Sha: sha}, nil
}

// PublishVersion is the image landing in the registry. It does not deploy
// anything: that still takes a commit, on purpose.
func (s *Server) PublishVersion(ctx context.Context, req *lgsv1.PublishVersionRequest) (*lgsv1.PublishVersionResponse, error) {
	var version string
	err := s.games.Update(ctx, req.GetGameId(), func(g *game) error {
		if req.GetKind() == "hotfix" {
			g.Patch++
		} else {
			g.Minor++
			g.Patch = 0
		}
		version = fmt.Sprintf("v1.%d.%d", g.Minor, g.Patch)
		g.Versions = append(g.Versions, version)
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return &lgsv1.PublishVersionResponse{Version: version}, nil
}

func (s *Server) Plan(ctx context.Context, req *lgsv1.PlanRequest) (*lgsv1.PlanResponse, error) {
	spec, err := ParseManifest(req.GetYaml())
	if err != nil {
		return &lgsv1.PlanResponse{Errors: []string{err.Error()}}, nil
	}
	errs := Lint(spec)
	err = s.games.View(ctx, req.GetGameId(), func(g *game) error {
		errs = append(errs, Policy(spec, g.Versions, g.Actual)...)
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return &lgsv1.PlanResponse{Valid: len(errs) == 0, Errors: errs, Spec: spec}, nil
}

func (s *Server) ListCommits(ctx context.Context, req *lgsv1.GameRef) (*lgsv1.ListCommitsResponse, error) {
	resp := &lgsv1.ListCommitsResponse{}
	err := s.games.View(ctx, req.GetGameId(), func(g *game) error {
		for _, c := range slices.Backward(g.Commits) {
			resp.Commits = append(resp.Commits, &lgsv1.CommitRecord{
				Sha: c.SHA, Parent: c.Parent, Message: c.Message, Yaml: c.YAML, Tick: c.Tick, Status: c.Status,
			})
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return resp, nil
}

func (s *Server) Reconcile(ctx context.Context, req *lgsv1.ReconcileRequest) (*lgsv1.ReconcileResponse, error) {
	var resp *lgsv1.ReconcileResponse
	err := s.games.Update(ctx, req.GetGameId(), func(g *game) error {
		// The conductor retries a tick if a later service in the barrier failed.
		// Applying it twice would quietly make rollouts run at double speed.
		if req.GetTick() == g.LastTick && g.LastResp != nil {
			resp = g.LastResp
			return nil
		}
		ev := &eventLog{tick: req.GetTick()}
		g.runPipelines(req.GetTick(), ev)
		g.runFlux(req.GetTick(), ev)
		g.converge(req, ev)
		resp = &lgsv1.ReconcileResponse{Topology: g.Actual, Gitops: g.gitopsStatus(), Events: ev.events}
		g.LastTick, g.LastResp = req.GetTick(), resp
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return resp, nil
}

func (g *game) runPipelines(tick int32, ev *eventLog) {
	for _, run := range g.Runs {
		if run.GetStatus() != "running" {
			continue
		}
		run.StageTicksLeft--
		if run.StageTicksLeft > 0 {
			continue
		}
		c := g.commit(run.GetSha())
		switch run.GetStage() {
		case "lint":
			spec, err := ParseManifest(c.YAML)
			if err != nil {
				g.failRun(run, c, ev, []string{err.Error()})
				continue
			}
			if errs := Lint(spec); len(errs) > 0 {
				g.failRun(run, c, ev, errs)
				continue
			}
			run.Stage, run.StageTicksLeft = "policy", policyTicks
		case "policy":
			spec, _ := ParseManifest(c.YAML)
			if errs := Policy(spec, g.Versions, g.Actual); len(errs) > 0 {
				g.failRun(run, c, ev, errs)
				continue
			}
			run.Stage, run.StageTicksLeft = "plan", planTicks
		case "plan":
			run.Stage, run.StageTicksLeft = "merge", mergeTicks
		case "merge":
			run.Stage, run.Status = "done", "passed"
			c.Status = "merged"
			g.HeadSHA = c.SHA
			ev.info("infrastructure", "merged", "Merged "+short(c.SHA)+" to main: "+c.Message, c.SHA)
		}
	}
	if len(g.Runs) > 8 {
		g.Runs = g.Runs[len(g.Runs)-8:]
	}
}

func (g *game) failRun(run *lgsv1.PipelineRun, c *commit, ev *eventLog, errs []string) {
	stage := run.GetStage()
	run.Status, run.Errors = "failed", errs
	c.Status = "failed"
	ev.warn("infrastructure", "pipeline_failed", fmt.Sprintf("Pipeline failed at %s for %s", stage, short(c.SHA)), c.SHA)
}

func (g *game) runFlux(tick int32, ev *eventLog) {
	g.FluxTimer--
	if g.FluxTimer > 0 {
		return
	}
	g.FluxTimer = fluxInterval
	if g.HeadSHA == g.AppliedSHA {
		return
	}
	c := g.commit(g.HeadSHA)
	spec, err := ParseManifest(c.YAML)
	if err != nil {
		return // can't happen past lint; if it does, Flux stalling on it is the right behaviour
	}
	g.Desired = spec
	g.AppliedSHA = c.SHA
	g.Actual.Revision = c.SHA
	c.Status = "applied"
	ev.info("infrastructure", "flux_applied", "Flux applied "+short(c.SHA), c.SHA)
}

func (g *game) gitopsStatus() *lgsv1.GitOpsStatus {
	return &lgsv1.GitOpsStatus{
		HeadSha:   g.HeadSHA,
		Pipelines: g.Runs,
		Versions:  g.Versions,
		Flux: &lgsv1.FluxStatus{
			NextSyncIn: g.FluxTimer,
			AppliedSha: g.AppliedSHA,
			TargetSha:  g.HeadSHA,
			Status:     g.fluxState(),
			Detail:     g.fluxDetail(),
		},
	}
}

func (g *game) commit(sha string) *commit {
	for _, c := range g.Commits {
		if c.SHA == sha {
			return c
		}
	}
	return nil
}

func hashCommit(c *commit) string {
	sum := sha1.Sum(fmt.Appendf(nil, "%s\x00%d\x00%s\x00%s", c.Parent, c.Tick, c.Message, c.YAML))
	return hex.EncodeToString(sum[:])
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func grpcErr(err error) error {
	if errorsIsNoGame(err) {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Error(codes.Unavailable, err.Error())
}

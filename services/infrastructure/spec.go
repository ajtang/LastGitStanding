package infrastructure

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/catalog"
)

// Azure's ceiling for a managed Postgres disk, give or take. Also what stops
// someone doubling their way past int32 and into a billion-dollar invoice.
const maxStorageGB = 32768

// StarterManifest is what the player inherits. It works today. It has a cert
// that expires soon and an apparent belief that retries are free.
const StarterManifest = `# platform.yaml
# The only way anything changes in production. No SSH. No portal. No exceptions.
regions:
  - name: eastus
    nodes: 3
    nodeSize: standard   # small | standard | large
    towers: 1            # load balancers

app:
  version: v1.0.0
  replicas: 3            # per region
  rollout: rolling       # rolling | canary
  retries: 2
  circuitBreaker: false

database:
  tier: gp2              # burst1 | gp2 | gp4 | gp8 | gp16 | mem32
  readReplicas: 0
  storageGb: 100
  indexes: []
  primaryRegion: eastus
  standbyRegion: ""

cache:
  nodes: 1
  replicated: false

ingress:
  certManager: false
  certRenewal: 0         # someone set a calendar reminder for this. They left.

observability:
  metrics: basic         # basic | full
  logs: sampled          # none | sampled | full
  traces: none           # none | sampled | full
  alerting: threshold    # none | threshold | slo
`

// ParseManifest goes YAML -> JSON -> proto so a typo in a field name fails lint,
// the way it would against a real CRD schema. The errors are a little raw. So
// are kubectl's.
func ParseManifest(src string) (*lgsv1.PlatformSpec, error) {
	raw, err := yaml.YAMLToJSON([]byte(src))
	if err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	spec := &lgsv1.PlatformSpec{}
	if err := protojson.Unmarshal(raw, spec); err != nil {
		return nil, fmt.Errorf("schema: %s", strings.ReplaceAll(err.Error(), " ", " "))
	}
	return spec, nil
}

// RenderManifest loses comments, which is the correct amount of respect for
// comments when a bot is doing the editing. Used by the headless players.
func RenderManifest(spec *lgsv1.PlatformSpec) string {
	raw, err := protojson.Marshal(spec)
	if err != nil {
		panic(err)
	}
	out, err := yaml.JSONToYAML(raw)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// Lint catches what a schema can. Policy, below, catches what only makes sense
// in context. The split is the same one you'd want from OPA in real life, and it
// means a typo fails fast rather than after you've waited for the policy stage.
func Lint(spec *lgsv1.PlatformSpec) []string {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if len(spec.GetRegions()) == 0 {
		fail("regions: at least one region is required (running nowhere is very cheap but a hard sell)")
	}
	seen := map[string]bool{}
	for _, r := range spec.GetRegions() {
		if !catalog.IsRegion(r.GetName()) {
			fail("regions: unknown region %q (have %s)", r.GetName(), strings.Join(catalog.Regions, ", "))
		}
		if seen[r.GetName()] {
			fail("regions: %q is listed twice", r.GetName())
		}
		seen[r.GetName()] = true
		if _, ok := catalog.NodeSizes[r.GetNodeSize()]; !ok {
			fail("regions.%s.nodeSize: unknown size %q", r.GetName(), r.GetNodeSize())
		}
		if r.GetNodes() < 1 || r.GetNodes() > 100 {
			fail("regions.%s.nodes: must be between 1 and 100", r.GetName())
		}
		if r.GetTowers() < 1 {
			fail("regions.%s.towers: need at least one load balancer", r.GetName())
		}
	}
	if !slices.Contains([]string{"rolling", "canary"}, spec.GetApp().GetRollout()) {
		fail("app.rollout: must be rolling or canary, got %q", spec.GetApp().GetRollout())
	}
	if r := spec.GetApp().GetReplicas(); r < 1 || r > catalog.MaxPerRegion {
		fail("app.replicas: must be between 1 and %d", catalog.MaxPerRegion)
	}
	if r := spec.GetApp().GetRetries(); r < 0 || r > 10 {
		fail("app.retries: must be between 0 and 10")
	}
	if _, ok := catalog.DBTiers[spec.GetDatabase().GetTier()]; !ok {
		fail("database.tier: unknown tier %q", spec.GetDatabase().GetTier())
	}
	if spec.GetDatabase().GetReadReplicas() < 0 || spec.GetDatabase().GetReadReplicas() > 5 {
		fail("database.readReplicas: must be between 0 and 5")
	}
	if g := spec.GetDatabase().GetStorageGb(); g < 10 || g > maxStorageGB {
		fail("database.storageGb: must be between 10 and %d", maxStorageGB)
	}
	if spec.GetCache().GetNodes() < 0 || spec.GetCache().GetNodes() > 20 {
		fail("cache.nodes: must be between 0 and 20")
	}
	obs := spec.GetObservability()
	checkEnum := func(field, val string, table map[string]float64) {
		if _, ok := table[val]; !ok {
			fail("observability.%s: unknown value %q", field, val)
		}
	}
	checkEnum("metrics", obs.GetMetrics(), catalog.MetricsPerDay)
	checkEnum("logs", obs.GetLogs(), catalog.LogsPerDay)
	checkEnum("traces", obs.GetTraces(), catalog.TracesPerDay)
	checkEnum("alerting", obs.GetAlerting(), catalog.AlertsPerDay)
	return errs
}

// Policy needs the catalog of published versions and what's live right now.
func Policy(spec *lgsv1.PlatformSpec, versions []string, live *lgsv1.Topology) []string {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if !slices.Contains(versions, spec.GetApp().GetVersion()) {
		fail("app.version: %q has no published image. Ship it first; hope is not a registry.", spec.GetApp().GetVersion())
	}
	db := spec.GetDatabase()
	names := make([]string, 0, len(spec.GetRegions()))
	for _, r := range spec.GetRegions() {
		names = append(names, r.GetName())
	}
	if !slices.Contains(names, db.GetPrimaryRegion()) {
		fail("database.primaryRegion: %q is not one of your regions", db.GetPrimaryRegion())
	}
	if s := db.GetStandbyRegion(); s != "" {
		if s == db.GetPrimaryRegion() {
			fail("database.standbyRegion: a standby in the same region is a very expensive replica of your problem")
		}
		if !slices.Contains(names, s) {
			fail("database.standbyRegion: %q is not one of your regions", s)
		}
	}
	if live != nil && db.GetStorageGb() < live.GetDatabase().GetStorageGb() {
		fail("database.storageGb: managed disks only grow (%d -> %d). Everyone learns this once.",
			live.GetDatabase().GetStorageGb(), db.GetStorageGb())
	}
	return errs
}

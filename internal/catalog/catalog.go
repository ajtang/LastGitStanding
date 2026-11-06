// Package catalog is the price list and the physics. Capacity and cost live in
// the same table on purpose: if you tune one without looking at the other, you
// have invented a free lunch and every player will find it within a day.
package catalog

const (
	PodRPS        = 60.0   // one app pod, CPU-bound, before anything goes wrong
	PodMemoryMB   = 4096.0 // limit
	PodBaseMB     = 1800.0
	TowerRPS      = 900.0
	TowerPerDay   = 22.0
	CachePerDay   = 28.0
	CacheReplica  = 1.6 // replication is not free; it is merely cheaper than the outage
	CacheUsers    = 40000.0
	RegionPerDay  = 120.0 // peering, duplicated control plane, the meeting about it
	StoragePerGB  = 0.09
	CertMgrPerDay = 3.0
	EngineerDay   = 420.0
	HiringFee     = 2500.0
	MaxPerRegion  = 200
)

type NodeSize struct {
	PodSlots   int32
	CostPerDay float64
}

var NodeSizes = map[string]NodeSize{
	"small":    {PodSlots: 2, CostPerDay: 32},
	"standard": {PodSlots: 4, CostPerDay: 58},
	"large":    {PodSlots: 16, CostPerDay: 205}, // the bin-packing discount, payable in blast radius
}

type DBTier struct {
	QPS        float64
	CostPerDay float64
	Rank       int
}

// Doubling cores costs a bit more than double, because that is how it works.
var DBTiers = map[string]DBTier{
	"burst1": {QPS: 150, CostPerDay: 35, Rank: 0},
	"gp2":    {QPS: 500, CostPerDay: 75, Rank: 1},
	"gp4":    {QPS: 1000, CostPerDay: 160, Rank: 2},
	"gp8":    {QPS: 2000, CostPerDay: 340, Rank: 3},
	"gp16":   {QPS: 4000, CostPerDay: 720, Rank: 4},
	"mem32":  {QPS: 8000, CostPerDay: 1600, Rank: 5},
}

// Regions the game knows about. Names match Azure so the muscle memory transfers.
var Regions = []string{"eastus", "westeurope", "southeastasia", "westus2", "uksouth"}

func IsRegion(name string) bool {
	for _, r := range Regions {
		if r == name {
			return true
		}
	}
	return false
}

// Observability is priced like the real thing: logs scale with traffic, which
// is why every company eventually has a meeting about its log bill.
var (
	MetricsPerDay = map[string]float64{"basic": 0, "full": 45}
	LogsPerDay    = map[string]float64{"none": 0, "sampled": 18, "full": 40}
	LogsPerRPSDay = map[string]float64{"none": 0, "sampled": 0.02, "full": 0.12}
	TracesPerDay  = map[string]float64{"none": 0, "sampled": 25, "full": 70}
	AlertsPerDay  = map[string]float64{"none": 0, "threshold": 8, "slo": 30}
)

// Stage is a funding round: a pile of money in exchange for higher expectations.
type Stage struct {
	Name      string
	NextUsers float64 // users needed to close the next round
	Grant     float64 // paid on entering the *next* stage
	SLO       float64
}

var Stages = []Stage{
	{Name: "Seed", NextUsers: 20000, Grant: 40000, SLO: 0.99},
	{Name: "Series A", NextUsers: 80000, Grant: 120000, SLO: 0.995},
	{Name: "Series B", NextUsers: 200000, Grant: 300000, SLO: 0.999},
	{Name: "Series C", NextUsers: 500000, Grant: 0, SLO: 0.999},
}

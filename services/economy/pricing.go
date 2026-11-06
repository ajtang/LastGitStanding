package economy

import (
	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/catalog"
)

// costOfTopology bills what exists, including nodes still booting and failed
// regions. Your cloud provider also charges you during their outage.
func costOfTopology(t *lgsv1.Topology, rps float64) float64 {
	cost := 0.0
	for i, r := range t.GetRegions() {
		nodes := r.GetNodesReady() + r.GetNodesProvisioning()
		cost += float64(nodes)*catalog.NodeSizes[r.GetNodeSize()].CostPerDay + float64(r.GetTowers())*catalog.TowerPerDay
		if i > 0 {
			cost += catalog.RegionPerDay
		}
	}
	db := t.GetDatabase()
	cost += databaseCost(db.GetTier(), db.GetReadReplicas(), db.GetStorageGb(), db.GetStandbyRegion())
	cost += cacheCost(t.GetCache())
	if t.GetIngress().GetCertManager() {
		cost += catalog.CertMgrPerDay
	}
	return cost + observabilityCost(t.GetObservability(), rps)
}

func databaseCost(tier string, replicas, storageGB int32, standby string) float64 {
	unit := catalog.DBTiers[tier].CostPerDay
	copies := float64(1 + replicas)
	if standby != "" {
		copies++ // a standby is a whole database that does nothing until the worst day of your year
	}
	return unit*copies + float64(storageGB)*catalog.StoragePerGB*copies
}

func cacheCost(c *lgsv1.CacheSpec) float64 {
	cost := float64(c.GetNodes()) * catalog.CachePerDay
	if c.GetReplicated() {
		cost *= catalog.CacheReplica
	}
	return cost
}

func observabilityCost(o *lgsv1.ObservabilitySpec, rps float64) float64 {
	return catalog.MetricsPerDay[o.GetMetrics()] +
		catalog.LogsPerDay[o.GetLogs()] + catalog.LogsPerRPSDay[o.GetLogs()]*rps +
		catalog.TracesPerDay[o.GetTraces()] +
		catalog.AlertsPerDay[o.GetAlerting()]
}

// quoteSpec is what the editor shows while you're typing. It leaves out engineers
// and traffic-scaled log costs, so a line of YAML is the only thing on the bill.
func quoteSpec(s *lgsv1.PlatformSpec) *lgsv1.QuoteResponse {
	resp := &lgsv1.QuoteResponse{}
	add := func(name string, cost float64) {
		if cost > 0 {
			resp.Items = append(resp.Items, &lgsv1.LineItem{Name: name, CostPerDay: cost})
			resp.CostPerDay += cost
		}
	}
	for i, r := range s.GetRegions() {
		add("nodes "+r.GetName(), float64(r.GetNodes())*catalog.NodeSizes[r.GetNodeSize()].CostPerDay)
		add("load balancers "+r.GetName(), float64(r.GetTowers())*catalog.TowerPerDay)
		if i > 0 {
			add("region premium "+r.GetName(), catalog.RegionPerDay)
		}
	}
	db := s.GetDatabase()
	add("database", databaseCost(db.GetTier(), db.GetReadReplicas(), db.GetStorageGb(), db.GetStandbyRegion()))
	add("cache", cacheCost(s.GetCache()))
	if s.GetIngress().GetCertManager() {
		add("cert-manager", catalog.CertMgrPerDay)
	}
	add("observability", observabilityCost(s.GetObservability(), 0))
	return resp
}

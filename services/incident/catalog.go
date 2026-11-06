package incident

// Kinds are also the answers the player can give on a diagnosis. If you add one,
// add it to web/lib/causes.ts too or nobody will ever be able to name it.
const (
	TrafficSpike      = "traffic_spike"
	MemoryLeak        = "memory_leak"
	DatabaseHotspot   = "database_hotspot"
	BadDeployment     = "bad_deployment"
	CertExpiry        = "certificate_expiry"
	RedisFailure      = "redis_failure"
	RegionFailure     = "region_failure"
	NetworkPartition  = "network_partition"
	CapacityExhausted = "capacity_exhausted"
	StorageFull       = "storage_full"
	Vulnerability     = "vulnerability"
)

type definition struct {
	// Titles describe symptoms, and several kinds share one on purpose. The page
	// tells you where it hurts. It doesn't tell you why.
	title   string
	runbook string
	weight  float64 // relative chance when the dice say "something happens"
	stage   int     // earliest funding stage it can appear at randomly; -1 never random
}

var definitions = map[string]definition{
	TrafficSpike: {
		title:   "Requests are being shed",
		runbook: "Marketing got lucky, or unlucky. It will pass, but your retries may keep the overload alive after it does. Scale the app, and take retries down or turn on the circuit breaker if the fleet won't recover on its own.",
		weight:  3, stage: 0,
	},
	MemoryLeak: {
		title:   "Pods keep restarting",
		runbook: "The sawtooth in memory started with a release. Roll app.version back or ship a hotfix. More replicas only spreads the restarts out, and a canary won't catch this: it's too slow to show up during the hold.",
		stage:   -1,
	},
	DatabaseHotspot: {
		title:   "Database latency is climbing",
		runbook: "One query dominates. The slow query log names the index it wants: add it under database.indexes. A bigger tier buys time at a price, and it will still be there tomorrow.",
		weight:  2, stage: 0,
	},
	BadDeployment: {
		title:   "Elevated 5xx from app",
		runbook: "Errors correlate with one version. Revert app.version to the previous one. You can fix forward with a hotfix, but the rollback is what your users would vote for.",
		stage:   -1,
	},
	CertExpiry: {
		title:   "Clients can't connect",
		runbook: "Pods are idle because nothing can complete a handshake. Bump ingress.certRenewal to renew now, then set ingress.certManager: true so this is never your problem again.",
		stage:   -1,
	},
	RedisFailure: {
		title:   "Database latency is climbing",
		runbook: "The cache is gone, so its traffic is now the database's. Unreplicated Redis is down until it's rebuilt, and it comes back cold. Buy DB headroom now. cache.replicated: true turns this into a blip next time.",
		weight:  2, stage: 1,
	},
	RegionFailure: {
		title:   "A region stopped answering",
		runbook: "The region is gone until the cloud says otherwise. Traffic only moves if you have somewhere for it to go, and the database only survives with a standby elsewhere. If you have neither, the status page is your remaining tool.",
		weight:  1, stage: 2,
	},
	NetworkPartition: {
		title:   "Elevated 5xx from app",
		runbook: "Every pod is green and calls to the primary time out; traces show it. Promote a standby by changing database.primaryRegion if you have one. Otherwise wait, and lower retries so they don't pile on. A circuit breaker keeps threads from stalling on dead connections.",
		weight:  1.5, stage: 1,
	},
	CapacityExhausted: {
		title:   "Requests are being shed",
		runbook: "No exotic cause: you ran out. Check app, tower and database saturation, then scale whichever is pinned. Remember the path from commit to capacity takes time, so scale before peak next time.",
		stage:   -1,
	},
	StorageFull: {
		title:   "Writes are failing",
		runbook: "The disk is full and the database went read-only. Raise database.storageGb. It only goes up, so maybe alert at 80% next time.",
		stage:   -1,
	},
	Vulnerability: {
		title:   "Security advisory for a dependency",
		runbook: "A CVE affects the running image. Ship a hotfix and deploy it before the patch window closes. Nothing is broken yet, which is exactly why this gets deferred, and why the fine is so large.",
		weight:  1, stage: 1,
	},
}

// Queries the hotspot can pick. Each names the index that fixes it, and that name
// only appears in the slow query log, so logs are load-bearing for this incident.
var hotQueries = []struct{ Index, SQL string }{
	{"orders_customer_id", "SELECT * FROM orders WHERE customer_id = $1 ORDER BY created_at DESC"},
	{"events_tenant_ts", "SELECT count(*) FROM events WHERE tenant_id = $1 AND ts > now() - interval '1 day'"},
	{"sessions_token", "SELECT * FROM sessions WHERE token = $1"},
	{"invoices_status", "SELECT * FROM invoices WHERE status = 'open' ORDER BY due_at"},
}

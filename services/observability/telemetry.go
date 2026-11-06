package observability

import (
	"fmt"
	"strings"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/dice"
)

func matches(l *lgsv1.LogLine, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	return strings.Contains(strings.ToLower(l.GetMessage()), q) ||
		strings.Contains(strings.ToLower(l.GetService()), q) ||
		strings.Contains(strings.ToLower(l.GetLevel()), q)
}

// generateLogs writes what each component would have written. It never names a
// cause. Everything here is a symptom that a person has to interpret, which is
// why the cert line talks about a handshake and not about a certificate incident.
func generateLogs(f *lgsv1.Frame) []*lgsv1.LogLine {
	var lines []*lgsv1.LogLine
	h, fx, tick := f.GetHealth(), f.GetEffects(), f.GetTick()
	rng := dice.For(int64(len(f.GetGameId())), tick, "logs")
	log := func(level, service, format string, args ...any) {
		lines = append(lines, &lgsv1.LogLine{Tick: tick, Level: level, Service: service, Message: fmt.Sprintf(format, args...)})
	}

	if rng.Float64() < 0.5 {
		log("info", "app", "GET /api/orders 200 %.0fms", h.GetLatencyP50Ms())
	}
	if rng.Float64() < 0.1 {
		log("info", "app", "GC pause %.1fms, heap %.0f%%", 2+rng.Float64()*6, h.GetMemoryUtilization()*100)
	}

	if fx.GetTlsExpired() {
		log("error", "ingress", "tls: client handshake failed: remote error: tls: bad certificate")
		log("warn", "edge-probe", "x509: certificate has expired or is not yet valid")
	}
	if fx.GetCacheDown() {
		log("error", "app", "redis: dial tcp 10.0.4.12:6379: connect: connection refused")
		log("warn", "app", "cache miss fallback: querying primary directly")
	}
	if q := fx.GetHotQuery(); q != "" {
		log("warn", "postgres", "duration: %.0f ms  statement: %s", 1800+rng.Float64()*2400, q)
	}
	if p := fx.GetPartitionLoss(); p > 0 && rng.Float64() < 0.5+p/2 {
		log("error", "app", "pq: dial tcp 10.0.8.4:5432: i/o timeout")
	}
	if h.GetDatabase().GetUtilization() > 0.95 {
		log("warn", "postgres", "connection pool saturated; %.0f%% of capacity, queries waiting", h.GetDatabase().GetUtilization()*100)
	}
	if h.GetStorageUtilization() >= 1 {
		log("error", "postgres", "could not extend file \"base/16384/2619\": No space left on device")
		log("error", "app", "pq: cannot execute INSERT in a read-only transaction")
	} else if h.GetStorageUtilization() > 0.85 {
		log("warn", "postgres", "disk usage at %.0f%%", h.GetStorageUtilization()*100)
	}
	for _, flaw := range fx.GetFlaws() {
		if flaw.GetErrorRate() > 0 {
			log("error", "app", "panic: runtime error: invalid memory address or nil pointer dereference [version=%s]", flaw.GetVersion())
		}
		if flaw.GetLeakMbPerTick() > 0 && rng.Float64() < 0.6 {
			log("warn", "app", "heap in use growing without bound since start [version=%s]", flaw.GetVersion())
		}
	}
	for _, pod := range h.GetOomKilledPods() {
		log("error", "kubelet", "Container app in pod %s was OOMKilled (exit 137)", pod)
	}
	if h.GetApp().GetUtilization() > 1 {
		log("warn", "app", "request queue full, shedding load (%.0f%% saturated)", h.GetApp().GetUtilization()*100)
	}
	if h.GetRetryAmplification() > 1.3 {
		log("warn", "app", "client retries amplifying load x%.1f", h.GetRetryAmplification())
	}
	if h.GetIngress().GetUtilization() > 1 {
		log("error", "ingress", "503 upstream connect error: load balancer connection limit reached")
	}
	for _, r := range h.GetRegions() {
		if !r.GetUp() && r.GetPodsReady() > 0 {
			log("error", "edge-probe", "health check failed for %s: context deadline exceeded", r.GetName())
		}
		if r.GetPodsPending() > 0 && rng.Float64() < 0.5 {
			log("warn", "scheduler", "%d pods pending in %s: 0/N nodes available: insufficient cpu", r.GetPodsPending(), r.GetName())
		}
	}
	return lines
}

// generateTraces synthesizes one request per tick from what the model computed.
// Traces are what crack the incidents logs can't: a partition looks like healthy pods
// with a very long, very sad database span.
//
// Sampling is tail-biased toward cache misses, as real tail sampling favours slow
// requests. A faithful 92% of traces saying "redis: hit" would be accurate and useless.
func generateTraces(f *lgsv1.Frame) []*lgsv1.Trace {
	h, fx, tick := f.GetHealth(), f.GetEffects(), f.GetTick()
	if h == nil {
		return nil
	}
	rng := dice.For(int64(len(f.GetGameId())), tick, "trace")
	hitCache := rng.Float64() < h.GetCacheHitRatio()*0.4
	failing := rng.Float64() < h.GetErrorRate()

	tr := &lgsv1.Trace{Id: fmt.Sprintf("%016x", rng.Uint64()), Tick: tick, Error: failing}
	cursor := 0.0
	span := func(service, op string, dur float64, err bool, note string) {
		tr.Spans = append(tr.Spans, &lgsv1.Span{Service: service, Operation: op, StartMs: cursor, DurationMs: dur, Error: err, Note: note})
		cursor += dur
	}

	if fx.GetTlsExpired() && failing {
		span("ingress", "TLS handshake", h.GetIngress().GetLatencyMs(), true, "handshake failed before any request was sent")
		tr.TotalMs = cursor
		return []*lgsv1.Trace{tr}
	}

	span("ingress", "route", h.GetIngress().GetLatencyMs(), false, "")
	appDur := h.GetApp().GetLatencyMs()
	note := ""
	if h.GetApp().GetUtilization() > 0.9 {
		note = "most of this was time in queue"
	}
	span("app", "GET /api/orders", appDur, failing && h.GetApp().GetErrorRate() > 0.05, note)

	if fx.GetCacheDown() {
		span("redis", "GET order:*", 250, true, "connection refused; 250ms wasted before falling back")
		hitCache = false
	} else {
		span("redis", "GET order:*", h.GetCache().GetLatencyMs(), false, map[bool]string{true: "hit", false: "miss"}[hitCache])
	}
	if !hitCache {
		dbDur := h.GetDatabase().GetLatencyMs()
		dbErr := failing && h.GetDatabase().GetErrorRate() > 0.02
		dbNote := ""
		switch {
		case fx.GetPartitionLoss() > 0 && failing:
			dbDur = 3000
			dbErr = true
			dbNote = "connect timeout: no route to primary, peer reports healthy"
		case fx.GetHotQuery() != "":
			dbNote = "plan: Seq Scan. rows examined: 4,812,330"
		case h.GetDatabase().GetUtilization() > 1:
			dbNote = "waited on connection pool"
		}
		span("postgres", "SELECT orders", dbDur, dbErr, dbNote)
	}
	tr.TotalMs = cursor
	return []*lgsv1.Trace{tr}
}

type rule struct {
	name, severity string
	holdThreshold  int32
	holdSLO        int32
	sloOnly        bool
	check          func(f *lgsv1.Frame) (bool, string)
}

// Threshold alerting waits and misses things. SLO burn-rate alerting pages sooner
// and also sees what's coming: that gap is the whole pitch for the upgrade.
var rules = []rule{
	{"HighErrorRate", "page", 3, 1, false, func(f *lgsv1.Frame) (bool, string) {
		e := f.GetHealth().GetErrorRate()
		return e > 0.05, fmt.Sprintf("%.1f%% of requests failing", e*100)
	}},
	{"HighLatency", "page", 3, 1, false, func(f *lgsv1.Frame) (bool, string) {
		p := f.GetHealth().GetLatencyP95Ms()
		return p > 600, fmt.Sprintf("p95 at %.0fms", p)
	}},
	{"ErrorBudgetBurn", "page", 0, 1, true, func(f *lgsv1.Frame) (bool, string) {
		e := f.GetHealth().GetErrorRate()
		allowed := 1 - f.GetLedger().GetSloTarget()
		return allowed > 0 && e > allowed*10, fmt.Sprintf("burning error budget at %.0fx", e/max(allowed, 0.0001))
	}},
	{"AppSaturation", "warn", 4, 2, false, func(f *lgsv1.Frame) (bool, string) {
		u := f.GetHealth().GetApp().GetUtilization()
		return u > 0.85, fmt.Sprintf("app fleet at %.0f%%", u*100)
	}},
	{"DatabaseSaturation", "warn", 4, 2, false, func(f *lgsv1.Frame) (bool, string) {
		u := f.GetHealth().GetDatabase().GetUtilization()
		return u > 0.85, fmt.Sprintf("database at %.0f%%", u*100)
	}},
	{"LoadBalancerSaturation", "warn", 4, 2, true, func(f *lgsv1.Frame) (bool, string) {
		u := f.GetHealth().GetIngress().GetUtilization()
		return u > 0.8, fmt.Sprintf("towers at %.0f%%", u*100)
	}},
	{"DiskFilling", "warn", 1, 1, false, func(f *lgsv1.Frame) (bool, string) {
		u := f.GetHealth().GetStorageUtilization()
		return u > 0.8, fmt.Sprintf("database disk at %.0f%%", u*100)
	}},
	{"CertExpiringSoon", "warn", 1, 1, false, func(f *lgsv1.Frame) (bool, string) {
		left := f.GetTopology().GetIngress().GetCertExpiresAtTick() - f.GetTick()
		return left < 96 && left > 0, fmt.Sprintf("TLS certificate expires in %d ticks", left)
	}},
	{"PodCrashLooping", "warn", 1, 1, false, func(f *lgsv1.Frame) (bool, string) {
		n := len(f.GetHealth().GetOomKilledPods())
		return n > 0, fmt.Sprintf("%d pods killed this tick", n)
	}},
	{"MemoryGrowth", "warn", 0, 2, true, func(f *lgsv1.Frame) (bool, string) {
		u := f.GetHealth().GetMemoryUtilization()
		return u > 0.8, fmt.Sprintf("worst pod at %.0f%% of its limit", u*100)
	}},
}

func evaluateAlerts(g *game, f *lgsv1.Frame) *lgsv1.IngestResponse {
	tier := f.GetTopology().GetObservability().GetAlerting()
	resp := &lgsv1.IngestResponse{}
	if tier == "" || tier == "none" {
		g.alerts = map[string]*firing{}
		return resp
	}
	for _, r := range rules {
		st, ok := g.alerts[r.name]
		if !ok {
			st = &firing{}
			g.alerts[r.name] = st
		}
		bad, summary := r.check(f)
		hold := r.holdThreshold
		if tier == "slo" {
			hold = r.holdSLO
		}
		if !bad || (r.sloOnly && tier != "slo") {
			st.hot, st.active = 0, false
			continue
		}
		st.hot++
		st.summary = summary
		if st.hot >= hold && !st.active {
			st.active, st.since = true, f.GetTick()
			resp.Events = append(resp.Events, &lgsv1.Event{
				Tick: f.GetTick(), Source: "observability", Kind: "alert_firing",
				Severity: r.severity, Message: r.name + ": " + summary, Ref: r.name,
			})
		}
		if st.active {
			resp.Firing = append(resp.Firing, &lgsv1.AlertState{Name: r.name, Severity: r.severity, Summary: summary, SinceTick: st.since})
		}
	}
	return resp
}

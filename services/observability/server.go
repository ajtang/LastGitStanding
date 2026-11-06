// Package observability decides what the player is allowed to know.
//
// The flow model knows exactly what's wrong. This service gets that truth and
// hands back only what the player has paid to see, in the form real tools would
// show it: symptoms, noise, and the odd clue. Telemetry is kept in memory and not
// checkpointed. A restart loses history, as restarts of cheap telemetry do.
package observability

import (
	"context"
	"sync"
	"time"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

const (
	historyTicks     = 960 // ten game days at full resolution
	logRetention     = 400
	traceRetention   = 24
	basicMetricsStep = 5
)

// forgetAfter is how long telemetry outlives the last tick. This service keeps nothing
// in the store, so a player back after longer than this gets empty charts, the same as
// after a restart, and they refill as the game runs.
const forgetAfter = 24 * time.Hour

type game struct {
	ingested time.Time // guarded by the Server's mu
	mu       sync.Mutex
	frames   []*sample
	logs     []*lgsv1.LogLine
	traces   []*lgsv1.Trace
	alerts   map[string]*firing
	lastTick int32
	lastResp *lgsv1.IngestResponse
	obs      *lgsv1.ObservabilitySpec
}

type sample struct {
	tick int32
	h    *lgsv1.Health
}

type firing struct {
	since   int32
	hot     int32 // consecutive ticks the condition has held
	active  bool
	summary string
}

type Server struct {
	lgsv1.UnimplementedObservabilityServer
	now   func() time.Time
	mu    sync.Mutex
	games map[string]*game
	swept time.Time
}

func NewServer() *Server { return &Server{now: time.Now, games: map[string]*game{}} }

// game is for Ingest, the only thing allowed to bring a game into existence here.
func (s *Server) game(id string) *game {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Sub(s.swept) >= time.Hour {
		s.swept = now
		for k, g := range s.games {
			if now.Sub(g.ingested) >= forgetAfter {
				delete(s.games, k)
			}
		}
	}
	g, ok := s.games[id]
	if !ok {
		g = &game{alerts: map[string]*firing{}}
		s.games[id] = g
	}
	g.ingested = now
	return g
}

// peek is for the read paths. These take a game ID straight from a URL, so if they
// created entries, anyone could fill this map by asking about games that don't exist.
func (s *Server) peek(id string) *game {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok := s.games[id]; ok {
		return g
	}
	return &game{alerts: map[string]*firing{}}
}

func (s *Server) Ingest(_ context.Context, f *lgsv1.Frame) (*lgsv1.IngestResponse, error) {
	g := s.game(f.GetGameId())
	g.mu.Lock()
	defer g.mu.Unlock()
	if f.GetTick() == g.lastTick && g.lastResp != nil {
		return g.lastResp, nil
	}

	g.obs = f.GetTopology().GetObservability()
	g.frames = append(g.frames, &sample{tick: f.GetTick(), h: f.GetHealth()})
	if len(g.frames) > historyTicks {
		g.frames = g.frames[1:]
	}
	g.logs = append(g.logs, generateLogs(f)...)
	if len(g.logs) > logRetention {
		g.logs = g.logs[len(g.logs)-logRetention:]
	}
	g.traces = append(g.traces, generateTraces(f)...)
	if len(g.traces) > traceRetention {
		g.traces = g.traces[len(g.traces)-traceRetention:]
	}

	resp := evaluateAlerts(g, f)
	g.lastTick, g.lastResp = f.GetTick(), resp
	return resp, nil
}

type metricDef struct {
	name string
	unit string
	get  func(*lgsv1.Health) float64
	full bool // only for people who pay for the good dashboard
}

var metrics = []metricDef{
	{"availability", "%", func(h *lgsv1.Health) float64 { return (1 - h.GetErrorRate()) * 100 }, false},
	{"latency_mean", "ms", func(h *lgsv1.Health) float64 { return h.GetLatencyMeanMs() }, false},
	{"traffic", "rps", func(h *lgsv1.Health) float64 { return h.GetOfferedRps() }, false},
	{"latency_p50", "ms", func(h *lgsv1.Health) float64 { return h.GetLatencyP50Ms() }, true},
	{"latency_p99", "ms", func(h *lgsv1.Health) float64 { return h.GetLatencyP99Ms() }, true},
	{"app_saturation", "%", func(h *lgsv1.Health) float64 { return h.GetApp().GetUtilization() * 100 }, true},
	{"db_saturation", "%", func(h *lgsv1.Health) float64 { return h.GetDatabase().GetUtilization() * 100 }, true},
	{"tower_saturation", "%", func(h *lgsv1.Health) float64 { return h.GetIngress().GetUtilization() * 100 }, true},
	{"cache_hit_ratio", "%", func(h *lgsv1.Health) float64 { return h.GetCacheHitRatio() * 100 }, true},
	{"memory", "%", func(h *lgsv1.Health) float64 { return h.GetMemoryUtilization() * 100 }, true},
	{"storage", "%", func(h *lgsv1.Health) float64 { return h.GetStorageUtilization() * 100 }, true},
	{"retry_amplification", "x", func(h *lgsv1.Health) float64 { return h.GetRetryAmplification() }, true},
}

func (s *Server) Metrics(_ context.Context, req *lgsv1.MetricsRequest) (*lgsv1.MetricsResponse, error) {
	g := s.peek(req.GetGameId())
	g.mu.Lock()
	defer g.mu.Unlock()

	tier := g.obs.GetMetrics()
	step := 1
	if tier != "full" {
		// Five-tick averages. A short, sharp failure is a slight bump on this chart,
		// which is why people who've been burned buy higher resolution.
		step = basicMetricsStep
	}
	window := int(req.GetWindow())
	if window <= 0 || window > len(g.frames) {
		window = len(g.frames)
	}
	frames := g.frames[len(g.frames)-window:]

	resp := &lgsv1.MetricsResponse{Tier: tier}
	for _, def := range metrics {
		if def.full && tier != "full" {
			continue
		}
		series := &lgsv1.Series{Name: def.name, Unit: def.unit, ResolutionTicks: int32(step)}
		for i := 0; i < len(frames); i += step {
			end := min(i+step, len(frames))
			var sum float64
			for _, f := range frames[i:end] {
				sum += def.get(f.h)
			}
			series.Ticks = append(series.Ticks, frames[end-1].tick)
			series.Values = append(series.Values, sum/float64(end-i))
		}
		resp.Series = append(resp.Series, series)
	}
	return resp, nil
}

func (s *Server) Logs(_ context.Context, req *lgsv1.LogsRequest) (*lgsv1.LogsResponse, error) {
	g := s.peek(req.GetGameId())
	g.mu.Lock()
	defer g.mu.Unlock()
	tier := g.obs.GetLogs()
	resp := &lgsv1.LogsResponse{Tier: tier}
	if tier == "none" || tier == "" {
		return resp, nil
	}
	limit := int(req.GetLimit())
	if limit <= 0 || limit > 200 {
		limit = 80
	}
	for i := len(g.logs) - 1; i >= 0 && len(resp.Lines) < limit; i-- {
		line := g.logs[i]
		if tier == "sampled" && line.GetTick()%4 != 0 {
			continue // the one line you needed was in the other 75%
		}
		if matches(line, req.GetQuery()) {
			resp.Lines = append(resp.Lines, line)
		}
	}
	return resp, nil
}

func (s *Server) Traces(_ context.Context, req *lgsv1.TracesRequest) (*lgsv1.TracesResponse, error) {
	g := s.peek(req.GetGameId())
	g.mu.Lock()
	defer g.mu.Unlock()
	tier := g.obs.GetTraces()
	resp := &lgsv1.TracesResponse{Tier: tier}
	if tier == "none" || tier == "" {
		return resp, nil
	}
	limit := 2
	if tier == "full" {
		limit = 10
	}
	for i := len(g.traces) - 1; i >= 0 && len(resp.Traces) < limit; i-- {
		tr := g.traces[i]
		// Head sampling drops most errors on the floor, the same way it does at home.
		if tier == "sampled" && i%3 != 0 {
			continue
		}
		resp.Traces = append(resp.Traces, tr)
	}
	return resp, nil
}

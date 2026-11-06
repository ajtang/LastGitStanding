// Package gateway is the REST and SSE edge for the browser. It translates, it redacts
// and it turns away anyone asking too often, and that's all it should do. Game logic
// that shows up here is game logic in the wrong place.
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

type Deps struct {
	Sim     lgsv1.SimulationClient
	Infra   lgsv1.InfrastructureClient
	Econ    lgsv1.EconomyClient
	Obs     lgsv1.ObservabilityClient
	Events  lgsv1.EventProcessingClient
	Players lgsv1.PlayerStateClient
	// AllowedOrigins is checked exactly. "*" isn't accepted here on purpose.
	AllowedOrigins []string
	// ClientIPHeader names the header a trusted proxy puts the real client IP in, for
	// example CF-Connecting-IP. Leave it empty unless the gateway can't be reached
	// except through that proxy, because anyone else can send the header too.
	ClientIPHeader string
	// AdminPasscode unlocks leaderboard editing. Empty means the routes don't exist.
	AdminPasscode string
}

type Gateway struct {
	Deps
	marshal protojson.MarshalOptions
	limiter *limiter
}

func New(d Deps) http.Handler {
	g := &Gateway{Deps: d, marshal: protojson.MarshalOptions{EmitUnpopulated: true}, limiter: newLimiter(d.ClientIPHeader)}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /api/v1/games", g.createGame)
	mux.HandleFunc("GET /api/v1/leaderboard", g.leaderboard)
	mux.HandleFunc("GET /api/v1/games/{id}", g.getFrame)
	mux.HandleFunc("GET /api/v1/games/{id}/stream", g.stream)
	mux.HandleFunc("POST /api/v1/games/{id}/commits", g.commit)
	mux.HandleFunc("GET /api/v1/games/{id}/commits", g.listCommits)
	mux.HandleFunc("POST /api/v1/games/{id}/plan", g.plan)
	mux.HandleFunc("POST /api/v1/games/{id}/ship", g.command(func(b body) *lgsv1.Command {
		return &lgsv1.Command{Action: &lgsv1.Command_Ship{Ship: &lgsv1.ShipAction{Kind: b.Kind}}}
	}))
	mux.HandleFunc("POST /api/v1/games/{id}/speed", g.command(func(b body) *lgsv1.Command {
		return &lgsv1.Command{Action: &lgsv1.Command_Speed{Speed: &lgsv1.SpeedAction{Speed: b.Speed}}}
	}))
	mux.HandleFunc("POST /api/v1/games/{id}/hire", g.command(func(b body) *lgsv1.Command {
		return &lgsv1.Command{Action: &lgsv1.Command_Hire{Hire: &lgsv1.HireAction{Delta: b.Delta}}}
	}))
	mux.HandleFunc("POST /api/v1/games/{id}/debt", g.command(func(b body) *lgsv1.Command {
		return &lgsv1.Command{Action: &lgsv1.Command_PayDebt{PayDebt: &lgsv1.PayDebtAction{Hours: b.Hours}}}
	}))
	mux.HandleFunc("POST /api/v1/games/{id}/incidents/{incident}/diagnosis", g.incidentCommand(func(id string, b body) *lgsv1.Command {
		return &lgsv1.Command{Action: &lgsv1.Command_Diagnose{Diagnose: &lgsv1.DiagnoseAction{IncidentId: id, Cause: b.Cause}}}
	}))
	mux.HandleFunc("POST /api/v1/games/{id}/incidents/{incident}/postmortem", g.incidentCommand(func(id string, _ body) *lgsv1.Command {
		return &lgsv1.Command{Action: &lgsv1.Command_Postmortem{Postmortem: &lgsv1.PostmortemAction{IncidentId: id}}}
	}))
	mux.HandleFunc("GET /api/v1/games/{id}/metrics", g.metrics)
	mux.HandleFunc("GET /api/v1/games/{id}/logs", g.logs)
	mux.HandleFunc("GET /api/v1/games/{id}/traces", g.traces)
	mux.HandleFunc("GET /api/v1/games/{id}/timeline", g.timeline)
	if d.AdminPasscode != "" {
		mux.HandleFunc("POST /api/v1/admin/session", g.admin(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		mux.HandleFunc("PATCH /api/v1/admin/runs/{id}", g.admin(g.editRun))
		mux.HandleFunc("DELETE /api/v1/admin/runs/{id}", g.admin(g.deleteRun))
	}

	// CORS goes outermost so the browser can read a 429 instead of calling it a network error.
	return g.cors(g.limiter.wrap(mux))
}

// body is the union of the small JSON payloads the REST surface takes. Six one-field
// request types would have been more correct and much less pleasant.
type body struct {
	Player  string  `json:"player"`
	Seed    string  `json:"seed"`
	Speed   int32   `json:"speed"`
	Yaml    string  `json:"yaml"`
	Message string  `json:"message"`
	Kind    string  `json:"kind"`
	Cause   string  `json:"cause"`
	Delta   int32   `json:"delta"`
	Hours   float64 `json:"hours"`
	// A pointer so a request that leaves it out can't zero someone's score by accident.
	Score *float64 `json:"score"`
}

func decode(w http.ResponseWriter, r *http.Request) (body, bool) {
	var b body
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10) // a manifest, not a novel
	err := json.NewDecoder(r.Body).Decode(&b)
	if err != nil && err != io.EOF {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return b, false
	}
	return b, true
}

func (g *Gateway) createGame(w http.ResponseWriter, r *http.Request) {
	b, ok := decode(w, r)
	if !ok {
		return
	}
	var seed int64
	if b.Seed != "" {
		parsed, err := strconv.ParseInt(b.Seed, 10, 64)
		if err != nil {
			http.Error(w, "seed must be an integer", http.StatusBadRequest)
			return
		}
		seed = parsed
	}
	run, err := g.Players.CreateRun(r.Context(), &lgsv1.CreateRunRequest{Player: b.Player, Seed: seed})
	if err != nil {
		g.fail(w, err)
		return
	}
	// A new game starts paused. Nobody should be losing users while reading the HUD.
	resp, err := g.Sim.StartGame(r.Context(), &lgsv1.StartGameRequest{GameId: run.GetGameId(), Seed: run.GetSeed(), Speed: 0})
	if err != nil {
		g.fail(w, err)
		return
	}
	g.send(w, redact(resp.GetFrame()))
}

func (g *Gateway) getFrame(w http.ResponseWriter, r *http.Request) {
	f, err := g.Sim.GetFrame(r.Context(), &lgsv1.WatchRequest{GameId: r.PathValue("id")})
	if err != nil {
		g.fail(w, err)
		return
	}
	g.send(w, redact(f))
}

// stream is SSE rather than WebSocket because the browser only ever listens here;
// commands go over plain POSTs. It's one less protocol to terminate at the ingress.
func (g *Gateway) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	stream, err := g.Sim.Watch(r.Context(), &lgsv1.WatchRequest{GameId: r.PathValue("id")})
	if err != nil {
		g.fail(w, err)
		return
	}
	first, err := stream.Recv()
	if err != nil {
		g.fail(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	for f := first; ; {
		raw, err := g.marshal.Marshal(redact(f))
		if err != nil {
			slog.Error("marshal frame", "err", err)
			return
		}
		if _, err := fmt.Fprintf(w, "event: frame\ndata: %s\n\n", raw); err != nil {
			return
		}
		flusher.Flush()
		f, err = stream.Recv()
		if err != nil {
			return // the browser's EventSource reconnects by itself
		}
	}
}

func (g *Gateway) commit(w http.ResponseWriter, r *http.Request) {
	b, ok := decode(w, r)
	if !ok {
		return
	}
	g.submit(w, r, &lgsv1.Command{GameId: r.PathValue("id"), Action: &lgsv1.Command_Commit{Commit: &lgsv1.CommitAction{Yaml: b.Yaml, Message: b.Message}}})
}

func (g *Gateway) command(build func(body) *lgsv1.Command) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, ok := decode(w, r)
		if !ok {
			return
		}
		cmd := build(b)
		cmd.GameId = r.PathValue("id")
		g.submit(w, r, cmd)
	}
}

func (g *Gateway) incidentCommand(build func(string, body) *lgsv1.Command) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, ok := decode(w, r)
		if !ok {
			return
		}
		cmd := build(r.PathValue("incident"), b)
		cmd.GameId = r.PathValue("id")
		g.submit(w, r, cmd)
	}
}

func (g *Gateway) submit(w http.ResponseWriter, r *http.Request, cmd *lgsv1.Command) {
	res, err := g.Sim.Submit(r.Context(), cmd)
	if err != nil {
		g.fail(w, err)
		return
	}
	g.send(w, res)
}

func (g *Gateway) listCommits(w http.ResponseWriter, r *http.Request) {
	res, err := g.Infra.ListCommits(r.Context(), &lgsv1.GameRef{GameId: r.PathValue("id")})
	if err != nil {
		g.fail(w, err)
		return
	}
	g.send(w, res)
}

// plan composes two services and is the one place the gateway does that: the editor
// wants one answer to "is this valid, and what would it cost?" as you type.
func (g *Gateway) plan(w http.ResponseWriter, r *http.Request) {
	b, ok := decode(w, r)
	if !ok {
		return
	}
	plan, err := g.Infra.Plan(r.Context(), &lgsv1.PlanRequest{GameId: r.PathValue("id"), Yaml: b.Yaml})
	if err != nil {
		g.fail(w, err)
		return
	}
	var quote *lgsv1.QuoteResponse
	if plan.GetSpec() != nil && plan.GetValid() {
		quote, err = g.Econ.Quote(r.Context(), &lgsv1.QuoteRequest{Spec: plan.GetSpec()})
		if err != nil {
			g.fail(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"valid":%t,"errors":%s,"quote":%s}`, plan.GetValid(), mustJSON(plan.GetErrors()), g.jsonOrNull(quote))
}

func (g *Gateway) metrics(w http.ResponseWriter, r *http.Request) {
	window, _ := strconv.Atoi(r.URL.Query().Get("window"))
	res, err := g.Obs.Metrics(r.Context(), &lgsv1.MetricsRequest{GameId: r.PathValue("id"), Window: int32(window)})
	if err != nil {
		g.fail(w, err)
		return
	}
	g.send(w, res)
}

func (g *Gateway) logs(w http.ResponseWriter, r *http.Request) {
	res, err := g.Obs.Logs(r.Context(), &lgsv1.LogsRequest{GameId: r.PathValue("id"), Query: r.URL.Query().Get("q"), Limit: 80})
	if err != nil {
		g.fail(w, err)
		return
	}
	g.send(w, res)
}

func (g *Gateway) traces(w http.ResponseWriter, r *http.Request) {
	res, err := g.Obs.Traces(r.Context(), &lgsv1.TracesRequest{GameId: r.PathValue("id")})
	if err != nil {
		g.fail(w, err)
		return
	}
	g.send(w, res)
}

// timeline can lag or be briefly empty if Event Processing is behind. The UI treats
// that as normal, because it is.
func (g *Gateway) timeline(w http.ResponseWriter, r *http.Request) {
	res, err := g.Events.Timeline(r.Context(), &lgsv1.TimelineRequest{GameId: r.PathValue("id"), Limit: 150})
	if err != nil {
		g.fail(w, err)
		return
	}
	g.send(w, res)
}

func (g *Gateway) leaderboard(w http.ResponseWriter, r *http.Request) {
	res, err := g.Players.Leaderboard(r.Context(), &lgsv1.LeaderboardRequest{Limit: 20})
	if err != nil {
		g.fail(w, err)
		return
	}
	g.send(w, res)
}

// redact removes ground truth before anything leaves the building. Unnoticed
// incidents go too: if nobody has been paged, it's not on your screen yet.
func redact(f *lgsv1.Frame) *lgsv1.Frame {
	c := proto.Clone(f).(*lgsv1.Frame)
	c.Effects = nil
	visible := c.GetIncidents()[:0]
	for _, inc := range c.GetIncidents() {
		if inc.GetDetectedTick() != 0 {
			visible = append(visible, inc)
		}
	}
	c.Incidents = visible
	events := c.GetEvents()[:0]
	for _, ev := range c.GetEvents() {
		if ev.GetKind() != "incident_opened" { // the event version of the same spoiler
			events = append(events, ev)
		}
	}
	c.Events = events
	return c
}

func (g *Gateway) send(w http.ResponseWriter, m proto.Message) {
	raw, err := g.marshal.Marshal(m)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}

func (g *Gateway) jsonOrNull(m *lgsv1.QuoteResponse) string {
	if m == nil {
		return "null"
	}
	raw, err := g.marshal.Marshal(m)
	if err != nil {
		return "null"
	}
	return string(raw)
}

func mustJSON(v []string) string {
	if v == nil {
		v = []string{}
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func (g *Gateway) fail(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	switch status.Code(err) {
	case codes.NotFound:
		code = http.StatusNotFound
	case codes.InvalidArgument:
		code = http.StatusBadRequest
	case codes.ResourceExhausted:
		code = http.StatusTooManyRequests
	case codes.DeadlineExceeded, codes.Unavailable:
		code = http.StatusServiceUnavailable
	case codes.Canceled:
		return // client went away; nobody to tell
	}
	if code >= 500 {
		slog.Warn("upstream failure", "err", err)
	}
	http.Error(w, strings.TrimPrefix(status.Convert(err).Message(), "rpc error: "), code)
}

func (g *Gateway) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range g.AllowedOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Max-Age", strconv.Itoa(int((time.Hour).Seconds())))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

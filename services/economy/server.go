// Package economy is where architecture decisions turn into consequences. It's
// the only service allowed to tell the player no for money reasons, and it runs
// the funding rounds, which are the closest thing the game has to levels.
package economy

import (
	"context"
	"errors"
	"fmt"
	"math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/catalog"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
	"github.com/ajtang/LastGitStanding/internal/clock"
)

const (
	startBudget  = 28000.0
	startUsers   = 5000.0
	startEng     = 2
	arpuPerDay   = 0.05
	baseGrowth   = 0.075 // per day, when nothing is wrong and nothing is new
	hypeOnShip   = 0.05
	hypeDecay    = 0.985 // per tick: a launch is forgotten in about a week
	hoursPerDay  = 6.0   // of 8. The other two are meetings.
	hoursCapEach = 30.0
	firefighting = 0.4 // hours per tick per open incident that aren't going to features

	featureBaseHours = 14.0
	hotfixHours      = 5.0
	postmortemHours  = 4.0
	debtPerFeature   = 3.0
	debtPerHotfix    = 1.0
	debtPerHour      = 0.6
	postmortemCredit = 4.0

	bankruptGrace = 3 * clock.TicksPerDay
	sloWindow     = 3 * clock.TicksPerDay
	firedBelow    = 0.80
	minUsers      = 400
	roundMinTicks = clock.TicksPerDay // you can't close a round during the launch spike
)

type game struct {
	Budget      float64               `json:"budget"`
	Users       float64               `json:"users"`
	PeakUsers   float64               `json:"peakUsers"`
	Engineers   int32                 `json:"engineers"`
	Hours       float64               `json:"hours"`
	Debt        float64               `json:"debt"`
	Hype        float64               `json:"hype"`
	Stage       int                   `json:"stage"`
	HighestLive int                   `json:"highestLive"`
	Avail       []float64             `json:"avail"`
	InRed       int32                 `json:"inRed"`
	Score       float64               `json:"score"`
	OverReason  string                `json:"overReason"`
	Won         bool                  `json:"won"`
	LastSeen    *lgsv1.Ledger         `json:"lastSeen"`
	LastTick    int32                 `json:"lastTick"`
	LastResp    *lgsv1.SettleResponse `json:"lastResp"`
}

type Server struct {
	lgsv1.UnimplementedEconomyServer
	games *checkpoint.Keeper[game]
}

func NewServer(store checkpoint.Store) *Server {
	return &Server{games: checkpoint.NewKeeper[game](store, "economy")}
}

func (s *Server) Init(ctx context.Context, req *lgsv1.InitGame) (*lgsv1.InitEconomyResponse, error) {
	g := &game{Budget: startBudget, Users: startUsers, PeakUsers: startUsers, Engineers: startEng, Hours: 20}
	if err := s.games.Create(ctx, req.GetGameId(), g); err != nil {
		return nil, status.Errorf(codes.Unavailable, "checkpoint: %v", err)
	}
	return &lgsv1.InitEconomyResponse{Ledger: g.ledger(0, 0)}, nil
}

func (s *Server) Settle(ctx context.Context, req *lgsv1.SettleRequest) (*lgsv1.SettleResponse, error) {
	var resp *lgsv1.SettleResponse
	err := s.games.Update(ctx, req.GetGameId(), func(g *game) error {
		if req.GetTick() == g.LastTick && g.LastResp != nil {
			resp = g.LastResp
			return nil
		}
		resp = g.settle(req)
		g.LastTick, g.LastResp = req.GetTick(), resp
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return resp, nil
}

func (g *game) settle(req *lgsv1.SettleRequest) *lgsv1.SettleResponse {
	tick, h, topo := req.GetTick(), req.GetHealth(), req.GetTopology()
	var events []*lgsv1.Event
	emit := func(sev, kind, msg string) {
		events = append(events, &lgsv1.Event{Tick: tick, Source: "economy", Kind: kind, Severity: sev, Message: msg})
	}

	availability := 1 - h.GetErrorRate()
	g.Avail = append(g.Avail, availability)
	if len(g.Avail) > sloWindow {
		g.Avail = g.Avail[1:]
	}

	// ---- money

	cost := costOfTopology(topo, h.GetOfferedRps())
	eng := float64(g.Engineers) * catalog.EngineerDay
	costPerDay := cost + eng
	revenuePerDay := g.Users * arpuPerDay * availability
	g.Budget += (revenuePerDay - costPerDay) / clock.TicksPerDay
	if fine := req.GetEffects().GetFineUsd(); fine > 0 {
		g.Budget -= fine
	}

	// ---- users. Growth is earned slowly; churn is fast and doesn't send a survey.

	g.noticeShippedFeatures(topo)
	sat := satisfaction(availability, h.GetLatencyP95Ms())
	rate := (baseGrowth + g.Hype) * sat
	if sat < 0.5 {
		rate = -(0.5 - sat) * 0.9
	}
	g.Users *= 1 + rate/clock.TicksPerDay
	g.Users *= 1 - req.GetEffects().GetChurnShock()
	g.Hype *= hypeDecay
	g.PeakUsers = math.Max(g.PeakUsers, g.Users)

	// ---- people

	g.Hours += float64(g.Engineers) * hoursPerDay / clock.TicksPerDay
	g.Hours -= float64(req.GetOpenIncidents()) * firefighting
	g.Hours = math.Max(0, math.Min(g.Hours, float64(g.Engineers)*hoursCapEach))

	// ---- funding rounds

	g.checkRound(tick, emit)
	g.Score = g.PeakUsers/10 + g.rollingAvailability()*1000 + float64(g.Stage)*2500
	g.checkGameOver(emit)

	return &lgsv1.SettleResponse{Ledger: g.ledger(revenuePerDay, costPerDay), Events: events}
}

func (g *game) checkRound(tick int32, emit func(sev, kind, msg string)) {
	stage := catalog.Stages[g.Stage]
	if g.Users < stage.NextUsers || int32(len(g.Avail)) < roundMinTicks {
		return
	}
	// Investors do read the status page. Growth alone doesn't close the round.
	if g.rollingAvailability() < stage.SLO {
		return
	}
	if g.Stage == len(catalog.Stages)-1 {
		g.Won = true
		g.OverReason = "You rang the bell. The pager still works, but now it's someone's whole department."
		emit("page", "ipo", "IPO. You are the last git standing.")
		return
	}
	g.Budget += stage.Grant
	g.Stage++
	emit("page", "funding", fmt.Sprintf("Closed %s: $%s in the bank. SLO is now %.1f%%.",
		catalog.Stages[g.Stage].Name, money(stage.Grant), catalog.Stages[g.Stage].SLO*100))
}

func (g *game) checkGameOver(emit func(sev, kind, msg string)) {
	if g.Won {
		return
	}
	if g.Budget < 0 {
		g.InRed++
	} else {
		g.InRed = 0
	}
	switch {
	case g.InRed >= bankruptGrace:
		g.OverReason = "Out of runway. The last thing you deleted was the production cluster, for cost reasons."
	case g.Users < minUsers:
		g.OverReason = "Your last customer left. The status page is finally all green."
	case len(g.Avail) >= sloWindow && g.rollingAvailability() < firedBelow:
		g.OverReason = "Under 80% availability across three days. The board has asked for a change in engineering leadership. It's you."
	}
	if g.OverReason != "" {
		emit("page", "game_over", g.OverReason)
	}
}

// noticeShippedFeatures pays out hype when a new feature is actually running,
// not when it was merged. Code sitting in the registry has never impressed anyone.
func (g *game) noticeShippedFeatures(topo *lgsv1.Topology) {
	for _, r := range topo.GetRegions() {
		for _, p := range r.GetPods() {
			var major, minor, patch int
			if _, err := fmt.Sscanf(p.GetVersion(), "v%d.%d.%d", &major, &minor, &patch); err != nil {
				continue
			}
			if minor > g.HighestLive && p.GetPhase() == "ready" {
				g.Hype += hypeOnShip * float64(minor-g.HighestLive)
				g.HighestLive = minor
			}
		}
	}
}

// satisfaction is 1 when things are good. Users tolerate a slow site far more
// than a broken one, hence the different exponents. Below 0.5 they start leaving.
func satisfaction(availability, p95 float64) float64 {
	a := math.Pow(math.Max(availability, 0), 30)
	l := 1.0
	if p95 > 350 {
		l = math.Max(0.15, 1-(p95-350)/2000)
	}
	return a * l
}

func (g *game) rollingAvailability() float64 {
	if len(g.Avail) == 0 {
		return 1
	}
	sum := 0.0
	for _, a := range g.Avail {
		sum += a
	}
	return sum / float64(len(g.Avail))
}

func (g *game) featureCost() float64 {
	// Debt is interest on every future feature. That's not a metaphor here; it's
	// this line.
	return featureBaseHours * (1 + g.Debt/40)
}

func (s *Server) Spend(ctx context.Context, req *lgsv1.SpendRequest) (*lgsv1.SpendResponse, error) {
	resp := &lgsv1.SpendResponse{}
	err := s.games.Update(ctx, req.GetGameId(), func(g *game) error {
		var hours, debtDelta float64
		switch req.GetPurpose() {
		case "feature":
			hours, debtDelta = g.featureCost(), debtPerFeature
		case "hotfix":
			hours, debtDelta = hotfixHours, debtPerHotfix
		case "debt":
			hours = req.GetHours()
			if hours <= 0 {
				return status.Error(codes.InvalidArgument, "hours must be positive")
			}
			debtDelta = -hours * debtPerHour
		case "diagnosis":
			// Being wrong costs what it costs, even if that leaves you at zero.
			g.Hours = math.Max(0, g.Hours-req.GetHours())
			resp.Ok = true
			resp.Ledger = g.ledger(0, 0)
			return nil
		case "postmortem":
			hours, debtDelta = postmortemHours, -postmortemCredit
		case "refund":
			g.Hours += req.GetHours()
			g.Debt += postmortemCredit
			resp.Ok = true
			return nil
		default:
			return status.Errorf(codes.InvalidArgument, "unknown purpose %q", req.GetPurpose())
		}
		if g.Hours < hours {
			resp.Message = fmt.Sprintf("Needs %.0f engineer hours; you have %.0f. Hire, wait, or cut scope.", hours, g.Hours)
			return nil
		}
		g.Hours -= hours
		g.Debt = math.Max(0, g.Debt+debtDelta)
		resp.Ok = true
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return resp, nil
}

func (s *Server) Hire(ctx context.Context, req *lgsv1.HireRequest) (*lgsv1.SpendResponse, error) {
	resp := &lgsv1.SpendResponse{}
	err := s.games.Update(ctx, req.GetGameId(), func(g *game) error {
		next := g.Engineers + req.GetDelta()
		if next < 1 {
			resp.Message = "Someone has to hold the pager."
			return nil
		}
		if req.GetDelta() > 0 {
			fee := catalog.HiringFee * float64(req.GetDelta())
			if g.Budget < fee {
				resp.Message = "You can't afford the recruiter, let alone the engineer."
				return nil
			}
			g.Budget -= fee
		}
		g.Engineers = next
		resp.Ok = true
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return resp, nil
}

func (s *Server) Quote(_ context.Context, req *lgsv1.QuoteRequest) (*lgsv1.QuoteResponse, error) {
	return quoteSpec(req.GetSpec()), nil
}

func (g *game) ledger(revenue, cost float64) *lgsv1.Ledger {
	stage := catalog.Stages[g.Stage]
	rolling := g.rollingAvailability()
	allowed := 1 - stage.SLO
	remaining := 1.0
	if allowed > 0 {
		remaining = 1 - (1-rolling)/allowed
	}
	return &lgsv1.Ledger{
		Budget: g.Budget, RevenuePerDay: revenue, CostPerDay: cost, Users: g.Users,
		EngineerHours: g.Hours, Engineers: g.Engineers, TechDebt: g.Debt,
		SloTarget: stage.SLO, RollingAvailability: rolling, ErrorBudgetRemaining: remaining,
		Stage: stage.Name, NextStageUsers: stage.NextUsers, Satisfaction: satisfaction(rolling, 0),
		FeatureCostHours: g.featureCost(), Score: g.Score, GameOverReason: g.OverReason, Won: g.Won,
	}
}

func money(v float64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", v/1_000_000)
	case v >= 1000:
		return fmt.Sprintf("%.0fk", v/1000)
	}
	return fmt.Sprintf("%.0f", v)
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

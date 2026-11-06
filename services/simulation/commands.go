package simulation

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

// Submit is the single door for player actions. Going through the conductor means
// a command lands between ticks instead of in the middle of one, and it works
// while paused, because staring at a paused incident and thinking is a legitimate
// strategy that real life refuses to offer.
func (e *Engine) Submit(ctx context.Context, cmd *lgsv1.Command) (*lgsv1.CommandResult, error) {
	id := cmd.GetGameId()
	var tick int32
	if err := e.runs.View(ctx, id, func(r *runState) error { tick = r.Tick; return nil }); err != nil {
		return nil, grpcErr(err)
	}
	e.touch(id)

	switch a := cmd.GetAction().(type) {
	case *lgsv1.Command_Speed:
		speed := a.Speed.GetSpeed()
		if speed < 0 || speed > 8 {
			return nil, status.Error(codes.InvalidArgument, "speed must be between 0 and 8")
		}
		e.setSpeed(e.attach(id, 0), speed)
		return &lgsv1.CommandResult{Ok: true}, nil

	case *lgsv1.Command_Commit:
		resp, err := e.deps.Infra.Commit(ctx, &lgsv1.CommitRequest{GameId: id, Tick: tick, Yaml: a.Commit.GetYaml(), Message: a.Commit.GetMessage()})
		if err != nil {
			return nil, err
		}
		return &lgsv1.CommandResult{Ok: true, Ref: resp.GetSha(), Message: "Pushed. The pipeline has it."}, nil

	case *lgsv1.Command_Ship:
		kind := a.Ship.GetKind()
		if kind != "feature" && kind != "hotfix" {
			return nil, status.Errorf(codes.InvalidArgument, "can't ship a %q", kind)
		}
		spend, err := e.deps.Econ.Spend(ctx, &lgsv1.SpendRequest{GameId: id, Tick: tick, Purpose: kind})
		if err != nil {
			return nil, err
		}
		if !spend.GetOk() {
			return &lgsv1.CommandResult{Message: spend.GetMessage()}, nil
		}
		pub, err := e.deps.Infra.PublishVersion(ctx, &lgsv1.PublishVersionRequest{GameId: id, Tick: tick, Kind: kind})
		if err != nil {
			return nil, err
		}
		return &lgsv1.CommandResult{Ok: true, Ref: pub.GetVersion(), Message: pub.GetVersion() + " is in the registry. Deploying it takes a commit."}, nil

	case *lgsv1.Command_Diagnose:
		resp, err := e.deps.Incid.Diagnose(ctx, &lgsv1.DiagnoseRequest{GameId: id, Tick: tick, IncidentId: a.Diagnose.GetIncidentId(), Cause: a.Diagnose.GetCause()})
		if err != nil {
			return nil, err
		}
		if p := resp.GetHoursPenalty(); p > 0 {
			if _, err := e.deps.Econ.Spend(ctx, &lgsv1.SpendRequest{GameId: id, Tick: tick, Purpose: "diagnosis", Hours: p}); err != nil {
				return nil, err
			}
		}
		return &lgsv1.CommandResult{Ok: resp.GetCorrect(), Message: resp.GetMessage()}, nil

	case *lgsv1.Command_Postmortem:
		const hours = 4.0
		spend, err := e.deps.Econ.Spend(ctx, &lgsv1.SpendRequest{GameId: id, Tick: tick, Purpose: "postmortem"})
		if err != nil {
			return nil, err
		}
		if !spend.GetOk() {
			return &lgsv1.CommandResult{Message: spend.GetMessage()}, nil
		}
		pm, err := e.deps.Incid.Postmortem(ctx, &lgsv1.PostmortemRequest{GameId: id, Tick: tick, IncidentId: a.Postmortem.GetIncidentId()})
		if err == nil && pm.GetOk() {
			return &lgsv1.CommandResult{Ok: true, Message: pm.GetMessage()}, nil
		}
		// Two services, no transaction. Hours are the cheap side to compensate, so they
		// go first and come back if the incident says no.
		if _, rerr := e.deps.Econ.Spend(ctx, &lgsv1.SpendRequest{GameId: id, Tick: tick, Purpose: "refund", Hours: hours}); rerr != nil {
			return nil, fmt.Errorf("refunding postmortem hours: %w", rerr)
		}
		if err != nil {
			return nil, err
		}
		return &lgsv1.CommandResult{Message: pm.GetMessage()}, nil

	case *lgsv1.Command_PayDebt:
		spend, err := e.deps.Econ.Spend(ctx, &lgsv1.SpendRequest{GameId: id, Tick: tick, Purpose: "debt", Hours: a.PayDebt.GetHours()})
		if err != nil {
			return nil, err
		}
		if !spend.GetOk() {
			return &lgsv1.CommandResult{Message: spend.GetMessage()}, nil
		}
		return &lgsv1.CommandResult{Ok: true, Message: "Refactored. Nothing visibly changed, and everyone is slightly happier."}, nil

	case *lgsv1.Command_Hire:
		resp, err := e.deps.Econ.Hire(ctx, &lgsv1.HireRequest{GameId: id, Tick: tick, Delta: a.Hire.GetDelta()})
		if err != nil {
			return nil, err
		}
		return &lgsv1.CommandResult{Ok: resp.GetOk(), Message: resp.GetMessage()}, nil
	}
	return nil, status.Error(codes.InvalidArgument, "empty command")
}

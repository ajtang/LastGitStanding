package events

import (
	"context"
	"strings"
	"testing"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

func TestDetectionPointsAtTheMostRecentChange(t *testing.T) {
	ctx := context.Background()
	s := NewServer(NewMemoryStore())
	feed := func(tick int32, kind, msg string) {
		t.Helper()
		if err := s.Handle(ctx, "g", &lgsv1.Event{Tick: tick, Kind: kind, Message: msg, Ref: "INC-001"}); err != nil {
			t.Fatal(err)
		}
	}
	feed(10, "flux_applied", "Flux applied old")
	feed(50, "flux_applied", "Flux applied recent")
	feed(55, "incident_detected", "something hurts")

	resp, err := s.Timeline(ctx, &lgsv1.TimelineRequest{GameId: "g"})
	if err != nil {
		t.Fatal(err)
	}
	newest := resp.GetEvents()[0]
	if newest.GetKind() != "suspect_change" || !strings.Contains(newest.GetMessage(), "recent") {
		t.Fatalf("newest event = %s %q, want a suspect pointing at the recent change", newest.GetKind(), newest.GetMessage())
	}
}

func TestOldChangesAreNotBlamed(t *testing.T) {
	ctx := context.Background()
	s := NewServer(NewMemoryStore())
	s.Handle(ctx, "g", &lgsv1.Event{Tick: 1, Kind: "flux_applied", Message: "ancient"})
	s.Handle(ctx, "g", &lgsv1.Event{Tick: 1 + correlationWindow + 1, Kind: "incident_detected"})

	resp, _ := s.Timeline(ctx, &lgsv1.TimelineRequest{GameId: "g"})
	for _, ev := range resp.GetEvents() {
		if ev.GetKind() == "suspect_change" {
			t.Fatalf("blamed a change from outside the window: %q", ev.GetMessage())
		}
	}
}

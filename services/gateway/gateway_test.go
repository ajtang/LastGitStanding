package gateway

import (
	"testing"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

// If this breaks, the diagnosis mechanic is one devtools tab away from pointless.
func TestRedactRemovesGroundTruth(t *testing.T) {
	f := &lgsv1.Frame{
		Effects: &lgsv1.Effects{TlsExpired: true, HotQuery: "SELECT"},
		Incidents: []*lgsv1.IncidentView{
			{Id: "seen", DetectedTick: 3},
			{Id: "unseen", DetectedTick: 0},
		},
		Events: []*lgsv1.Event{
			{Kind: "incident_opened", Ref: "unseen"},
			{Kind: "merged"},
		},
	}
	got := redact(f)
	if got.GetEffects() != nil {
		t.Error("effects reached the browser")
	}
	if len(got.GetIncidents()) != 1 || got.GetIncidents()[0].GetId() != "seen" {
		t.Errorf("incidents = %v, want only the detected one", got.GetIncidents())
	}
	if len(got.GetEvents()) != 1 || got.GetEvents()[0].GetKind() != "merged" {
		t.Errorf("events = %v, want no spoilers", got.GetEvents())
	}
	if f.GetEffects() == nil || len(f.GetIncidents()) != 2 {
		t.Error("redact mutated the original frame, which other watchers share")
	}
}

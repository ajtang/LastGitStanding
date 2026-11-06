package observability

import (
	"context"
	"strings"
	"testing"
	"time"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

func frame(tick int32, obs *lgsv1.ObservabilitySpec, fx *lgsv1.Effects, h *lgsv1.Health) *lgsv1.Frame {
	return &lgsv1.Frame{
		GameId: "g", Tick: tick, Health: h, Effects: fx, Ledger: &lgsv1.Ledger{SloTarget: 0.99},
		Topology: &lgsv1.Topology{Observability: obs, Ingress: &lgsv1.IngressState{CertExpiresAtTick: 9999}},
	}
}

func TestYouOnlySeeWhatYouPaidFor(t *testing.T) {
	ctx := context.Background()
	hot := &lgsv1.Effects{HotQuery: "SELECT * FROM orders -- missing index: orders_customer_id"}

	cheap := NewServer()
	dear := NewServer()
	for tick := int32(1); tick <= 40; tick++ {
		h := &lgsv1.Health{App: &lgsv1.ComponentHealth{}, Database: &lgsv1.ComponentHealth{}, Ingress: &lgsv1.ComponentHealth{}, Cache: &lgsv1.ComponentHealth{}}
		cheap.Ingest(ctx, frame(tick, &lgsv1.ObservabilitySpec{Metrics: "basic", Logs: "none", Traces: "none", Alerting: "none"}, hot, h))
		dear.Ingest(ctx, frame(tick, &lgsv1.ObservabilitySpec{Metrics: "full", Logs: "full", Traces: "full", Alerting: "slo"}, hot, h))
	}

	if logs, _ := cheap.Logs(ctx, &lgsv1.LogsRequest{GameId: "g"}); len(logs.GetLines()) != 0 {
		t.Fatalf("no logging tier returned %d lines", len(logs.GetLines()))
	}
	logs, _ := dear.Logs(ctx, &lgsv1.LogsRequest{GameId: "g", Query: "missing index"})
	if len(logs.GetLines()) == 0 || !strings.Contains(logs.GetLines()[0].GetMessage(), "orders_customer_id") {
		t.Fatal("full logs should name the index the hotspot wants")
	}

	cm, _ := cheap.Metrics(ctx, &lgsv1.MetricsRequest{GameId: "g"})
	dm, _ := dear.Metrics(ctx, &lgsv1.MetricsRequest{GameId: "g"})
	if len(cm.GetSeries()) >= len(dm.GetSeries()) {
		t.Fatalf("basic has %d series, full has %d", len(cm.GetSeries()), len(dm.GetSeries()))
	}
	if len(cm.GetSeries()[0].GetValues()) >= len(dm.GetSeries()[0].GetValues()) {
		t.Fatal("basic metrics should be lower resolution")
	}

	if tr, _ := cheap.Traces(ctx, &lgsv1.TracesRequest{GameId: "g"}); len(tr.GetTraces()) != 0 {
		t.Fatal("no tracing tier returned traces")
	}
}

func TestSLOAlertingPagesBeforeThresholdAlerting(t *testing.T) {
	firstPage := func(alerting string) int32 {
		s := NewServer()
		obs := &lgsv1.ObservabilitySpec{Alerting: alerting}
		for tick := int32(1); tick < 10; tick++ {
			h := &lgsv1.Health{ErrorRate: 0.2}
			resp, _ := s.Ingest(context.Background(), frame(tick, obs, &lgsv1.Effects{}, h))
			if len(resp.GetFiring()) > 0 {
				return tick
			}
		}
		t.Fatalf("%s alerting never fired on 20%% errors", alerting)
		return 0
	}
	if slo, threshold := firstPage("slo"), firstPage("threshold"); slo >= threshold {
		t.Fatalf("slo fired at %d, threshold at %d", slo, threshold)
	}
}

func TestPartitionIsVisibleInTracesAndNotInPodCounts(t *testing.T) {
	fx := &lgsv1.Effects{PartitionLoss: 0.8}
	h := &lgsv1.Health{ErrorRate: 1, CacheHitRatio: 0, App: &lgsv1.ComponentHealth{}, Database: &lgsv1.ComponentHealth{}, Ingress: &lgsv1.ComponentHealth{}, Cache: &lgsv1.ComponentHealth{}}
	traces := generateTraces(&lgsv1.Frame{GameId: "g", Tick: 3, Health: h, Effects: fx})
	last := traces[0].GetSpans()[len(traces[0].GetSpans())-1]
	if last.GetService() != "postgres" || !last.GetError() {
		t.Fatalf("expected the failure on the postgres span, got %+v", last)
	}
}

// These read paths take a game ID straight from a URL.
func TestReadingAnUnknownGameDoesNotCreateIt(t *testing.T) {
	ctx := context.Background()
	s := NewServer()
	s.Metrics(ctx, &lgsv1.MetricsRequest{GameId: "made-up"})
	s.Logs(ctx, &lgsv1.LogsRequest{GameId: "made-up"})
	s.Traces(ctx, &lgsv1.TracesRequest{GameId: "made-up"})
	if len(s.games) != 0 {
		t.Fatalf("%d games appeared from reads", len(s.games))
	}
}

func TestTelemetryForAbandonedGamesIsForgotten(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(1_000_000, 0)
	s := NewServer()
	s.now = func() time.Time { return clock }
	spec := &lgsv1.ObservabilitySpec{Metrics: "full"}
	for _, id := range []string{"abandoned", "running"} {
		f := frame(1, spec, nil, &lgsv1.Health{})
		f.GameId = id
		if _, err := s.Ingest(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	clock = clock.Add(forgetAfter)
	f := frame(2, spec, nil, &lgsv1.Health{})
	f.GameId = "running"
	if _, err := s.Ingest(ctx, f); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.games["abandoned"]; ok {
		t.Fatal("abandoned game is still in memory")
	}
	if _, ok := s.games["running"]; !ok {
		t.Fatal("a running game was forgotten")
	}
}

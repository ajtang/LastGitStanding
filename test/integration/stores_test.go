// Package integration exercises the code that talks to real Redis and Postgres.
// Everything else in the repo tests against in-memory stores, so this is the only
// place a bad SQL statement or a Streams misunderstanding can get caught.
//
//	scripts/dev-deps.sh start          # or use your own, then:
//	REDIS_URL=... DATABASE_URL=... go test ./test/integration/
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/bus"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
	"github.com/ajtang/LastGitStanding/services/events"
	"github.com/ajtang/LastGitStanding/services/playerstate"
)

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opts)
	t.Cleanup(func() { c.Close() })
	return c
}

func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func unique(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }

func TestCheckpointSurvivesARestartThroughRedis(t *testing.T) {
	type state struct{ Tick int }
	ctx := context.Background()
	store := checkpoint.NewRedisStore(redisClient(t))
	id := unique("ckpt")

	before := checkpoint.NewKeeper[state](store, "itest")
	if err := before.Create(ctx, id, &state{Tick: 41}); err != nil {
		t.Fatal(err)
	}
	if err := before.Update(ctx, id, func(s *state) error { s.Tick++; return nil }); err != nil {
		t.Fatal(err)
	}

	after := checkpoint.NewKeeper[state](store, "itest") // a new process, as far as it can tell
	err := after.View(ctx, id, func(s *state) error {
		if s.Tick != 42 {
			t.Fatalf("tick = %d after reload, want 42", s.Tick)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEventsFlowFromStreamToPostgresTimeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := events.NewPostgresStore(ctx, pgPool(t))
	if err != nil {
		t.Fatal(err)
	}
	s := events.NewServer(store)
	stream := bus.NewRedis(redisClient(t), unique("consumer"))
	id := unique("game")

	go stream.Consume(ctx, s.Handle)

	if err := stream.Publish(ctx, id, []*lgsv1.Event{
		{Tick: 7, Source: "infrastructure", Kind: "flux_applied", Severity: "info", Message: "Flux applied abc", Ref: "abc"},
		{Tick: 9, Source: "incident", Kind: "incident_detected", Severity: "page", Message: "ow", Ref: "INC-001"},
	}); err != nil {
		t.Fatal(err)
	}

	// Asynchronous by design. Wait for the correlator's output, the last thing written.
	for ctx.Err() == nil {
		got, err := store.Recent(ctx, id, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range got {
			if ev.GetKind() == "suspect_change" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the event to reach Postgres and be correlated")
}

func TestTimelineIgnoresRedeliveredEvents(t *testing.T) {
	ctx := context.Background()
	store, err := events.NewPostgresStore(ctx, pgPool(t))
	if err != nil {
		t.Fatal(err)
	}
	id := unique("dupes")
	ev := &lgsv1.Event{Tick: 3, Source: "x", Kind: "merged", Severity: "info", Message: "same", Ref: "abc"}
	for range 3 { // at-least-once delivery means this will happen
		if err := store.Append(ctx, id, ev); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Recent(ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%d rows for one event", len(got))
	}
}

func TestLeaderboardOnlyShowsFinishedRuns(t *testing.T) {
	ctx := context.Background()
	store, err := playerstate.NewPostgresStore(ctx, pgPool(t))
	if err != nil {
		t.Fatal(err)
	}
	s := playerstate.NewServer(store)

	done, err := s.CreateRun(ctx, &lgsv1.CreateRunRequest{Player: "finisher", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	unfinished, err := s.CreateRun(ctx, &lgsv1.CreateRunRequest{Player: "still going", Seed: 2})
	if err != nil {
		t.Fatal(err)
	}
	// A score nobody could reach by accident puts it at the top, whatever else is there.
	if _, err := s.FinishRun(ctx, &lgsv1.FinishRunRequest{GameId: done.GetGameId(), Tick: 500, Score: 1e12, PeakUsers: 9000, Outcome: "fired"}); err != nil {
		t.Fatal(err)
	}
	board, err := s.Leaderboard(ctx, &lgsv1.LeaderboardRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if board.GetRuns()[0].GetGameId() != done.GetGameId() {
		t.Fatalf("top run = %s", board.GetRuns()[0].GetPlayer())
	}
	for _, r := range board.GetRuns() {
		if r.GetGameId() == unfinished.GetGameId() {
			t.Fatal("an unfinished run reached the leaderboard")
		}
	}
	// Clean up the absurd score so it doesn't top the wall of anyone's dev database.
	if _, err := pgPool(t).Exec(ctx, `DELETE FROM runs WHERE game_id = ANY($1)`, []string{done.GetGameId(), unfinished.GetGameId()}); err != nil {
		t.Fatal(err)
	}
}

func TestEditingAndDeletingARunOnlyTouchesFinishedRuns(t *testing.T) {
	ctx := context.Background()
	store, err := playerstate.NewPostgresStore(ctx, pgPool(t))
	if err != nil {
		t.Fatal(err)
	}
	s := playerstate.NewServer(store)

	done, err := s.CreateRun(ctx, &lgsv1.CreateRunRequest{Player: "before", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.CreateRun(ctx, &lgsv1.CreateRunRequest{Player: "still going", Seed: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pgPool(t).Exec(ctx, `DELETE FROM runs WHERE game_id = ANY($1)`, []string{done.GetGameId(), live.GetGameId()})
	})
	if _, err := s.FinishRun(ctx, &lgsv1.FinishRunRequest{GameId: done.GetGameId(), Tick: 500, Score: 10, Outcome: "fired"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.EditRun(ctx, &lgsv1.EditRunRequest{GameId: done.GetGameId(), Player: "after", Score: 99})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetPlayer() != "after" || got.GetScore() != 99 || got.GetTicks() != 500 {
		t.Fatalf("edited run = %v", got)
	}
	if _, err := s.EditRun(ctx, &lgsv1.EditRunRequest{GameId: live.GetGameId(), Player: "x", Score: 1}); status.Code(err) != codes.NotFound {
		t.Fatalf("editing a live run: %v", err)
	}
	if _, err := s.DeleteRun(ctx, &lgsv1.DeleteRunRequest{GameId: live.GetGameId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("deleting a live run: %v", err)
	}
	if _, err := s.DeleteRun(ctx, &lgsv1.DeleteRunRequest{GameId: done.GetGameId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteRun(ctx, &lgsv1.DeleteRunRequest{GameId: done.GetGameId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("deleting it twice: %v", err)
	}
}

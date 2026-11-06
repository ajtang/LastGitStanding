package playerstate

import (
	"context"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

// The cut falls in the middle of the last emoji. Half a rune can't be marshalled as a
// proto string, so before the fix this name failed the whole request.
func TestLongNamesAreCutOnACharacterBoundary(t *testing.T) {
	s := NewServer(NewMemoryStore())
	run, err := s.CreateRun(context.Background(), &lgsv1.CreateRunRequest{Player: strings.Repeat("a", 30) + "🔥🔥"})
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(run.GetPlayer()) || run.GetPlayer() != strings.Repeat("a", 30) {
		t.Fatalf("got %q", run.GetPlayer())
	}
}

func finishedRun(t *testing.T, s *Server, player string, score float64) string {
	t.Helper()
	ctx := context.Background()
	run, err := s.CreateRun(ctx, &lgsv1.CreateRunRequest{Player: player})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, &lgsv1.FinishRunRequest{GameId: run.GetGameId(), Tick: 96, Score: score, Outcome: "fired"}); err != nil {
		t.Fatal(err)
	}
	return run.GetGameId()
}

func TestAnEditedScoreReordersTheWallAndKeepsTheRestOfTheRun(t *testing.T) {
	ctx := context.Background()
	s := NewServer(NewMemoryStore())
	finishedRun(t, s, "first", 100)
	second := finishedRun(t, s, "second", 50)

	if _, err := s.EditRun(ctx, &lgsv1.EditRunRequest{GameId: second, Player: "  renamed  ", Score: 200}); err != nil {
		t.Fatal(err)
	}
	board, err := s.Leaderboard(ctx, &lgsv1.LeaderboardRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	top := board.GetRuns()[0]
	if top.GetPlayer() != "renamed" || top.GetScore() != 200 || top.GetTicks() != 96 {
		t.Fatalf("top = %v", top)
	}
}

func TestEditsThatWouldBreakTheWallAreRefused(t *testing.T) {
	ctx := context.Background()
	s := NewServer(NewMemoryStore())
	id := finishedRun(t, s, "someone", 100)
	for name, req := range map[string]*lgsv1.EditRunRequest{
		"blank name":     {GameId: id, Player: "   ", Score: 1},
		"negative score": {GameId: id, Player: "x", Score: -1},
		"NaN score":      {GameId: id, Player: "x", Score: math.NaN()},
		"infinite score": {GameId: id, Player: "x", Score: math.Inf(1)},
	} {
		if _, err := s.EditRun(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A live game still has to write its result, which would undo an edit and fail on a
// deleted row, so runs are out of reach until they've finished.
func TestUnfinishedRunsCannotBeEditedOrDeleted(t *testing.T) {
	ctx := context.Background()
	s := NewServer(NewMemoryStore())
	live, err := s.CreateRun(ctx, &lgsv1.CreateRunRequest{Player: "playing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditRun(ctx, &lgsv1.EditRunRequest{GameId: live.GetGameId(), Player: "x", Score: 1}); status.Code(err) != codes.NotFound {
		t.Errorf("edit: %v", err)
	}
	if _, err := s.DeleteRun(ctx, &lgsv1.DeleteRunRequest{GameId: live.GetGameId()}); status.Code(err) != codes.NotFound {
		t.Errorf("delete: %v", err)
	}
	if _, err := s.FinishRun(ctx, &lgsv1.FinishRunRequest{GameId: live.GetGameId(), Score: 5, Outcome: "fired"}); err != nil {
		t.Errorf("the game could no longer finish: %v", err)
	}
}

func TestADeletedRunLeavesTheWall(t *testing.T) {
	ctx := context.Background()
	s := NewServer(NewMemoryStore())
	id := finishedRun(t, s, "someone", 100)
	if _, err := s.DeleteRun(ctx, &lgsv1.DeleteRunRequest{GameId: id}); err != nil {
		t.Fatal(err)
	}
	board, err := s.Leaderboard(ctx, &lgsv1.LeaderboardRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(board.GetRuns()) != 0 {
		t.Fatalf("still on the wall: %v", board.GetRuns())
	}
}

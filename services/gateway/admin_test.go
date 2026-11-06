package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

const testPasscode = "not-the-real-one"

type fakePlayers struct {
	lgsv1.PlayerStateClient
	edits, deletes int
}

func (f *fakePlayers) EditRun(_ context.Context, req *lgsv1.EditRunRequest, _ ...grpc.CallOption) (*lgsv1.Run, error) {
	f.edits++
	return &lgsv1.Run{GameId: req.GetGameId(), Player: req.GetPlayer(), Score: req.GetScore()}, nil
}

func (f *fakePlayers) DeleteRun(context.Context, *lgsv1.DeleteRunRequest, ...grpc.CallOption) (*lgsv1.DeleteRunResponse, error) {
	f.deletes++
	return &lgsv1.DeleteRunResponse{}, nil
}

func adminDo(h http.Handler, method, path, passcode, body string) int {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "198.51.100.1:1000"
	if passcode != "" {
		req.Header.Set("Authorization", "Bearer "+passcode)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// Anyone can read the UI source and call these directly, so nothing may get through
// on the strength of knowing the URL.
func TestLeaderboardCannotBeChangedWithoutThePasscode(t *testing.T) {
	players := &fakePlayers{}
	h := New(Deps{Players: players, AdminPasscode: testPasscode})

	for _, passcode := range []string{"", "wrong"} {
		if code := adminDo(h, http.MethodPatch, "/api/v1/admin/runs/g1", passcode, `{"player":"x","score":1}`); code != http.StatusUnauthorized {
			t.Errorf("edit with %q: got %d", passcode, code)
		}
		if code := adminDo(h, http.MethodDelete, "/api/v1/admin/runs/g1", passcode, ""); code != http.StatusUnauthorized {
			t.Errorf("delete with %q: got %d", passcode, code)
		}
	}
	if players.edits != 0 || players.deletes != 0 {
		t.Fatalf("reached the store: %d edits, %d deletes", players.edits, players.deletes)
	}

	if code := adminDo(h, http.MethodPatch, "/api/v1/admin/runs/g1", testPasscode, `{"player":"x","score":1}`); code != http.StatusOK {
		t.Errorf("edit with the passcode: got %d", code)
	}
	if code := adminDo(h, http.MethodDelete, "/api/v1/admin/runs/g1", testPasscode, ""); code != http.StatusNoContent {
		t.Errorf("delete with the passcode: got %d", code)
	}
}

func TestAdminRoutesDoNotExistWithoutAConfiguredPasscode(t *testing.T) {
	h := New(Deps{Players: &fakePlayers{}})
	// An empty header must not match an empty passcode.
	for _, passcode := range []string{"", "anything"} {
		if code := adminDo(h, http.MethodPost, "/api/v1/admin/session", passcode, ""); code != http.StatusNotFound {
			t.Errorf("with %q: got %d", passcode, code)
		}
	}
}

func TestWrongPasscodesRunOutAndThenEvenTheRightOneIsRefused(t *testing.T) {
	h := New(Deps{Players: &fakePlayers{}, AdminPasscode: testPasscode})
	for i := range wrongPasscodeBurst {
		if code := adminDo(h, http.MethodPost, "/api/v1/admin/session", "wrong", ""); code != http.StatusUnauthorized {
			t.Fatalf("guess %d: got %d", i, code)
		}
	}
	if code := adminDo(h, http.MethodPost, "/api/v1/admin/session", "wrong", ""); code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: got %d", code)
	}
	if code := adminDo(h, http.MethodPost, "/api/v1/admin/session", testPasscode, ""); code != http.StatusTooManyRequests {
		t.Fatalf("right passcode while locked out: got %d, which tells a script it found it", code)
	}
}

// Fixing a whole leaderboard shouldn't lock the admin out.
func TestCorrectPasscodesDoNotSpendTheGuessAllowance(t *testing.T) {
	h := New(Deps{Players: &fakePlayers{}, AdminPasscode: testPasscode})
	for i := range wrongPasscodeBurst * 3 {
		if code := adminDo(h, http.MethodPost, "/api/v1/admin/session", testPasscode, ""); code != http.StatusNoContent {
			t.Fatalf("request %d: got %d", i, code)
		}
	}
}

func TestEditWithoutAScoreIsRefused(t *testing.T) {
	players := &fakePlayers{}
	h := New(Deps{Players: players, AdminPasscode: testPasscode})
	if code := adminDo(h, http.MethodPatch, "/api/v1/admin/runs/g1", testPasscode, `{"player":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("got %d", code)
	}
	if players.edits != 0 {
		t.Fatal("a missing score reached the store and would have zeroed it")
	}
}

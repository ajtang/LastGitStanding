package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

// admin guards a handler with the passcode. The button in the UI is hidden, but the
// source is public, so this check is the only thing protecting the leaderboard.
func (g *Gateway) admin(next http.HandlerFunc) http.HandlerFunc {
	// Hashing first makes both sides the same length, so the comparison time says
	// nothing about how long the passcode is.
	want := sha256.Sum256([]byte(g.AdminPasscode))
	return func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		correct := subtle.ConstantTimeCompare(sum[:], want[:]) == 1
		if !g.limiter.guess(r, correct) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many wrong passcodes. Try again in a minute.", http.StatusTooManyRequests)
			return
		}
		if !correct {
			slog.Warn("wrong admin passcode", "client", g.limiter.clientIP(r))
			http.Error(w, "Wrong passcode.", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (g *Gateway) editRun(w http.ResponseWriter, r *http.Request) {
	b, ok := decode(w, r)
	if !ok {
		return
	}
	if b.Score == nil {
		http.Error(w, "score is required", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	run, err := g.Players.EditRun(r.Context(), &lgsv1.EditRunRequest{GameId: id, Player: b.Player, Score: *b.Score})
	if err != nil {
		g.fail(w, err)
		return
	}
	slog.Info("leaderboard run edited", "game_id", id, "player", run.GetPlayer(), "score", run.GetScore())
	g.send(w, run)
}

func (g *Gateway) deleteRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := g.Players.DeleteRun(r.Context(), &lgsv1.DeleteRunRequest{GameId: id}); err != nil {
		g.fail(w, err)
		return
	}
	slog.Info("leaderboard run deleted", "game_id", id)
	w.WriteHeader(http.StatusNoContent)
}

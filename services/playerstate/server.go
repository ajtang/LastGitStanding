// Package playerstate holds the only data that outlives a run: who played, how
// it ended, and whose name goes on the wall. Live game state is deliberately not
// here. It changes every tick and is worthless a week later, so it's in Redis.
package playerstate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

type Store interface {
	Insert(ctx context.Context, run *lgsv1.Run) error
	Finish(ctx context.Context, req *lgsv1.FinishRunRequest) (*lgsv1.Run, error)
	Top(ctx context.Context, limit int) ([]*lgsv1.Run, error)
	Edit(ctx context.Context, req *lgsv1.EditRunRequest) (*lgsv1.Run, error)
	Delete(ctx context.Context, gameID string) error
}

// ErrNoRun means no finished run has that ID. Edit and Delete leave unfinished runs
// alone: the game's own FinishRun would overwrite an edit, and would fail on a deleted row.
var ErrNoRun = errors.New("no finished run with that id")

type Server struct {
	lgsv1.UnimplementedPlayerStateServer
	store Store
}

func NewServer(store Store) *Server { return &Server{store: store} }

func (s *Server) CreateRun(ctx context.Context, req *lgsv1.CreateRunRequest) (*lgsv1.Run, error) {
	player := cleanName(req.GetPlayer())
	if player == "" {
		player = "on-call"
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, status.Errorf(codes.Internal, "entropy: %v", err)
	}
	seed := req.GetSeed()
	if seed == 0 {
		for _, b := range buf[:6] {
			seed = seed<<8 | int64(b)
		}
	}
	run := &lgsv1.Run{GameId: hex.EncodeToString(buf[:]), Player: player, Seed: seed}
	if err := s.store.Insert(ctx, run); err != nil {
		return nil, status.Errorf(codes.Unavailable, "insert run: %v", err)
	}
	return run, nil
}

func (s *Server) FinishRun(ctx context.Context, req *lgsv1.FinishRunRequest) (*lgsv1.Run, error) {
	run, err := s.store.Finish(ctx, req)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "finish run: %v", err)
	}
	return run, nil
}

func (s *Server) Leaderboard(ctx context.Context, req *lgsv1.LeaderboardRequest) (*lgsv1.LeaderboardResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	runs, err := s.store.Top(ctx, limit)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "leaderboard: %v", err)
	}
	return &lgsv1.LeaderboardResponse{Runs: runs}, nil
}

func (s *Server) EditRun(ctx context.Context, req *lgsv1.EditRunRequest) (*lgsv1.Run, error) {
	player := cleanName(req.GetPlayer())
	if player == "" {
		return nil, status.Error(codes.InvalidArgument, "the name can't be empty")
	}
	if score := req.GetScore(); score < 0 || math.IsNaN(score) || math.IsInf(score, 0) {
		return nil, status.Error(codes.InvalidArgument, "the score must be a positive number")
	}
	run, err := s.store.Edit(ctx, &lgsv1.EditRunRequest{GameId: req.GetGameId(), Player: player, Score: req.GetScore()})
	if errors.Is(err, ErrNoRun) {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "edit run: %v", err)
	}
	return run, nil
}

func (s *Server) DeleteRun(ctx context.Context, req *lgsv1.DeleteRunRequest) (*lgsv1.DeleteRunResponse, error) {
	err := s.store.Delete(ctx, req.GetGameId())
	if errors.Is(err, ErrNoRun) {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "delete run: %v", err)
	}
	return &lgsv1.DeleteRunResponse{}, nil
}

func cleanName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > 32 {
		// Bytes, not characters, so an emoji can land on the cut. Half a rune isn't a
		// valid proto string, and the whole request would fail over a name.
		name = strings.ToValidUTF8(name[:32], "")
	}
	return name
}

type MemoryStore struct {
	mu   sync.Mutex
	runs map[string]*lgsv1.Run
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{runs: map[string]*lgsv1.Run{}} }

func (m *MemoryStore) Insert(_ context.Context, run *lgsv1.Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[run.GetGameId()] = run
	return nil
}

func (m *MemoryStore) Finish(_ context.Context, req *lgsv1.FinishRunRequest) (*lgsv1.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.runs[req.GetGameId()]
	if run == nil {
		return nil, status.Error(codes.NotFound, "no such run")
	}
	run.Ticks, run.Score, run.PeakUsers, run.Outcome, run.Won = req.GetTick(), req.GetScore(), req.GetPeakUsers(), req.GetOutcome(), req.GetWon()
	return run, nil
}

func (m *MemoryStore) Top(_ context.Context, limit int) ([]*lgsv1.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*lgsv1.Run
	for _, r := range m.runs {
		if r.GetOutcome() != "" {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b *lgsv1.Run) int {
		switch {
		case a.GetScore() > b.GetScore():
			return -1
		case a.GetScore() < b.GetScore():
			return 1
		}
		return strings.Compare(a.GetGameId(), b.GetGameId())
	})
	return out[:min(limit, len(out))], nil
}

// Edit swaps in a copy because Top hands out the stored pointers, and gRPC may still be
// marshalling one of them after the lock is released.
func (m *MemoryStore) Edit(_ context.Context, req *lgsv1.EditRunRequest) (*lgsv1.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.runs[req.GetGameId()]
	if old == nil || old.GetOutcome() == "" {
		return nil, ErrNoRun
	}
	run := proto.Clone(old).(*lgsv1.Run)
	run.Player, run.Score = req.GetPlayer(), req.GetScore()
	m.runs[req.GetGameId()] = run
	return run, nil
}

func (m *MemoryStore) Delete(_ context.Context, gameID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.runs[gameID]
	if run == nil || run.GetOutcome() == "" {
		return ErrNoRun
	}
	delete(m.runs, gameID)
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  game_id    TEXT PRIMARY KEY,
  player     TEXT NOT NULL,
  seed       BIGINT NOT NULL,
  ticks      INT NOT NULL DEFAULT 0,
  score      DOUBLE PRECISION NOT NULL DEFAULT 0,
  peak_users DOUBLE PRECISION NOT NULL DEFAULT 0,
  outcome    TEXT NOT NULL DEFAULT '',
  won        BOOLEAN NOT NULL DEFAULT FALSE,
  started_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS runs_leaderboard ON runs (score DESC) WHERE outcome <> '';`

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(ctx context.Context, pool *pgxpool.Pool) (*PostgresStore, error) {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, err
	}
	return &PostgresStore{pool: pool}, nil
}

func (p *PostgresStore) Insert(ctx context.Context, run *lgsv1.Run) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO runs (game_id, player, seed) VALUES ($1, $2, $3)`,
		run.GetGameId(), run.GetPlayer(), run.GetSeed())
	return err
}

func (p *PostgresStore) Finish(ctx context.Context, req *lgsv1.FinishRunRequest) (*lgsv1.Run, error) {
	run := &lgsv1.Run{}
	err := p.pool.QueryRow(ctx, `
		UPDATE runs SET ticks = $2, score = $3, peak_users = $4, outcome = $5, won = $6
		WHERE game_id = $1
		RETURNING game_id, player, seed, ticks, score, peak_users, outcome, won`,
		req.GetGameId(), req.GetTick(), req.GetScore(), req.GetPeakUsers(), req.GetOutcome(), req.GetWon(),
	).Scan(&run.GameId, &run.Player, &run.Seed, &run.Ticks, &run.Score, &run.PeakUsers, &run.Outcome, &run.Won)
	return run, err
}

func (p *PostgresStore) Top(ctx context.Context, limit int) ([]*lgsv1.Run, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT game_id, player, seed, ticks, score, peak_users, outcome, won FROM runs
		WHERE outcome <> '' ORDER BY score DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*lgsv1.Run
	for rows.Next() {
		run := &lgsv1.Run{}
		if err := rows.Scan(&run.GameId, &run.Player, &run.Seed, &run.Ticks, &run.Score, &run.PeakUsers, &run.Outcome, &run.Won); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (p *PostgresStore) Edit(ctx context.Context, req *lgsv1.EditRunRequest) (*lgsv1.Run, error) {
	run := &lgsv1.Run{}
	err := p.pool.QueryRow(ctx, `
		UPDATE runs SET player = $2, score = $3
		WHERE game_id = $1 AND outcome <> ''
		RETURNING game_id, player, seed, ticks, score, peak_users, outcome, won`,
		req.GetGameId(), req.GetPlayer(), req.GetScore(),
	).Scan(&run.GameId, &run.Player, &run.Seed, &run.Ticks, &run.Score, &run.PeakUsers, &run.Outcome, &run.Won)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoRun
	}
	return run, err
}

func (p *PostgresStore) Delete(ctx context.Context, gameID string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM runs WHERE game_id = $1 AND outcome <> ''`, gameID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoRun
	}
	return nil
}

package events

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

// One table, so no migration framework yet. The day there are two migrations,
// that decision gets revisited.
const schema = `
CREATE TABLE IF NOT EXISTS timeline (
  id       BIGSERIAL PRIMARY KEY,
  game_id  TEXT NOT NULL,
  tick     INT  NOT NULL,
  source   TEXT NOT NULL,
  kind     TEXT NOT NULL,
  severity TEXT NOT NULL,
  message  TEXT NOT NULL,
  ref      TEXT NOT NULL DEFAULT '',
  UNIQUE (game_id, tick, kind, ref, message)
);
CREATE INDEX IF NOT EXISTS timeline_game_recent ON timeline (game_id, id DESC);`

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(ctx context.Context, pool *pgxpool.Pool) (*PostgresStore, error) {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, err
	}
	return &PostgresStore{pool: pool}, nil
}

func (p *PostgresStore) Append(ctx context.Context, gameID string, ev *lgsv1.Event) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO timeline (game_id, tick, source, kind, severity, message, ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING`,
		gameID, ev.GetTick(), ev.GetSource(), ev.GetKind(), ev.GetSeverity(), ev.GetMessage(), ev.GetRef())
	return err
}

func (p *PostgresStore) Recent(ctx context.Context, gameID string, limit int) ([]*lgsv1.Event, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT tick, source, kind, severity, message, ref FROM timeline
		WHERE game_id = $1 ORDER BY id DESC LIMIT $2`, gameID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*lgsv1.Event
	for rows.Next() {
		ev := &lgsv1.Event{}
		if err := rows.Scan(&ev.Tick, &ev.Source, &ev.Kind, &ev.Severity, &ev.Message, &ev.Ref); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Package bus carries game events from the tick loop to whoever cares later.
//
// Delivery is at-least-once and consumers must not assume they're fast. If you
// find yourself wanting a response from this, you wanted an RPC.
package bus

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protojson"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
)

type Handler func(ctx context.Context, gameID string, ev *lgsv1.Event) error

type Bus interface {
	Publish(ctx context.Context, gameID string, events []*lgsv1.Event) error
	Consume(ctx context.Context, h Handler) error // blocks until ctx is done
}

// Memory is for tests and `lgs dev` without Redis. It is an unbounded slice and a
// condition variable, which is all a message queue is until it has customers.
type Memory struct {
	mu    sync.Mutex
	cond  *sync.Cond
	queue []entry
}

type entry struct {
	game string
	ev   *lgsv1.Event
}

func NewMemory() *Memory {
	m := &Memory{}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *Memory) Publish(_ context.Context, gameID string, events []*lgsv1.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ev := range events {
		m.queue = append(m.queue, entry{gameID, ev})
	}
	m.cond.Broadcast()
	return nil
}

func (m *Memory) Consume(ctx context.Context, h Handler) error {
	stop := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.cond.Broadcast()
	})
	defer stop()
	for {
		m.mu.Lock()
		for len(m.queue) == 0 && ctx.Err() == nil {
			m.cond.Wait()
		}
		if ctx.Err() != nil {
			m.mu.Unlock()
			return ctx.Err()
		}
		batch := m.queue
		m.queue = nil
		m.mu.Unlock()
		for _, e := range batch {
			if err := h(ctx, e.game, e.ev); err != nil {
				slog.Warn("event handler failed; dropping", "err", err)
			}
		}
	}
}

const (
	streamKey = "lgs:events"
	groupName = "event-processing"
	// Capped so a dead consumer costs us history rather than a Redis OOM, which
	// would take the tick loop's checkpoints down with it.
	streamMaxLen = 200_000
)

type Redis struct {
	client   *redis.Client
	consumer string
}

func NewRedis(client *redis.Client, consumer string) *Redis {
	return &Redis{client: client, consumer: consumer}
}

func (r *Redis) Publish(ctx context.Context, gameID string, events []*lgsv1.Event) error {
	if len(events) == 0 {
		return nil
	}
	pipe := r.client.Pipeline()
	for _, ev := range events {
		raw, err := protojson.Marshal(ev)
		if err != nil {
			return err
		}
		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: streamKey, MaxLen: streamMaxLen, Approx: true,
			Values: map[string]any{"game": gameID, "event": raw},
		})
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (r *Redis) Consume(ctx context.Context, h Handler) error {
	err := r.client.XGroupCreateMkStream(ctx, streamKey, groupName, "0").Err()
	if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		return err
	}
	for {
		res, err := r.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: groupName, Consumer: r.consumer, Streams: []string{streamKey, ">"},
			Count: 200, Block: 2 * time.Second,
		}).Result()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			slog.Warn("reading event stream", "err", err)
			time.Sleep(time.Second)
			continue
		}
		for _, stream := range res {
			for _, msg := range stream.Messages {
				game, _ := msg.Values["game"].(string)
				raw, _ := msg.Values["event"].(string)
				ev := &lgsv1.Event{}
				if err := protojson.Unmarshal([]byte(raw), ev); err != nil {
					slog.Warn("undecodable event; acking so it can't wedge the group", "id", msg.ID, "err", err)
				} else if err := h(ctx, game, ev); err != nil {
					// Left unacked. Reclaiming the pending list is on the roadmap, along
					// with the rest of the things that only matter at real scale.
					slog.Warn("event handler failed", "id", msg.ID, "err", err)
					continue
				}
				r.client.XAck(ctx, streamKey, groupName, msg.ID)
			}
		}
	}
}

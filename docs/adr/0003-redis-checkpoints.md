# ADR-0003: Per-game state is JSON in Redis, written through on every tick

**Status:** accepted

## Why this exists

Games should survive a pod restart. State changes every tick and is worthless once the run
ends, and it's only ever read back whole by the service that wrote it.

## Decision

Each service keeps state in memory and writes the whole thing to Redis on every mutation,
reloading on a cache miss. Encoding is JSON. There are no partial updates and no queries.

Observability is the exception: its history is memory-only. Losing telemetry on restart is
how cheap telemetry behaves, and a tick's worth of history is the largest state we have.

## Tradeoffs

- **Redis is a hard dependency of the tick loop.** Given what the game teaches about Redis,
  this is funny, and it's also a real risk. Hence a Standard-tier instance with a replica.
- **Services are singletons.** A second replica would hold a stale in-memory copy. Fixing
  that takes either reading through on every call or sticky routing per game. Neither is
  worth doing until one simulation pod can't hold the load.
- **Writes are O(state size).** Infrastructure's state grows with commit history. In a
  90-day run that's still small, and it can be compacted if it ever isn't.
- **JSON over generated proto structs relies on their `json` tags.** It works for these
  messages and would break on a `oneof`, so nothing checkpointed may contain one.

## Rejected

- **Postgres for live state.** A durable, indexed write per tick per service, for data
  we throw away. Postgres keeps what outlives a run.
- **Event sourcing the commands and replaying.** Determinism makes it possible, and it's
  elegant, but recovery would get slower with every tick and every balance change would
  break old logs.

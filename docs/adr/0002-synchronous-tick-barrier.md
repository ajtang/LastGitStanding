# ADR-0002: A tick is a synchronous barrier, not an event flow

**Status:** accepted

## Why this exists

Each tick needs infrastructure, then incidents, then flow, then economy, then telemetry,
each using the output of the one before. The architecture diagram wants this to be
event-driven. The game needs it to be correct.

## Decision

The Simulation Engine calls each service in sequence with an 800ms budget per call. If any
fails, the tick is abandoned and retried at the next interval.

Retries are safe because every stateful service treats a repeated tick number as a replay
and returns its cached response. The flow model works on a copy of its state, which is
committed only once the barrier succeeds.

Anything that doesn't feed the next tick (the timeline, the leaderboard) is outside the
barrier and is best-effort.

## Tradeoffs

- **One slow service stops every game.** We accept this: for a turn-based simulation,
  freezing is a correct failure mode, and silently skipping Economy for a tick isn't.
- **Availability of the game is the product of the availability of its services.**
  That's a real cost of ADR-0001.
- The conductor needs to know the order, so it's coupled to everyone. It's the one place
  that coupling lives, and you can read the whole sequence in `Engine.Step`.

## Rejected

- **Choreography over events.** Determinism would then depend on delivery order, every
  service would need to buffer until its inputs arrived, and a tick would have no
  observable end. A saga to compute a number.
- **Parallel fan-out.** The steps depend on each other, so there's nothing to parallelize
  except Observability, and 1ms doesn't matter.

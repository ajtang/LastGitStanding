# Architecture

## One tick

Everything interesting happens inside `Engine.Step`. Read that first.

```
                   ┌───────────────── Simulation Engine ─────────────────┐
                   │  the clock, the order of operations, the flow model  │
                   └──┬────────┬────────┬────────┬────────┬───────────────┘
        1. Reconcile  │        │        │        │        │ after commit, best effort
   pipeline, Flux,    ▼        │        │        │        ▼
   move actual state  Infra    │        │        │     Redis Stream ──▶ Event Processing ──▶ Postgres
                               ▼        │        │     (slow path)      "what changed?"      timeline
           2. Advance    Incident       │        │
   open/close incidents                 │        │     on game over ──▶ Player State ──▶ Postgres
   against real topology                │        │                                       leaderboard
                       3. flow.Step  (local, pure)
                       latency, errors, saturation
                                        ▼        │
                       4. Settle     Economy     │
                       money, users, hours       ▼
                       5. Ingest           Observability
                       store truth, return fired alerts

   Browser ◀── SSE ── Gateway ◀── gRPC stream ── Simulation     (Gateway strips ground truth)
   Browser ── POST ─▶ Gateway ── Submit ───────▶ Simulation     (commands land between ticks)
```

The barrier order isn't arbitrary. Incidents read this tick's topology, so a fix takes effect
the moment it lands. Flow reads this tick's effects. Economy bills what flow actually served.
Alerts feed into the *next* tick's detection, which is why alerting can't be instant.

## Where things live

| Path | What's there |
|---|---|
| `proto/lgs/v1/` | The contracts, and also the domain vocabulary (ADR-0004) |
| `services/simulation/flow/` | The capacity model. Pure. The best place to start reading |
| `services/simulation/` | The conductor and the command door |
| `services/infrastructure/` | Commits, pipeline, Flux and convergence |
| `services/incident/` | The catalog of ways things go wrong, and what ends each |
| `services/economy/` | Money, users, funding rounds. Prices are in `internal/catalog` |
| `services/observability/` | Tiered metrics, generated logs and traces, alert rules |
| `internal/harness/` | All services in one process, plus the headless players |
| `web/components/city/` | The Three.js city. `layer.ts` is the idea, `world.ts` is the map |
| `platform/`, `clusters/` | Kubernetes manifests and Flux entry points |
| `infrastructure/bicep/` | Azure |

## How failure works in the model

Most of the game's character comes from `flow.State`, which exists because failures outlive
their causes:

- **Retry storms.** Failed attempts come back as load, so an overloaded fleet can sustain its
  own overload after the trigger is gone. Only a circuit breaker lets it recover by itself.
- **Cold caches.** Redis coming back doesn't fix the database. The cache warms over about 25
  ticks, and the database pays until it has.
- **Slow databases become app problems.** Slow queries hold app threads, which lowers app
  capacity on the next tick, so saturation shows up one hop from the actual cause.
- **Retries hide bad pods.** One bad pod behind retries is almost invisible to users, which
  is why canary analysis compares per-version errors instead.

Incident titles describe symptoms, and several kinds share one on purpose. "Database latency
is climbing" might be a hot query or a dead cache, and the fixes are completely different.

## Running it for real, and the elephant

The production deployment in `infrastructure/bicep` is single-region, with no zone redundancy
and burstable Postgres. Yes, it's a reliability game that wouldn't survive its own Region
Failure incident.

That's a deliberate decision. Going multi-region means a geo-replicated Redis with its failover
semantics in the tick loop, and a second AKS cluster, at about three times the cost, to protect
a game whose worst case is that a run pauses. The game itself would tell you to take that
trade only if the revenue justified it.

## Known limits

- **One Simulation Engine pod.** Games aren't sharded, and every service is a singleton (ADR-0003).
- **Rebalancing is a judgement call.** Headless players bound the game from both ends, but
  no bot plays like a tired human. See the balance section in the README.
- **Late-game money is too easy.** By Series C, budget stops being a real constraint.
- **Live command timing isn't recorded,** so a live game can't yet be replayed from its seed.
- **Abuse limits are per IP and per process.** They stop a script, not a botnet, and each gateway
  replica counts separately. Game IDs are the only credential: anyone with the ID can play that game.
- **Some rows never expire.** Checkpoints age out after a week and telemetry after a day, but
  leaderboard runs and the Postgres timeline stay until someone deletes them.

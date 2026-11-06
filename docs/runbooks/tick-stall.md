# Runbook: tick stalled

**Alert:** `Tick barrier is stalling` (more than 10 `tick stalled` logs in 5 minutes)

**What players see:** the clock stops. Nothing is lost. The same tick retries at each interval,
and services treat it as a replay, so it's safe to leave running while you look.

## Find the failing step

The log line says which step failed:

```bash
kubectl -n lgs logs deploy/simulation | grep "tick stalled" | tail -5
```

| `err` starts with | Look at |
|---|---|
| `infrastructure:` | `deploy/infrastructure`. A very long commit history makes each write slower |
| `incidents:` / `economy:` | that service's logs, then Redis |
| `observability:` | Memory. It holds telemetry in-process. An OOMKill loses history, not games |
| `context deadline exceeded` everywhere | **Redis first.** Every checkpoint in the barrier goes through it |

## If it's Redis

If this service starts screaming, it's usually Redis. Check the instance in the portal, then:

```bash
kubectl -n lgs exec deploy/gateway -- wget -qO- localhost:8080/healthz
```

The gateway stays up without Redis, so a good response here only rules out the gateway and
network. If Redis lost data, in-progress games are gone (the checkpoints are the only copy)
and players will see "Gone" on reload. Finished runs are safe in Postgres.

## After a restart

Games resume **paused** from their last checkpoint, and telemetry history starts empty. Both
are expected. Players press play.

## What not to do

Don't scale any of these up to "add capacity". A second replica holds a stale in-memory copy
of each game and corrupts the run (ADR-0003). The gateway and web are the only ones that scale.

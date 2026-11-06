# ADR-0007: One binary, every service, one Go module

**Status:** accepted

## Why this exists

Eight services sharing one proto package, released together. Separate modules and images
would give us version skew to manage in exchange for independence we don't use.

## Decision

`lgs <service>` picks which server to run. All Deployments use one image. `lgs dev` runs them
all in a single process, on real ports over real gRPC, so playing it locally needs Go and
nothing else.

## Tradeoffs

- Every service ships every dependency, so Observability links a Postgres driver it never
  uses. The image is still small enough that we don't care.
- A change to any service rolls all of them, which interrupts running games for a few
  seconds. They resume paused, from their checkpoints.

## Rejected

- **Per-service modules and images.** The right call once teams release independently. There
  is one team here.
- **docker-compose as the only dev path.** It's in the repo (`docker-compose.yml`), but it
  shouldn't be what stands between a contributor and a running game.

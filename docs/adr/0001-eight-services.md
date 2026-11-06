# ADR-0001: Eight services, and an honest account of why

**Status:** accepted

## Why this exists

The game teaches distributed systems, so it runs as one. That's a real reason, but it's
not a reason for any *particular* boundary. Those each have to earn their place, or this
is a monolith with extra latency.

## Decision

Split where a boundary gives a property we can name:

| Service | What the boundary buys |
|---|---|
| Simulation | Owns the clock and order of operations. Nothing else needs to know a tick exists until it's called. |
| Infrastructure | Sole writer of actual state. "Nothing changes except through the pipeline" is enforced by there being no other RPC. |
| Incident | Holds ground truth about what's wrong. Keeping it out of Infrastructure means it can only *observe* a fix, never grant one. |
| Economy | The one place prices and money live, and the only thing that can refuse an action for money reasons. |
| Observability | The information boundary. It's what turns truth into what the player has paid to see. |
| Event Processing | The slow path. Postgres can lag or be down without a tick noticing. |
| Player State | The only data that outlives a run, with a lifecycle unlike everything else's. |
| Gateway | Browser protocol translation, and the redaction boundary. |

## Tradeoffs

- Five gRPC calls per tick, per game, where function calls would do. At 8x that's 40 calls
  a second per game. Fine at this scale, and it would be the first thing to go at a much
  larger one.
- Most services are singletons per deployment (ADR-0003), so we get the operational cost
  of microservices without most of the scaling benefit. We know.

## Rejected

- **A modular monolith.** Correct for a normal product of this size. Here it would remove
  the failure modes the game is about, and `lgs dev` already gives us the convenience.
- **Separate Economy and "Users" services.** Growth and revenue change together every tick,
  on the same inputs. Splitting them would be a distributed transaction for no gain.
- **Separate Logs, Metrics and Traces services.** Same inputs, same tiering rule, same
  caller. Three deployments for one decision.

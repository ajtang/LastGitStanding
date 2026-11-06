# ADR-0004: Protobuf messages are the domain types

**Status:** accepted

## Why this exists

`Topology`, `Health` and `Effects` cross service boundaries every tick. The textbook answer
is internal domain types in each service with mapping layers at the edge, which for this
repo means thousands of lines that do nothing and can each be subtly wrong.

## Decision

Generated messages are used directly in domain code, including the pure flow model. Services
keep their own private Go structs only for state nobody else sees (for example `incident`
and `game`).

## Tradeoffs

- Domain code uses `GetFoo()` everywhere and is nil-safe by accident, which has hidden at
  least one wrong default so far. Zero means "unset" for `traffic_multiplier`, and flow has
  to know that.
- A proto change is a domain change, with no insulation. For one team in one repo, that's
  a feature.
- The browser types are still hand-mirrored in `web/lib/types.ts`. That's the one mapping
  layer we kept, and it will drift eventually.

## Rejected

- **Mapping layers.** They pay off when teams or release cycles differ. Here one binary
  ships everything at once (ADR-0007).
- **JSON over HTTP between services.** It loses the contract, and the contract is half the
  point of showing how this would really be built.

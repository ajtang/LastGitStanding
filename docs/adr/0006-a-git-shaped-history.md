# ADR-0006: The game's git is git-shaped, not git

**Status:** accepted

## Why this exists

Every infrastructure change has to flow through commit, pipeline and Flux. That's the
teaching mechanism. The question is how real the machinery needs to be.

## Decision

Infrastructure keeps a linear list of commits, with real SHA-1 hashes over parent, tick,
message and content. The pipeline is a state machine that takes ticks. "Flux" is a timer
that applies whatever is on main. Validation is real: manifests go through YAML, a strict
schema, lint and policy, in that order, and typos fail the way they would against a CRD.

Incidents resolve only by observing actual topology, so there's no API a fix could call.
That rule matters more than any of the machinery.

Some behaviours are copied from real tools because they're the lesson: Flux polls instead
of being told, a stalled reconcile says so, canary analysis can't see a slow leak, and a
failed canary leaves git and the cluster disagreeing until a human decides.

## Tradeoffs

- No branches, PRs or merge conflicts. These would teach more, but they take longer to play
  than an outage lasts. A single trunk is also a defensible real-world choice.
- A real infrastructure engineer will notice this isn't `go-git`. They'll also notice the
  rules behave the way they're used to, and that's the part we care about.

## Rejected

- **Real git via go-git, one repo per game.** Disk per game and an awkward checkpoint story,
  for hashes nobody can tell apart.
- **Direct "scale" buttons with a fake delay.** This is the whole thing the game argues
  against. The quick-edit chips rewrite the manifest text, so they're typing help and not a
  side door.

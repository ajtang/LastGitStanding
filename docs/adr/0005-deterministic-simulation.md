# ADR-0005: Same seed, same commands, same game

**Status:** accepted

## Why this exists

Balance changes need before-and-after numbers, and a bug report needs to be reproducible. A
stochastic game across eight processes offers neither unless this is designed in.

## Decision

- Randomness has no stored state. Every roll comes from `dice.For(seed, tick, stream)`, so
  retries roll the same numbers and adding a new stream can't shift existing ones.
- Domain code never reads the wall clock, and never lets map iteration order reach an
  outcome.
- `TestSameSeedSameGame` plays a game through the full stack twice and compares.

The browser's vehicles and particles use `Math.random()`. They're decoration, not state.

## Tradeoffs

- Determinism is only as good as command timing. Live games are reproducible only given the
  tick each command landed on, and we don't record that yet.
- Everyone has to remember the map rule. We have already shipped and caught that bug once,
  in `rollDeployFlaws`, in the same file as a comment warning about it.

## Rejected

- **A seeded RNG object per service.** A retried tick would advance it twice, and we'd have
  to checkpoint its state.
- **A central randomness service.** Yes, someone suggested it. No.

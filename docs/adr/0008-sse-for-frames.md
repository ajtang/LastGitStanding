# ADR-0008: Frames reach the browser over SSE

**Status:** accepted

## Why this exists

The browser needs a frame per tick, up to eight a second, while everything it sends is an
occasional discrete command with a response it wants to read.

## Decision

Server-sent events for frames, plain POSTs for commands. Internally the gateway consumes a
gRPC server stream from the Simulation Engine. A slow watcher drops frames instead of
slowing the game, and since every frame is a full snapshot, the next one repairs it.

## Tradeoffs

- Frames are full JSON snapshots, not deltas. A late-game frame with a couple of hundred pods
  is tens of kilobytes, several times a second. Fine now, and the first thing to optimize.
- Proxies need telling not to buffer, which the ingress annotations handle.

## Rejected

- **WebSockets.** Bidirectional capability we wouldn't use, and more for the ingress to
  terminate. Reconnection would also be ours to write; `EventSource` does it for free.
- **gRPC-Web.** Tooling for the one consumer that would need it least.

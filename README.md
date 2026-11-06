# Last Git Standing

You inherit one cluster, one database, a few thousand users and a certificate nobody has looked
at in a while. Keep it up, keep it funded, and get it to IPO.

There's no console. Every change is a commit, through a pipeline, reconciled by Flux, and it all
takes longer than you'd like. Your infrastructure is a miniature city: each building is a pod, and
you can see when it's in trouble. Working out *why* costs money, and that's rather the point.

## Play it

You need Go and Node. No Docker, no database.

```bash
make dev
```

```bash
npm --prefix web install && npm --prefix web run dev
```

Then open <http://localhost:3000>. Space pauses, and a page drops you back to 1x.

That runs everything in memory. To use real Redis and Postgres without Docker, `make deps` starts
private instances and prints two URLs to export before `make dev`.

## What you'll be doing

- **Capacity.** App pods, node pools, load balancers, database tier, cache, disk. Each has its own
  limit, each costs money, and each takes real time to arrive.
- **GitOps.** Edit `platform.yaml`, push, then wait through lint, policy, plan, merge and the next
  Flux sync. Typos fail lint. Asking for more pods than you have nodes leaves them pending and Flux
  stalled. A failed canary leaves git and the cluster disagreeing until you decide.
- **Incidents.** Traffic spikes, memory leaks, query hotspots, bad deploys, cert expiry, Redis
  failures, region failures, network partitions, CVEs, and the ones you built yourself: running out
  of capacity and filling the disk. Pages describe symptoms. An incident ends only when reality
  stops having the problem.
- **Observability.** Metrics, logs, traces and alerts are all things you buy, and logs scale with
  traffic. Better tiers detect sooner and show what the cheap ones can't: the missing index is only
  named in the slow query log, and a network partition looks fine everywhere except in a trace.
- **The business.** Funding rounds need users *and* availability. Features drive growth, add debt,
  and are sometimes cursed. Debt makes everything after it worse. Postmortems make surprises rarer.

## Is it actually a game?

Headless players use the same frames and commands as a browser, over real gRPC:

```bash
make balance
```

| Player | Plays like | Typical result |
|---|---|---|
| `neglect` | does nothing | Fired around day 6, when the certificate expires |
| `cowboy` | ships constantly, no margin, no canary, no debt paydown | Fired or broke by about day 45, never closes Series A |
| `competent` | plans ahead, reads logs, rolls back, writes postmortems | IPO around day 65 in 9 of 10 seeds |

These are also tests (`internal/harness`). If neglect ever wins, or discipline stops clearly beating
recklessness, the build fails. Run this before and after touching a number in `internal/catalog`.

The bots have instant reflexes, so they bound the game rather than predict it. Expect a human to
find it harder than `competent` does.

## Reading the code

Start with `services/simulation/flow/flow.go`, then `Engine.Step` in
`services/simulation/engine.go`. [docs/architecture.md](docs/architecture.md) has the map, and
[docs/adr/](docs/adr/) has the reasoning, including what was rejected.

## Testing

```bash
make test
```

```bash
REDIS_URL=... DATABASE_URL=... make integration
```

Tests live next to their code and try to pin down behaviour someone would care about: a retry storm
outlives its trigger, an expired cert leaves pods looking bored, a promoted standby doesn't fail back
into a dead region. `test/integration` covers the code that talks to real stores.

## Deploying

`infrastructure/bicep` creates AKS with the Flux extension, ACR, Postgres, Redis, Key Vault and Azure
Monitor. After that, `clusters/<env>` is what Flux watches and `platform/lgs/overlays/<env>` is what
gets applied. The release workflow never touches a cluster. It pushes images and commits a new tag.

Before a first deploy, fill in the `REPLACE_WITH_*` values in the overlays from the Bicep outputs,
and swap the `lastgitstanding.example` hostnames and alert address for your own.

`lgs dev` also works as a small public demo, because it binds everything to loopback. Put a tunnel
or reverse proxy in front of the gateway on `:8080`, and set:

| Variable | What it's for |
|---|---|
| `ALLOWED_ORIGINS` | Comma-separated origins allowed to call the gateway. Required for any non-local frontend |
| `CLIENT_IP_HEADER` | The header your proxy puts the client IP in, such as `CF-Connecting-IP`. Without it, every player behind the proxy shares one rate limit. Only set it if the proxy is the only way in, since anyone can send the header |
| `LGS_MAX_GAMES` | How many game clocks can run at once. The default is 50, and new games get a 429 past that |
| `LGS_ADMIN_PASSCODE` | Turns on leaderboard editing. Unset, the admin routes don't exist. Keep it out of git, since this is the only thing protecting the leaderboard |
| `LGS_GATEWAY_ADDR` | Where the gateway listens, if loopback isn't what you want |

A game nobody has watched for two minutes pauses and stops counting towards `LGS_MAX_GAMES`. It
stays resumable for a week. Without `REDIS_URL` and `DATABASE_URL`, a restart loses every game and
the leaderboard.

// lgs is one binary containing every service, selected by subcommand. One image
// per release means there's no version skew to think about between services that
// share a proto package, and `lgs dev` gets the whole platform without Docker.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/bus"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
	"github.com/ajtang/LastGitStanding/internal/harness"
	"github.com/ajtang/LastGitStanding/internal/svc"
	"github.com/ajtang/LastGitStanding/services/economy"
	"github.com/ajtang/LastGitStanding/services/events"
	"github.com/ajtang/LastGitStanding/services/gateway"
	"github.com/ajtang/LastGitStanding/services/incident"
	"github.com/ajtang/LastGitStanding/services/infrastructure"
	"github.com/ajtang/LastGitStanding/services/observability"
	"github.com/ajtang/LastGitStanding/services/playerstate"
	"github.com/ajtang/LastGitStanding/services/simulation"
)

type runner func(ctx context.Context, e *env) error

var services = map[string]runner{
	"simulation":     runSimulation,
	"infrastructure": runInfrastructure,
	"incident":       runIncident,
	"economy":        runEconomy,
	"observability":  runObservability,
	"events":         runEvents,
	"playerstate":    runPlayerState,
	"gateway":        runGateway,
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lgs <dev|balance|"+strings.Join(serviceNames(), "|")+">")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd := os.Args[1]; cmd {
	case "dev":
		err = runDev(ctx)
	case "balance":
		err = runBalance(ctx)
	default:
		run, ok := services[cmd]
		if !ok {
			fmt.Fprintln(os.Stderr, "unknown command:", cmd)
			os.Exit(2)
		}
		err = run(ctx, newEnv())
	}
	if err != nil && ctx.Err() == nil {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func serviceNames() []string {
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	return names
}

// runDev starts everything in one process on localhost with the same wiring as
// production, real ports and real gRPC included. REDIS_URL and DATABASE_URL are
// honoured if set; otherwise state lives in memory until you hit ctrl-c.
func runDev(ctx context.Context) error {
	// Dev mode has no NetworkPolicy in front of it, and these gRPC ports trust whoever
	// connects. On a laptop that means the coffee shop; on a VPS, the whole internet.
	bindHost = "127.0.0.1"
	e := newEnv()
	if e.store == nil {
		slog.Info("no REDIS_URL: using in-memory checkpoints and events")
		mem := checkpoint.NewMemoryStore()
		e.store, e.bus = mem, bus.NewMemory()
	}
	g, ctx := errgroup.WithContext(ctx)
	for name, run := range services {
		g.Go(func() error {
			if err := run(ctx, e); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			return nil
		})
	}
	return g.Wait()
}

// runBalance plays headless games and prints how they went. Run it before and after
// touching a number in the catalog, and paste the difference into the PR.
func runBalance(ctx context.Context) error {
	s := harness.Start()
	defer s.Stop()
	for _, newPlayer := range []func() harness.Player{
		func() harness.Player { return harness.Neglect{} },
		func() harness.Player { return &harness.Competent{Cowboy: true} },
		func() harness.Player { return &harness.Competent{} },
	} {
		for seed := int64(1); seed <= 10; seed++ {
			r, err := harness.Play(ctx, s, seed, newPlayer(), 150)
			if err != nil {
				return err
			}
			outcome := "still running at day cap"
			if r.Won {
				outcome = "IPO"
			} else if r.Outcome != "" {
				outcome = r.Outcome[:min(len(r.Outcome), 40)] + "..."
			}
			fmt.Printf("%-10s seed=%-3d day=%-4.0f %-9s users=%-8.0f budget=%-9.0f avail=%.4f debt=%-3.0f score=%-7.0f %s\n",
				r.Player, r.Seed, r.Days, r.Stage, r.Users, r.Budget, r.Avail, r.Debt, r.Score, outcome)
		}
	}
	return nil
}

// ---- environment

type env struct {
	store checkpoint.Store
	bus   bus.Bus
	pool  *pgxpool.Pool
}

func newEnv() *env {
	e := &env{}
	if url := os.Getenv("REDIS_URL"); url != "" {
		opts, err := redis.ParseURL(url)
		if err != nil {
			slog.Error("bad REDIS_URL", "err", err)
			os.Exit(1)
		}
		client := redis.NewClient(opts)
		host, _ := os.Hostname()
		e.store, e.bus = checkpoint.NewRedisStore(client), bus.NewRedis(client, host)
	}
	if url := os.Getenv("DATABASE_URL"); url != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			slog.Error("bad DATABASE_URL", "err", err)
			os.Exit(1)
		}
		e.pool = pool
	}
	return e
}

func (e *env) requireStore() (checkpoint.Store, error) {
	if e.store == nil {
		return nil, fmt.Errorf("REDIS_URL is required")
	}
	return e.store, nil
}

func addr(service, fallback string) string {
	key := "LGS_" + strings.ToUpper(service) + "_ADDR"
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// defaultMaxGames is a guess at what one small machine can tick at once, and it counts
// only games somebody is watching. It's a ceiling for a bad day, not a capacity plan.
const defaultMaxGames = 50

const (
	simAddr     = "localhost:7001"
	infraAddr   = "localhost:7002"
	incidAddr   = "localhost:7003"
	econAddr    = "localhost:7004"
	obsAddr     = "localhost:7005"
	eventsAddr  = "localhost:7006"
	playersAddr = "localhost:7007"
)

func dial(service, fallback string) *grpc.ClientConn {
	conn, err := grpc.NewClient(addr(service, fallback), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("bad grpc target", "service", service, "err", err)
		os.Exit(1)
	}
	return conn
}

// bindHost is empty for a real service, whose pod or container network decides who
// can reach it, and loopback under `lgs dev`.
var bindHost string

// serve runs a gRPC server plus a sidecar metrics port. PORT overrides the gRPC port
// so a Kubernetes container can always listen on the same one.
func serve(ctx context.Context, name, fallback string, srv *grpc.Server) error {
	listen := bindHost + ":" + strings.Split(fallback, ":")[1]
	if v := os.Getenv("PORT"); v != "" {
		listen = bindHost + ":" + v
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return svc.ServeGRPC(ctx, name, listen, srv) })
	if metrics := os.Getenv("METRICS_ADDR"); metrics != "" {
		g.Go(func() error { return svc.ServeHTTP(ctx, name+"-metrics", metrics, svc.MetricsHandler()) })
	}
	return g.Wait()
}

// ---- services

func runInfrastructure(ctx context.Context, e *env) error {
	store, err := e.requireStore()
	if err != nil {
		return err
	}
	srv := svc.NewGRPCServer()
	lgsv1.RegisterInfrastructureServer(srv, infrastructure.NewServer(store))
	return serve(ctx, "infrastructure", infraAddr, srv)
}

func runIncident(ctx context.Context, e *env) error {
	store, err := e.requireStore()
	if err != nil {
		return err
	}
	srv := svc.NewGRPCServer()
	lgsv1.RegisterIncidentsServer(srv, incident.NewServer(store))
	return serve(ctx, "incident", incidAddr, srv)
}

func runEconomy(ctx context.Context, e *env) error {
	store, err := e.requireStore()
	if err != nil {
		return err
	}
	srv := svc.NewGRPCServer()
	lgsv1.RegisterEconomyServer(srv, economy.NewServer(store))
	return serve(ctx, "economy", econAddr, srv)
}

func runObservability(ctx context.Context, _ *env) error {
	srv := svc.NewGRPCServer()
	lgsv1.RegisterObservabilityServer(srv, observability.NewServer())
	return serve(ctx, "observability", obsAddr, srv)
}

func runSimulation(ctx context.Context, e *env) error {
	store, err := e.requireStore()
	if err != nil {
		return err
	}
	maxGames := defaultMaxGames
	if v := os.Getenv("LGS_MAX_GAMES"); v != "" {
		if maxGames, err = strconv.Atoi(v); err != nil {
			return fmt.Errorf("bad LGS_MAX_GAMES: %w", err)
		}
	}
	engine := simulation.New(simulation.Deps{
		Infra:    lgsv1.NewInfrastructureClient(dial("infrastructure", infraAddr)),
		Incid:    lgsv1.NewIncidentsClient(dial("incident", incidAddr)),
		Econ:     lgsv1.NewEconomyClient(dial("economy", econAddr)),
		Obs:      lgsv1.NewObservabilityClient(dial("observability", obsAddr)),
		Players:  lgsv1.NewPlayerStateClient(dial("playerstate", playersAddr)),
		Bus:      e.bus,
		Store:    store,
		MaxGames: maxGames,
	})
	go engine.ReapIdle(ctx)
	srv := svc.NewGRPCServer()
	lgsv1.RegisterSimulationServer(srv, engine)
	return serve(ctx, "simulation", simAddr, srv)
}

func runEvents(ctx context.Context, e *env) error {
	if e.bus == nil {
		return fmt.Errorf("REDIS_URL is required")
	}
	var store events.Store = events.NewMemoryStore()
	if e.pool != nil {
		pg, err := events.NewPostgresStore(ctx, e.pool)
		if err != nil {
			return fmt.Errorf("timeline schema: %w", err)
		}
		store = pg
	}
	s := events.NewServer(store)
	srv := svc.NewGRPCServer()
	lgsv1.RegisterEventProcessingServer(srv, s)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return e.bus.Consume(ctx, s.Handle) })
	g.Go(func() error { return serve(ctx, "events", eventsAddr, srv) })
	return g.Wait()
}

func runPlayerState(ctx context.Context, e *env) error {
	var store playerstate.Store = playerstate.NewMemoryStore()
	if e.pool != nil {
		pg, err := playerstate.NewPostgresStore(ctx, e.pool)
		if err != nil {
			return fmt.Errorf("runs schema: %w", err)
		}
		store = pg
	}
	srv := svc.NewGRPCServer()
	lgsv1.RegisterPlayerStateServer(srv, playerstate.NewServer(store))
	return serve(ctx, "playerstate", playersAddr, srv)
}

func runGateway(ctx context.Context, _ *env) error {
	origins := strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",")
	if os.Getenv("ALLOWED_ORIGINS") == "" {
		origins = []string{"http://localhost:3000"}
	}
	h := gateway.New(gateway.Deps{
		Sim:            lgsv1.NewSimulationClient(dial("simulation", simAddr)),
		Infra:          lgsv1.NewInfrastructureClient(dial("infrastructure", infraAddr)),
		Econ:           lgsv1.NewEconomyClient(dial("economy", econAddr)),
		Obs:            lgsv1.NewObservabilityClient(dial("observability", obsAddr)),
		Events:         lgsv1.NewEventProcessingClient(dial("events", eventsAddr)),
		Players:        lgsv1.NewPlayerStateClient(dial("playerstate", playersAddr)),
		AllowedOrigins: origins,
		ClientIPHeader: os.Getenv("CLIENT_IP_HEADER"),
		AdminPasscode:  os.Getenv("LGS_ADMIN_PASSCODE"),
	})
	listen := addr("gateway", bindHost+":8080")
	return svc.ServeHTTP(ctx, "gateway", listen, http.Handler(h))
}

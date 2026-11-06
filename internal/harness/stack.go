// Package harness runs every service in one process over real gRPC, minus the
// network. It exists so "does the game work" is a test, and so balance changes
// can be argued about with numbers rather than vibes.
package harness

import (
	"context"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	lgsv1 "github.com/ajtang/LastGitStanding/gen/lgs/v1"
	"github.com/ajtang/LastGitStanding/internal/bus"
	"github.com/ajtang/LastGitStanding/internal/checkpoint"
	"github.com/ajtang/LastGitStanding/services/economy"
	"github.com/ajtang/LastGitStanding/services/events"
	"github.com/ajtang/LastGitStanding/services/incident"
	"github.com/ajtang/LastGitStanding/services/infrastructure"
	"github.com/ajtang/LastGitStanding/services/observability"
	"github.com/ajtang/LastGitStanding/services/playerstate"
	"github.com/ajtang/LastGitStanding/services/simulation"
)

type Stack struct {
	Engine  *simulation.Engine
	Infra   lgsv1.InfrastructureClient
	Incid   lgsv1.IncidentsClient
	Obs     lgsv1.ObservabilityClient
	Econ    lgsv1.EconomyClient
	Players lgsv1.PlayerStateClient
	Events  lgsv1.EventProcessingClient
	Sim     lgsv1.SimulationClient

	Store checkpoint.Store
	Bus   bus.Bus

	stop func()
}

func Start() *Stack {
	store := checkpoint.NewMemoryStore()
	eventBus := bus.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())

	var servers []*grpc.Server
	dial := func(register func(*grpc.Server)) *grpc.ClientConn {
		lis := bufconn.Listen(1 << 20)
		srv := grpc.NewServer()
		register(srv)
		servers = append(servers, srv)
		go srv.Serve(lis)
		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			panic(err) // NewClient doesn't dial; this only fails on bad options
		}
		return conn
	}

	infra := lgsv1.NewInfrastructureClient(dial(func(s *grpc.Server) { lgsv1.RegisterInfrastructureServer(s, infrastructure.NewServer(store)) }))
	incid := lgsv1.NewIncidentsClient(dial(func(s *grpc.Server) { lgsv1.RegisterIncidentsServer(s, incident.NewServer(store)) }))
	econ := lgsv1.NewEconomyClient(dial(func(s *grpc.Server) { lgsv1.RegisterEconomyServer(s, economy.NewServer(store)) }))
	obs := lgsv1.NewObservabilityClient(dial(func(s *grpc.Server) { lgsv1.RegisterObservabilityServer(s, observability.NewServer()) }))
	players := lgsv1.NewPlayerStateClient(dial(func(s *grpc.Server) {
		lgsv1.RegisterPlayerStateServer(s, playerstate.NewServer(playerstate.NewMemoryStore()))
	}))

	eventsSrv := events.NewServer(events.NewMemoryStore())
	eventsClient := lgsv1.NewEventProcessingClient(dial(func(s *grpc.Server) { lgsv1.RegisterEventProcessingServer(s, eventsSrv) }))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		eventBus.Consume(ctx, eventsSrv.Handle)
	}()

	engine := simulation.New(simulation.Deps{Infra: infra, Incid: incid, Econ: econ, Obs: obs, Players: players, Bus: eventBus, Store: store})
	sim := lgsv1.NewSimulationClient(dial(func(s *grpc.Server) { lgsv1.RegisterSimulationServer(s, engine) }))

	return &Stack{
		Engine: engine, Infra: infra, Incid: incid, Obs: obs, Econ: econ, Players: players, Events: eventsClient, Sim: sim, Store: store, Bus: eventBus,
		stop: func() {
			cancel()
			wg.Wait()
			for _, s := range servers {
				s.Stop()
			}
		},
	}
}

func (s *Stack) Stop() { s.stop() }

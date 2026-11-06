// Package svc is the shared service bootstrap: the health endpoint, the metrics
// endpoint and graceful shutdown. It's deliberately not a framework. If a service
// needs something different it should just do something different.
package svc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
)

var rpcDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name: "lgs_rpc_duration_seconds",
	Help: "Server-side gRPC latency. The tick budget is 800ms per call; alert long before that.",
	// Sub-millisecond buckets because these calls should be, and because when one
	// stops being, that's the interesting part of the graph.
	Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
}, []string{"method", "error"})

func init() { prometheus.MustRegister(rpcDuration) }

func observeRPC(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := next(ctx, req)
	rpcDuration.WithLabelValues(info.FullMethod, fmt.Sprint(err != nil)).Observe(time.Since(start).Seconds())
	return resp, err
}

func NewGRPCServer() *grpc.Server {
	return grpc.NewServer(
		grpc.UnaryInterceptor(observeRPC),
		// Frames stream to browsers for as long as a game is open; don't let an idle
		// LB quietly eat the connection.
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second}),
	)
}

// ServeGRPC runs a server until ctx is cancelled, with the standard health service
// attached so Kubernetes probes have something to call.
func ServeGRPC(ctx context.Context, name, addr string, srv *grpc.Server) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s: listen: %w", name, err)
	}
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)

	stop := context.AfterFunc(ctx, func() {
		hs.Shutdown()
		graceful := make(chan struct{})
		go func() { srv.GracefulStop(); close(graceful) }()
		select {
		case <-graceful:
		case <-time.After(10 * time.Second):
			srv.Stop() // Watch streams never finish on their own, so don't wait forever.
		}
	})
	defer stop()

	slog.Info("serving", "service", name, "addr", addr)
	if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// ServeHTTP runs an HTTP server until ctx is cancelled.
func ServeHTTP(ctx context.Context, name, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	stop := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	})
	defer stop()
	slog.Info("serving", "service", name, "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// MetricsHandler is what the Azure Monitor managed Prometheus scraper reads.
func MetricsHandler() http.Handler { return promhttp.Handler() }

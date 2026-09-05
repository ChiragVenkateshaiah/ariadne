// Package metrics is the one shared piece of Prometheus wiring every
// control-plane component uses: a tiny HTTP server exposing /metrics on its
// own port (gRPC and HTTP can't easily share one listener), started
// alongside -- not instead of -- each component's gRPC server. Each
// package defines its own counters/histograms next to the code that
// increments them (see internal/watch, internal/logs, internal/orchestrate)
// -- this file only serves what prometheus's default registry already
// collected.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Serve starts the /metrics HTTP server in the background and returns
// immediately; the caller wires its shutdown into the same context that
// stops the gRPC server. The port is overridable via METRICS_PORT (e.g. to
// avoid a clash running two of these components on the same host network),
// falling back to defaultPort -- each cmd/* passes its own, mirroring how
// each already picks a distinct default gRPC port.
func Serve(ctx context.Context, logger *slog.Logger, defaultPort string) {
	port := os.Getenv("METRICS_PORT")
	if port == "" {
		port = defaultPort
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: fmt.Sprintf(":%s", port), Handler: mux}

	lis, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		logger.Error("metrics server failed to listen", "port", port, "error", err.Error())
		return
	}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	go func() {
		logger.Info("metrics listening", "addr", lis.Addr().String())
		if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server exited", "error", err.Error())
		}
	}()
}

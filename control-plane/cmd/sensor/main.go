// Command sensor runs the Ariadne Cluster Sensor: it watches the resource
// kinds in internal/watch.Resources, normalizes changes into ChangeEvents,
// and serves them over gRPC as ChangeStreamService.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"github.com/chirag/ariadne/control-plane/internal/watch"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("component", "sensor")

	cfg, err := loadKubeConfig()
	if err != nil {
		logger.Error("failed to load kubeconfig", "error", err.Error())
		os.Exit(1)
	}

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		logger.Error("failed to build dynamic client", "error", err.Error())
		os.Exit(1)
	}

	broker := watch.NewBroker(historySize())
	sensor := watch.NewSensor(client, broker, logger, resyncPeriod())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := sensor.Start(ctx); err != nil {
		logger.Error("sensor failed to start", "error", err.Error())
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "9090"
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		logger.Error("failed to listen", "error", err.Error())
		os.Exit(1)
	}

	grpcServer := grpc.NewServer()
	ariadnev1.RegisterChangeStreamServiceServer(grpcServer, watch.NewServer(broker))

	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthSrv)

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		grpcServer.GracefulStop()
	}()

	logger.Info("sensor listening", "addr", lis.Addr().String())
	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("grpc server exited", "error", err.Error())
		os.Exit(1)
	}
}

// loadKubeConfig prefers in-cluster config (how this runs in the demo, as a
// Deployment with a ServiceAccount) and falls back to $KUBECONFIG / the
// default kubeconfig path for local development against kind.
func loadKubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func historySize() int {
	return 5000 // ~a demo session's worth of change events, kept for replay/recent-changes
}

func resyncPeriod() time.Duration {
	return 10 * time.Minute
}

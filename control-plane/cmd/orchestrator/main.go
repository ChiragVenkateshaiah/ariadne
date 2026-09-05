// Command orchestrator runs OrchestratorService: turns ValidatorTasks into
// Kubernetes Jobs and streams back their results.
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

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"github.com/chirag/ariadne/control-plane/internal/orchestrate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("component", "orchestrator")

	cfg, err := loadKubeConfig()
	if err != nil {
		logger.Error("failed to load kubeconfig", "error", err.Error())
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error("failed to build kubernetes clientset", "error", err.Error())
		os.Exit(1)
	}

	defaultNamespace := os.Getenv("DEFAULT_NAMESPACE")
	if defaultNamespace == "" {
		defaultNamespace = "travel"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	port := os.Getenv("PORT")
	if port == "" {
		port = "9092"
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		logger.Error("failed to listen", "error", err.Error())
		os.Exit(1)
	}

	grpcServer := grpc.NewServer()
	ariadnev1.RegisterOrchestratorServiceServer(grpcServer, orchestrate.NewServer(clientset, defaultNamespace))

	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthSrv)

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		grpcServer.GracefulStop()
	}()

	logger.Info("orchestrator listening", "addr", lis.Addr().String(), "default_namespace", defaultNamespace)
	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("grpc server exited", "error", err.Error())
		os.Exit(1)
	}
}

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

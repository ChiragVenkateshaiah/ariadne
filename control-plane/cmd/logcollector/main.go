// Command logcollector runs the Ariadne LogCollector: pod logs, K8s Events,
// and the API-server audit log, unified behind one gRPC service so the
// Evidence Correlator can ask one question ("what happened around this
// failure?") instead of three.
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
	"github.com/chirag/ariadne/control-plane/internal/logs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("component", "logcollector")

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

	var changeStreamClient ariadnev1.ChangeStreamServiceClient
	if addr := os.Getenv("SENSOR_ADDR"); addr != "" {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			logger.Warn("could not dial sensor; recent-changes enrichment disabled", "addr", addr, "error", err.Error())
		} else {
			changeStreamClient = ariadnev1.NewChangeStreamServiceClient(conn)
			logger.Info("connected to sensor for recent-changes enrichment", "addr", addr)
		}
	}

	auditLogPath := os.Getenv("AUDIT_LOG_PATH")
	if auditLogPath == "" {
		logger.Warn("AUDIT_LOG_PATH not set; audit evidence and QueryAuditLog will be unavailable")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	port := os.Getenv("PORT")
	if port == "" {
		port = "9091"
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		logger.Error("failed to listen", "error", err.Error())
		os.Exit(1)
	}

	grpcServer := grpc.NewServer()
	ariadnev1.RegisterLogCollectorServiceServer(grpcServer, logs.NewServer(clientset, changeStreamClient, auditLogPath))

	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthSrv)

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		grpcServer.GracefulStop()
	}()

	logger.Info("logcollector listening", "addr", lis.Addr().String(), "audit_log_path", auditLogPath)
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

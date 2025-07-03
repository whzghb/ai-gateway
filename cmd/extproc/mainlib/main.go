// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package mainlib

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	metricsdk "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/envoyproxy/ai-gateway/filterapi/x"
	"github.com/envoyproxy/ai-gateway/internal/extproc"
	"github.com/envoyproxy/ai-gateway/internal/metrics"
	"github.com/envoyproxy/ai-gateway/internal/version"
)

// extProcFlags is the struct that holds the flags passed to the external processor.
type extProcFlags struct {
	configPath  string     // path to the configuration file.
	extProcAddr string     // gRPC address for the external processor.
	logLevel    slog.Level // log level for the external processor.
	metricsPort int        // HTTP port for the metrics server.
	healthPort  int        // HTTP port for the health check server.
}

// parseAndValidateFlags parses and validates the flags passed to the external processor.
func parseAndValidateFlags(args []string) (extProcFlags, error) {
	var (
		flags extProcFlags
		errs  []error
		fs    = flag.NewFlagSet("AI Gateway External Processor", flag.ContinueOnError)
	)

	fs.StringVar(&flags.configPath,
		"configPath",
		"",
		"path to the configuration file. The file must be in YAML format specified in filterapi.Config type. "+
			"The configuration file is watched for changes.",
	)
	fs.StringVar(&flags.extProcAddr,
		"extProcAddr",
		":1063",
		"gRPC address for the external processor. For example, :1063 or unix:///tmp/ext_proc.sock.",
	)
	logLevelPtr := fs.String(
		"logLevel",
		"info",
		"log level for the external processor. One of 'debug', 'info', 'warn', or 'error'.",
	)
	fs.IntVar(&flags.metricsPort, "metricsPort", 1064, "port for the metrics server.")
	fs.IntVar(&flags.healthPort, "healthPort", 1065, "port for the health check HTTP server.")

	if err := fs.Parse(args); err != nil {
		return extProcFlags{}, fmt.Errorf("failed to parse extProcFlags: %w", err)
	}

	if flags.configPath == "" {
		errs = append(errs, fmt.Errorf("configPath must be provided"))
	}
	if err := flags.logLevel.UnmarshalText([]byte(*logLevelPtr)); err != nil {
		errs = append(errs, fmt.Errorf("failed to unmarshal log level: %w", err))
	}

	return flags, errors.Join(errs...)
}

// Main is a main function for the external processor exposed
// for allowing users to build their own external processor.
//
// * ctx is the context for the external processor.
// * args are the command line arguments passed to the external processor without the program name.
// * stderr is the writer to use for standard error where the external processor will output logs.
//
// This returns an error if the external processor fails to start, or nil otherwise. When the `ctx` is canceled,
// the function will return nil.
func Main(ctx context.Context, args []string, stderr io.Writer) (err error) {
	defer func() {
		if errors.Is(err, context.Canceled) {
			err = nil
		}
	}()
	/*
		- args:
		    - -configPath
		    - /etc/filter-config/filter-config.yaml
		    - -logLevel
		    - info
		    - -extProcAddr
		    - unix:///etc/ai-gateway-extproc-uds/run.sock
		    - -metricsPort
		    - "1064"
		    - -healthPort
		    - "1065"
	*/
	flags, err := parseAndValidateFlags(args)
	if err != nil {
		return fmt.Errorf("failed to parse and validate extProcFlags: %w", err)
	}

	l := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: flags.logLevel}))

	l.Info("starting external processor",
		slog.String("version", version.Version),
		slog.String("address", flags.extProcAddr),
		slog.String("configPath", flags.configPath),
	)

	// 监听在unix:///etc/ai-gateway-extproc-uds/run.sock
	network, address := listenAddress(flags.extProcAddr)
	lis, err := net.Listen(network, address)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	// 为unix
	if network == "unix" {
		// Change the permission of the UDS to 0775 so that the envoy process (the same group) can access it.
		err = os.Chmod(address, 0o775)
		if err != nil {
			return fmt.Errorf("failed to change UDS permission: %w", err)
		}
	}

	metricsServer, meter := startMetricsServer(fmt.Sprintf(":%d", flags.metricsPort), l)
	chatCompletionMetrics := metrics.NewChatCompletion(meter, x.NewCustomChatCompletionMetrics)
	embeddingsMetrics := metrics.NewEmbeddings(meter)

	server, err := extproc.NewServer(l)
	if err != nil {
		return fmt.Errorf("failed to create external processor server: %w", err)
	}
	// 注册processor
	server.Register("/v1/chat/completions", extproc.ChatCompletionProcessorFactory(chatCompletionMetrics))
	server.Register("/v1/embeddings", extproc.EmbeddingsProcessorFactory(embeddingsMetrics))
	server.Register("/v1/models", extproc.NewModelsProcessor)

	// 启动配置文件监听器
	// server实现了LoadConfig方法,所以可以作为一个ConfigReceiver传入
	if err := extproc.StartConfigWatcher(ctx, flags.configPath, server, l, time.Second*5); err != nil {
		return fmt.Errorf("failed to start config watcher: %w", err)
	}

	s := grpc.NewServer()
	// server需要实现Process方法
	/*
		type ExternalProcessorServer interface {
			// This begins the bidirectional stream that Envoy will use to
			// give the server control over what the filter does. The actual
			// protocol is described by the ProcessingRequest and ProcessingResponse
			// messages below.
			Process(ExternalProcessor_ProcessServer) error
		}
	*/
	extprocv3.RegisterExternalProcessorServer(s, server)
	grpc_health_v1.RegisterHealthServer(s, server)

	hs := startHealthCheckServer(fmt.Sprintf(":%d", flags.healthPort), l, lis)
	go func() {
		<-ctx.Done()
		s.GracefulStop()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			l.Error("Failed to shutdown metrics server gracefully", "error", err)
		}
		if err := hs.Shutdown(shutdownCtx); err != nil {
			l.Error("Failed to shutdown health check server gracefully", "error", err)
		}
	}()
	return s.Serve(lis)
}

// listenAddress returns the network and address for the given address flag.
func listenAddress(addrFlag string) (string, string) {
	if strings.HasPrefix(addrFlag, "unix://") {
		p := strings.TrimPrefix(addrFlag, "unix://")
		_ = os.Remove(p) // Remove the socket file if it exists.
		return "unix", p
	}
	return "tcp", addrFlag
}

// startMetricsServer starts the HTTP server for Prometheus metrics.
func startMetricsServer(addr string, logger *slog.Logger) (*http.Server, metric.Meter) {
	registry := prometheus.NewRegistry()
	/*
		otelprom 是指 OpenTelemetry（开源可观测性框架）与 Prometheus（开源监控系统）的集成方案。它允许：
		将 OpenTelemetry 收集的指标（Metrics）导出为 Prometheus 兼容格式
		通过 Prometheus 生态（Grafana、Alertmanager）进行指标展示和告警
		统一处理追踪（Traces）、指标（Metrics）和日志（Logs）的采集
	*/
	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		log.Fatal("failed to create metrics exporter")
	}
	// 创建为一个提供者
	provider := metricsdk.NewMeterProvider(metricsdk.WithReader(exporter))
	/*
		MeterProvider 是 OpenTelemetry 中负责管理指标（Metrics）采集、处理和导出的核心组件。它作为指标系统的入口点，主要职责包括：
		创建和管理 Meter 实例（指标工厂）
		配置指标处理器（Processor）和导出器（Exporter）
		控制指标采集周期和批处理策略
	*/
	meter := provider.Meter("envoyproxy/ai-gateway")

	// Create a new HTTP server for metrics.
	mux := http.NewServeMux()

	// Register the metrics handler.
	// 暴露metrics接口
	mux.Handle("/metrics", promhttp.HandlerFor(
		registry,
		promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		},
	))

	// Add a simple health check endpoint.
	// 健康检查接口
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("starting metrics server", "address", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Metrics server failed", "error", err)
		}
	}()

	return server, meter
}

// startHealthCheckServer is a proxy for the gRPC health check server.
// This is necessary because the gRPC health check at k8s level does not
// support unix domain sockets. To make the health check work regardless of
// the network type, we serve a simple HTTP server that checks the gRPC health.
func startHealthCheckServer(addr string, l *slog.Logger, grpcLis net.Listener) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		var prefix string
		switch grpcLis.Addr().Network() {
		case "unix":
			prefix = "unix://"
		default:
			prefix = ""
		}

		conn, err := grpc.NewClient(prefix+grpcLis.Addr().String(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			http.Error(w, fmt.Sprintf("dial failed: %v", err), http.StatusInternalServerError)
			return
		}
		defer conn.Close()

		client := grpc_health_v1.NewHealthClient(conn)
		resp, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: ""})
		if err != nil {
			http.Error(w, fmt.Sprintf("health check RPC failed: %v", err), http.StatusInternalServerError)
			return
		}
		if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
			http.Error(w, fmt.Sprintf("unhealthy status: %s", resp.Status), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		l.Info("Starting health check HTTP server", "addr", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.Error("Health check server failed", "error", err)
		}
	}()
	return server
}

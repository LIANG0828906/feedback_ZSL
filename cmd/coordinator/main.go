package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"settlement/internal/api"
	"settlement/internal/config"
	"settlement/internal/coordinator"
)

func main() {
	configPath := flag.String("config", "configs/coordinator.json", "path to coordinator JSON configuration")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	cfg, err := config.LoadCoordinator(*configPath)
	if err != nil {
		logger.Error("invalid coordinator configuration", "config", *configPath, "error", err)
		os.Exit(2)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	service, err := coordinator.New(rootCtx, cfg, logger)
	if err != nil {
		logger.Error("coordinator startup failed", "error", err)
		os.Exit(1)
	}
	defer service.Close()
	service.Start(rootCtx)

	grpcListener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("gRPC listen failed", "address", cfg.GRPCAddress, "error", err)
		os.Exit(1)
	}
	httpListener, err := net.Listen("tcp", cfg.HTTPAddress)
	if err != nil {
		_ = grpcListener.Close()
		logger.Error("HTTP listen failed", "address", cfg.HTTPAddress, "error", err)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(8*1024*1024),
		grpc.MaxSendMsgSize(8*1024*1024),
	)
	api.RegisterSettlementLockServer(grpcServer, service)
	reflection.Register(grpcServer)
	httpServer := &http.Server{
		Handler:           service.HTTPHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("gRPC coordinator listening", "address", cfg.GRPCAddress)
		if serveErr := grpcServer.Serve(grpcListener); serveErr != nil {
			errCh <- serveErr
		}
	}()
	go func() {
		logger.Info("HTTP status and metrics listening", "address", cfg.HTTPAddress)
		if serveErr := httpServer.Serve(httpListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	select {
	case <-rootCtx.Done():
		logger.Info("coordinator shutdown requested")
	case serveErr := <-errCh:
		logger.Error("coordinator server failed", "error", serveErr)
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		grpcServer.Stop()
	}
	logger.Info("coordinator stopped")
}

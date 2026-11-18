package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"archive/internal/config"
	"archive/internal/httpapi"
	"archive/internal/service"
)

func main() {
	var configPath string
	var listen string
	flag.StringVar(&configPath, "config", "", "path to JSON configuration file")
	flag.StringVar(&listen, "listen", "", "override listen address")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	if listen != "" {
		cfg.ListenAddr = listen
	}
	if cfg.GOMAXPROCS > 0 {
		runtime.GOMAXPROCS(cfg.GOMAXPROCS)
	}
	logger := log.New(os.Stdout, "archive ", log.LstdFlags|log.LUTC)
	ctx := context.Background()
	svc, err := service.New(ctx, cfg, logger)
	if err != nil {
		logger.Fatalf("init: %v", err)
	}
	defer svc.Close()
	svc.Start()

	handler := httpapi.New(svc, logger).Handler()
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      30 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Printf("http listening addr=%s quota=%d concurrency=%d stripe_size=%d", cfg.ListenAddr, cfg.WritebackQuotaBytes, cfg.RepairConcurrency, cfg.StripeSize)
		errCh <- server.ListenAndServe()
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		logger.Printf("signal=%s shutting down", sig)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Printf("http server: %v", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	logger.Printf("stopped")
}

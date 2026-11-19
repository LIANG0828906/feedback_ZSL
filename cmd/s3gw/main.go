package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"s3gw/internal/backend"
	"s3gw/internal/config"
	"s3gw/internal/gateway"
	"s3gw/internal/monitor"
	"s3gw/internal/store"
)

func main() {
	logger := log.New(os.Stdout, "s3gw ", log.LstdFlags|log.Lmicroseconds)
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		logger.Fatal(err)
	}
	if err := os.MkdirAll(cfg.TempDir, 0o755); err != nil {
		logger.Fatalf("create temp directory: %v", err)
	}
	st, err := store.Open(cfg.DataFile)
	if err != nil {
		logger.Fatalf("open SQLite: %v", err)
	}
	defer st.Close()
	_ = st.CleanupOldPending(context.Background(), time.Now().Add(-time.Hour))

	be, err := backend.New(cfg.Backend, cfg.BackendAccessKey, cfg.BackendSecretKey, cfg.Region)
	if err != nil {
		logger.Fatalf("create backend client: %v", err)
	}
	metrics := monitor.New()
	gw := gateway.New(cfg, st, be, metrics, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !cfg.DisableReaper {
		go func() {
			run := func() {
				result := gw.ReapOnce(ctx)
				if result.ExaminedSessions > 0 || result.FailedSessions > 0 {
					logger.Printf("reaper sessions=%d reclaimed=%d failed=%d bytes=%d",
						result.ExaminedSessions, result.ReclaimedSessions, result.FailedSessions, result.ReclaimedBytes)
				}
			}
			run()
			ticker := time.NewTicker(cfg.ReaperInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					run()
				}
			}
		}()
	}

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Printf("listening on %s backend=%s region=%s retention=%s", cfg.Listen, cfg.Backend, cfg.Region, cfg.PartRetention)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("HTTP server: %v", err)
	}
}

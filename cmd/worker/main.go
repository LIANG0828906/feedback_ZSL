package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"settlement/internal/config"
	"settlement/internal/worker"
)

func main() {
	configPath := flag.String("config", "configs/worker-A.json", "path to worker JSON configuration")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	cfg, err := config.LoadWorker(*configPath)
	if err != nil {
		logger.Error("invalid worker configuration", "config", *configPath, "error", err)
		os.Exit(2)
	}
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	w, err := worker.New(rootCtx, cfg, logger)
	if err != nil {
		logger.Error("worker startup failed", "worker_id", cfg.WorkerID, "error", err)
		os.Exit(1)
	}
	defer w.Close()
	if err := w.Run(rootCtx); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("worker stopped", "worker_id", cfg.WorkerID)
			return
		}
		logger.Error("worker failed", "worker_id", cfg.WorkerID, "error", err)
		os.Exit(1)
	}
}

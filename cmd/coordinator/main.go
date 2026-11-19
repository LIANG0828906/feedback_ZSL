package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"quota-coordinator/internal/config"
	"quota-coordinator/internal/coordinator"
	"quota-coordinator/internal/store"
)

func main() {
	configPath := flag.String("config", "config/coordinator.json", "path to coordinator JSON config")
	listen := flag.String("listen", "", "override listen address")
	redisAddr := flag.String("redis", "", "override Redis address")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	if *redisAddr != "" {
		cfg.Redis.Addr = *redisAddr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	st := store.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB, cfg.Redis.Prefix)
	if err := st.Ping(ctx); err != nil {
		cancel()
		log.Fatalf("redis ping: %v", err)
	}
	cancel()

	service, err := coordinator.New(cfg, st)
	if err != nil {
		log.Fatalf("initialize coordinator: %v", err)
	}
	service.Start()

	handler := coordinator.NewHandler(service)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("coordinator listening on %s; redis=%s prefix=%s", cfg.ListenAddr, cfg.Redis.Addr, cfg.Redis.Prefix)
		serverErrors <- server.ListenAndServe()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-signals:
		log.Printf("received %s, shutting down", sig)
	case err := <-serverErrors:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("http server: %v", err)
		}
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	service.Close()
	_ = st.Close()
}

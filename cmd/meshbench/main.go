package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"meshpolicy/internal/gen"
	"meshpolicy/internal/workbench"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "loopback listen address")
	dbPath := flag.String("db", "data/checkpoint.db", "local SQLite checkpoint file")
	seed := flag.Int64("seed", gen.DefaultSeed, "deterministic generator seed")
	flag.Parse()

	svc, err := workbench.NewService(*seed, *dbPath)
	if err != nil {
		log.Fatalf("initialize workbench: %v", err)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           workbench.NewHandler(svc),
		ReadHeaderTimeout: 5 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		log.Printf("mesh policy workbench listening on http://%s (sqlite=%s seed=%d)", *addr, *dbPath, *seed)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	fmt.Println("stopped")
}

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"cadlab/internal/app"
	"cadlab/internal/engine"
	"cadlab/internal/sample"
)

func main() {
	cwd, _ := os.Getwd()
	defaults := engine.DefaultOptions(cwd)
	listen := flag.String("listen", "127.0.0.1:34117", "local HTTP listen address")
	db := flag.String("db", defaults.DBPath, "SQLite checkpoint path")
	seed := flag.Int64("seed", sample.DefaultSeed, "deterministic sample seed")
	flag.Parse()
	opts := defaults
	opts.DBPath = *db
	a, err := app.New(opts, *seed)
	if err != nil {
		log.Fatalf("workbench init: %v", err)
	}
	defer a.Close()

	server := &http.Server{
		Addr:              *listen,
		Handler:           app.NewHTTPHandler(a),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("CADLab Workbench: http://%s\n", *listen)
	fmt.Printf("SQLite checkpoint: %s\n", *db)
	fmt.Printf("Deterministic seed: %d\n", *seed)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	abs, _ := filepath.Abs(*db)
	fmt.Printf("checkpoint closed: %s\n", abs)
}

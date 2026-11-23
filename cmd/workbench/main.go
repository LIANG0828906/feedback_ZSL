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

	"spectral-workbench/pkg/workbench"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "local HTTP listen address")
	dataDir := flag.String("data", "data", "writable persistent data directory")
	flag.Parse()

	absolute, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("resolve data directory: %v", err)
	}
	app, err := workbench.NewServer(absolute)
	if err != nil {
		log.Fatalf("start workbench: %v", err)
	}
	defer app.Close()

	server := &http.Server{
		Addr:              *addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       90 * time.Second,
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		log.Printf("SPECTRA workbench listening at http://%s", *addr)
		log.Printf("deterministic data directory: %s", absolute)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()
	<-stop
	fmt.Println("\nshutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

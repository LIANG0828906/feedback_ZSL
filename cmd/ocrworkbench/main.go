package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ocrworkbench/internal/app"
	"ocrworkbench/internal/web"
)

func main() {
	dataDir := flag.String("data", "data", "deterministic sample directory")
	dbPath := flag.String("db", "workbench.db", "SQLite checkpoint database")
	addr := flag.String("addr", "127.0.0.1:8099", "loopback listen address")
	seed := flag.Int64("seed", 42, "seed used when samples must be generated")
	noAuto := flag.Bool("no-auto-run", false, "do not start the three-path run on startup")
	flag.Parse()

	if err := requireLoopback(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "ocrworkbench:", err)
		os.Exit(2)
	}
	application, err := app.New(app.NewOptions{DataDir: *dataDir, DBPath: *dbPath, Seed: *seed})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ocrworkbench:", err)
		os.Exit(1)
	}
	defer application.Close()
	if !*noAuto {
		if err := application.Start(); err != nil {
			log.Printf("auto run: %v", err)
		}
	}
	handler := web.New(application).Handler()
	server := web.NewHTTPServer(*addr, handler)
	go func() {
		log.Printf("OCR workbench listening on http://%s (loopback only)", *addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	application.Cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing non-loopback listen address %q", addr)
	}
	return nil
}

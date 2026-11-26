package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lip-pipeline-prototype/internal/service"
	"lip-pipeline-prototype/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func usage() {
	log.Print("usage: lipctl serve [--addr 127.0.0.1:8080] [--db artifacts/lip-pipeline.db]")
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	dbPath := fs.String("db", "artifacts/lip-pipeline.db", "SQLite checkpoint path")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := service.New(ctx, *dbPath)
	if err != nil {
		log.Fatalf("initialize service: %v", err)
	}
	defer svc.Close()

	server := &http.Server{
		Addr:              *addr,
		Handler:           web.New(svc).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("lip pipeline console listening on http://%s (db=%s)", *addr, *dbPath)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dnslab/codes"
	"dnslab/selfcheck"
	"dnslab/service"
	"dnslab/workbench"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "diagnostics listen address")
	seed := flag.Int64("seed", 42, "deterministic generator seed")
	dbPath := flag.String("db", ".runtime/dns-checkpoint.sqlite", "sqlite checkpoint path")
	check := flag.Bool("selfcheck", false, "run deterministic acceptance self-check and exit")
	flag.Parse()

	if *check {
		report, err := selfcheck.Run(*seed)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		if err != nil {
			fmt.Fprintln(os.Stderr, "selfcheck failed:", err.Error())
			os.Exit(1)
		}
		return
	}

	svc, err := service.New(*seed, *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize service failed: %v\n", err)
		os.Exit(1)
	}
	defer svc.Close()

	server := &http.Server{
		Addr:              *addr,
		Handler:           workbench.NewServer(svc).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		fmt.Printf("DNS diagnostics workbench: http://%s\n", *addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "server failed: %v\n", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	fmt.Println("\nsignal received; stopping intake and preserving last complete checkpoint...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "graceful shutdown failed: %v\n", err)
	}
	if _, err := svc.SaveCheckpoint(context.Background()); err != nil {
		if biz, ok := codes.As(err); ok && biz.Code == codes.ECheckpointNotConfigured {
			return
		}
		fmt.Fprintf(os.Stderr, "final checkpoint save failed: %v\n", err)
	}
}

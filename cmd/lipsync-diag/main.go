package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"lipsync/diag"
	"lipsync/scenario"
)

func main() {
	defaults := diag.DefaultBuildOptions()
	addr := flag.String("addr", "127.0.0.1:8099", "local loopback listen address")
	seed := flag.Int64("seed", defaults.Seed, "deterministic sample seed")
	dbPath := flag.String("db", defaults.DBPath, "SQLite checkpoint database")
	fresh := flag.Bool("fresh", defaults.Fresh, "remove an existing diagnostic database")
	window := flag.Int("window", defaults.Window, "frames per checkpoint window")
	workers := flag.Int("workers", defaults.MaxWorkers, "maximum channel worker count")
	queue := flag.Int("queue", defaults.QueueBudget, "pending frame task queue budget")
	candidateBuffer := flag.Int("candidate-buffer", defaults.CandidateBuffer, "per-worker candidate buffer cap")
	once := flag.Bool("once", false, "print diagnostics JSON and exit")
	dumpDir := flag.String("dump-dir", "", "write generated v1/v2/v3 documents, truth and loss reports")
	flag.Parse()

	if *fresh {
		_ = os.MkdirAll(filepath.Dir(*dbPath), 0o755)
	}
	options := defaults
	options.Seed = *seed
	options.DBPath = *dbPath
	options.Fresh = *fresh
	options.Window = *window
	options.MaxWorkers = *workers
	options.QueueBudget = *queue
	options.CandidateBuffer = *candidateBuffer

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *dumpDir != "" {
		artifacts, err := scenario.Generate(options.Seed)
		if err != nil {
			log.Fatal(err)
		}
		if err := artifacts.WriteArtifacts(*dumpDir); err != nil {
			log.Fatal(err)
		}
	}
	state, err := diag.BuildState(ctx, options)
	if err != nil {
		log.Fatal(err)
	}
	if *once {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(state); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := requireLoopback(*addr); err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Handler:           diag.NewHandler(state),
		ReadHeaderTimeout: 3 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Printf("lipsync diagnostics: http://%s\n", listener.Addr().String())
	fmt.Printf("seed=%d frames=%d preempt=%d resume=%d abandon=%d truth_match=%v\n",
		state.Seed, len(state.Run.Frames), len(state.Run.Preemptions), len(state.Run.Resumes),
		len(state.Run.Abandonments), state.TruthMatches)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("diagnostic server must bind to a loopback address, got %q", host)
	}
	return nil
}

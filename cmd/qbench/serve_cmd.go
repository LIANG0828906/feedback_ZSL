package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"qualitybench/internal/core"
	"qualitybench/internal/dataset"
	"qualitybench/internal/server"
	"qualitybench/internal/store"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	project := fs.String("project", "samples/project.json", "project JSON")
	truth := fs.String("truth", "samples/truth.json", "truth JSON")
	dbPath := fs.String("db", "artifacts/dashboard.db", "SQLite database")
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	workers := fs.Int("workers", 4, "worker pool size")
	delayMS := fs.Int("delay-ms", 6, "per-task delay for visible progress")
	validatePaths := fs.Bool("validate-paths", true, "run live/resume/full comparison in background")
	if err := fs.Parse(args); err != nil {
		return core.Schema("args", "%v", err)
	}
	ds, err := dataset.Load(*project, *truth)
	if err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	svc := server.New(ds, st, server.Options{
		Workers: *workers, TaskDelay: time.Duration(*delayMS) * time.Millisecond,
		ValidatePaths: *validatePaths,
	})
	svc.Start()
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = httpServer.Shutdown(context.Background())
	}()
	fmt.Printf("diagnostic page: http://%s/\n", *addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

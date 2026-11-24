package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"economycore/internal/core"
	"economycore/internal/samples"
	"economycore/internal/verify"
	"economycore/internal/workbench"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "local HTTP listen address")
	samplesDir := flag.String("samples", "data/samples", "generated sample directory")
	seed := flag.Uint64("seed", 0x5eedc0de, "sample generation seed")
	verifyOnly := flag.Bool("verify", false, "run three-path and checkpoint verification, then exit")
	workDir := flag.String("workdir", "data/verification", "verification/checkpoint working directory")
	reportPath := flag.String("report", "artifacts/verification-report.json", "verification report output")
	windowSize := flag.Int64("window", 64, "checkpoint window size in logical ticks")
	previewStep := flag.Int64("preview-step", 8, "coarse preview reporting step")
	workers := flag.Int("workers", 3, "parallel window planner workers")
	buffer := flag.Int("buffer", 2, "plans buffered per worker")
	flag.Parse()

	manifest, err := samples.Ensure(*samplesDir, *seed)
	if err != nil {
		log.Fatal(err)
	}
	if *verifyOnly {
		runVerification(*samplesDir, *workDir, *reportPath, manifest, *windowSize, *previewStep, *workers, *buffer)
		return
	}
	server, err := workbench.New(*samplesDir)
	if err != nil {
		log.Fatal(err)
	}
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("workbench listening on http://%s", *addr)
		log.Printf("samples: %s", *samplesDir)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

func runVerification(samplesDir, workDir, reportPath string, manifest *samples.Manifest, windowSize, previewStep int64, workers, buffer int) {
	ctx := context.Background()
	scenes := make([]*core.Scenario, 0, len(manifest.Samples))
	for _, entry := range manifest.Samples {
		scene, err := samples.Load(samplesDir, entry.ID)
		if err != nil {
			log.Fatal(err)
		}
		scenes = append(scenes, scene)
	}
	opts := verify.DefaultOptions(workDir)
	opts.WindowSize = windowSize
	opts.PreviewStep = previewStep
	opts.Workers = workers
	opts.Buffer = buffer
	report, err := verify.Run(ctx, scenes, opts)
	if err != nil {
		log.Fatal(err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	data = append(data, '\n')
	if dir := filepath.Dir(reportPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	if err := os.WriteFile(reportPath, data, 0o644); err != nil {
		log.Fatal(err)
	}
	if _, err := os.Stdout.Write(data); err != nil {
		log.Fatal(err)
	}
	if !report.AllOK {
		fmt.Fprintln(os.Stderr, "verification failed")
		os.Exit(1)
	}
}

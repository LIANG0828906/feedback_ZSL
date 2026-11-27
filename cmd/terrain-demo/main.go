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
	"syscall"
	"time"

	tb "terrainbake.local/terrain"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	cacheDir := flag.String("cache", ".terrain-cache", "atomic checkpoint directory")
	sampleDir := flag.String("samples", "samples", "directory for generated schema samples")
	verifyOnly := flag.Bool("verify-only", false, "run three-path verification and exit")
	parallelism := flag.Int("parallelism", 3, "worker pool size")
	memoryBudget := flag.Int64("memory-budget", 8<<20, "global mesh memory budget in bytes")
	vertexCap := flag.Int("vertex-cap", 4096, "per-thread vertex buffer cap")
	flag.Parse()

	p := tb.NewGenerator(tb.DefaultSeed).Project()
	files, err := tb.WriteSamples(*sampleDir, p)
	if err != nil {
		log.Fatalf("write samples: %v", err)
	}
	log.Printf("samples: v1=%s v2=%s v3=%s", files["v1"], files["v2"], files["v3"])

	cfg := tb.EngineConfig{
		MaxParallelism:       *parallelism,
		MaxVerticesPerThread: *vertexCap,
		MeshMemoryBudget:     *memoryBudget,
	}
	if *verifyOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		report, err := tb.VerifyThreePaths(ctx, p, cfg, tb.DefaultVerificationEdits(), "")
		summary := map[string]any{}
		if report != nil {
			summary["equal"] = report.Equal
			summary["message"] = report.Message
			summary["comparisons"] = report.Comparisons
			paths := make([]map[string]any, 0, len(report.Paths))
			for _, run := range report.Paths {
				paths = append(paths, map[string]any{
					"name": run.Name, "fingerprint": run.Fingerprint,
					"commit_sequence": run.CommitSequence,
					"written_count":   run.WrittenCount, "reused_count": run.ReusedCount,
				})
			}
			summary["paths"] = paths
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(summary)
		if err != nil {
			log.Fatalf("three-path verification: %v", err)
		}
		return
	}

	demo, err := tb.NewDemo(*cacheDir, cfg)
	if err != nil {
		log.Fatalf("initialize demo: %v", err)
	}
	srv, err := tb.NewServer(demo)
	if err != nil {
		log.Fatalf("initialize server: %v", err)
	}
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       25 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()
	fmt.Printf("TerrainBake demo listening on http://%s\n", *addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

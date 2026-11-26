package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"maskqc"
	"maskqc/web"
)

func main() {
	if len(os.Args) == 1 {
		if err := serveCommand("127.0.0.1:8080", "artifacts", "artifacts/maskqc-workbench.db"); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
		workdir := fs.String("workdir", "artifacts", "directory for generated verification databases")
		db := fs.String("db", "artifacts/maskqc-workbench.db", "SQLite workbench database")
		parallel := fs.Int("parallel", 2, "maximum tile worker parallelism")
		pixelCap := fs.Int("pixel-buffer-cap", 4096, "maximum pixels per worker buffer")
		queue := fs.Int("queue-budget", 8, "maximum outstanding/queued tile budget")
		_ = fs.Parse(os.Args[2:])
		if err := serveCommand(*addr, *workdir, *db, maskqc.BudgetConfig{MaxParallel: *parallel, MaxPixelsPerWorker: *pixelCap, MaxQueuedTiles: *queue}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		workdir := fs.String("workdir", filepath.Join("artifacts", "verify"), "directory for SQLite checkpoints and JSON report")
		seed := fs.Int64("seed", maskqc.DefaultSeed, "deterministic sample seed")
		crashAfter := fs.Int("crash-after", 1, "committed strokes before simulated termination")
		_ = fs.Parse(os.Args[2:])
		if err := verifyCommand(*workdir, *seed, *crashAfter); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println("maskqc", maskqc.AlgorithmVersion)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; use verify, serve, or version\n", os.Args[1])
		os.Exit(2)
	}
}

func serveCommand(addr, workdir, dbPath string, budgets ...maskqc.BudgetConfig) error {
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	budget := maskqc.DefaultBudgetConfig()
	if len(budgets) > 0 {
		budget = budgets[0]
	}
	server, err := web.NewServer(workdir, dbPath, budget)
	if err != nil {
		return err
	}
	return server.ListenAndServe(addr)
}

func verifyCommand(workdir string, seed int64, crashAfter int) error {
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := maskqc.RunThreePaths(ctx, maskqc.ThreePathOptions{
		WorkDir: workdir, Seed: seed, CrashAfter: crashAfter,
		Budget: maskqc.BudgetConfig{MaxParallel: 2, MaxPixelsPerWorker: 4096, MaxQueuedTiles: 4},
	})
	outPath := filepath.Join(workdir, "three-path-report.json")
	var payload []byte
	if report != nil {
		payload, _ = json.MarshalIndent(report, "", "  ")
	}
	if writeErr := os.WriteFile(outPath, payload, 0o644); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return fmt.Errorf("three-path verification failed; report=%s: %w", outPath, err)
	}

	full := report.Full.Icons[maskqc.HarnessIconID]
	fmt.Println("PASS: realtime incremental, checkpoint recovery, and full recompute match")
	fmt.Printf("report: %s\n", outPath)
	fmt.Printf("mask_sha256: %s\n", full.Accumulated.Hash())
	fmt.Printf("domains: %d; event_sequence: %d; tile_sequence: stable\n", len(full.Analysis.Domains), len(report.Full.EventSequence))
	if topoErr := full.Topology.Error(); topoErr != nil {
		if de, ok := maskqc.AsError(topoErr); ok {
			fmt.Printf("topology diagnostics: %s (%d issues)\n", de.Code, len(full.Topology.Issues))
		}
	} else {
		fmt.Println("topology: within declared tolerances")
	}
	return nil
}

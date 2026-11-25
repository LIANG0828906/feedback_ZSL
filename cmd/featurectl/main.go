package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"syscall"
	"time"

	"featurekernel/internal/checkpoint"
	"featurekernel/internal/engine"
	"featurekernel/internal/gen"
	"featurekernel/internal/kernel"
	"featurekernel/internal/schema"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "diagnose":
		err = runDiagnose(os.Args[2:])
	case "consume":
		err = runConsume(os.Args[2:])
	case "snapshot":
		err = runSnapshot(os.Args[2:])
	case "version":
		fmt.Println("featurectl", kernel.CanonicalVersion, checkpoint.CheckpointVersionV1)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		var ke *kernel.Error
		if errors.As(err, &ke) {
			data, _ := json.MarshalIndent(ke, "", "  ")
			fmt.Fprintln(os.Stderr, string(data))
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "E_CLI_FAILED: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: featurectl <generate|verify|consume|snapshot|version> [flags]")
}

func runGenerate(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	dataDir := flags.String("data", "data", "data root")
	seed := flags.Int64("seed", gen.DefaultSeed, "fixed random seed")
	batches := flags.Int("batches", gen.DefaultBatchCount, "batch count")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := gen.GenerateTree(*dataDir, *seed, *batches)
	if err != nil {
		return err
	}
	fmt.Printf("generated %d batches under %s with seed %d\n", manifest.BatchCount, *dataDir, manifest.Seed)
	for _, batch := range manifest.Batches {
		fmt.Printf("  %s: %d schemas + truth\n", batch.BatchID, len(batch.SampleFiles))
	}
	return nil
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	dataDir := flags.String("data", "data", "data root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	manifestPath := filepath.Join(*dataDir, "manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return kernel.WrapError(kernel.CodeCheckpointOpen, err, map[string]any{"path": manifestPath})
	}
	var manifest gen.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return kernel.WrapError(kernel.CodeCheckpointCorrupt, err, map[string]any{"path": manifestPath})
	}

	for _, batch := range manifest.Batches {
		projects := map[string]*kernel.Project{}
		results := map[string]*kernel.ResultSet{}
		for _, version := range []string{"v1", "v2", "v3"} {
			relative := batch.SampleFiles[version]
			data, err := os.ReadFile(filepath.Join(*dataDir, relative))
			if err != nil {
				return err
			}
			project, err := schema.Load(data)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", batch.BatchID, version, err)
			}
			projects[version] = project
			result, err := engine.New(project)
			if err != nil {
				return err
			}
			rs, err := result.Run(ctx)
			if err != nil {
				return err
			}
			results[version] = rs
		}
		truthData, err := os.ReadFile(filepath.Join(*dataDir, batch.TruthFile))
		if err != nil {
			return err
		}
		var truth gen.TruthFile
		if err := json.Unmarshal(truthData, &truth); err != nil {
			return err
		}
		if err := kernel.CompareWindowResults(truth.Expected.Windows, results["v1"].Windows, truth.Tolerance); err != nil {
			return fmt.Errorf("%s truth: %w", batch.BatchID, err)
		}
		for _, version := range []string{"v2", "v3"} {
			if err := kernel.CompareWindowResults(results["v1"].Windows, results[version].Windows, truth.Tolerance); err != nil {
				return fmt.Errorf("%s %s vs v1: %w", batch.BatchID, version, err)
			}
			if !reflect.DeepEqual(results["v1"].RecomputedWindows, results[version].RecomputedWindows) {
				return fmt.Errorf("%s %s recomputed sets differ", batch.BatchID, version)
			}
			if !reflect.DeepEqual(results["v1"].Discarded, results[version].Discarded) {
				return fmt.Errorf("%s %s discarded sets differ", batch.BatchID, version)
			}
		}
		if len(truth.Expected.Discarded) != len(results["v1"].Discarded) {
			return fmt.Errorf("%s discard count mismatch truth=%d got=%d", batch.BatchID, len(truth.Expected.Discarded), len(results["v1"].Discarded))
		}
		fmt.Printf("PASS %s: windows=%d recomputed=%v discarded=%d paths=%d\n",
			batch.BatchID, len(results["v1"].Windows), results["v1"].RecomputedWindows,
			len(results["v1"].Discarded), len(results["v1"].PathDecisions))
	}
	fmt.Println("verify complete: schema loading, truth window aggregation and late records match")
	return nil
}

func runConsume(args []string) error {
	flags := flag.NewFlagSet("consume", flag.ContinueOnError)
	dbPath := flags.String("db", filepath.Join("data", "checkpoints", "feature.sqlite"), "sqlite checkpoint path")
	samplePath := flags.String("sample", "", "schema sample JSON path")
	targetSeq := flags.Int64("target", 0, "target sequence; 0 means all")
	batchSize := flags.Int("batch-size", 1, "commit batch size")
	crashPoint := flags.String("crash-point", "", "set after_window_states to hard exit before cursor update")
	termAfterCommit := flags.Int64("term-after-commit", 0, "send SIGTERM after this commit sequence")
	flags.String("seed", "", "unused compatibility flag")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *samplePath == "" {
		return kernel.NewError(kernel.CodeInvalidArgument, "--sample is required", nil)
	}
	data, err := os.ReadFile(*samplePath)
	if err != nil {
		return kernel.WrapError(kernel.CodeCheckpointOpen, err, map[string]any{"path": *samplePath})
	}
	project, err := schema.Load(data)
	if err != nil {
		return err
	}
	ctx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()

	store, err := checkpoint.OpenSQLite(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if *crashPoint == "after_window_states" {
		store.CrashAfterWindowStates = func() {
			fmt.Fprintln(os.Stderr, "CRASH injection: forced termination after window-state writes, before global cursor update")
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
	}
	runner, err := checkpoint.NewRunner(ctx, store, project, "")
	if err != nil {
		return err
	}
	if *termAfterCommit > 0 {
		runner.AfterCommit = func(seq int64) {
			if seq == *termAfterCommit {
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
				<-ctx.Done()
			}
		}
	}
	report, result, err := runner.Run(ctx, *targetSeq, *batchSize)
	if errors.Is(err, context.Canceled) {
		summary := map[string]any{
			"graceful_stop": true,
			"report":        report,
			"signal":        "SIGINT/SIGTERM persists through last complete commit point",
		}
		encoded, _ := json.MarshalIndent(summary, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	if err != nil {
		return err
	}
	summary := map[string]any{"report": report, "windows": len(result.Windows), "discarded": len(result.Discarded)}
	encoded, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(encoded))
	return nil
}

func runSnapshot(args []string) error {
	flags := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	dbPath := flags.String("db", filepath.Join("data", "checkpoints", "feature.sqlite"), "sqlite checkpoint path")
	outPath := flags.String("out", filepath.Join("data", "checkpoints", "snapshot.checkpoint.json"), "snapshot output")
	projectID := flags.String("project-id", "", "project id")
	batchID := flags.String("batch-id", "", "batch id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := checkpoint.OpenSQLite(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	snapshot, err := store.ExportSnapshot(ctx, *projectID, *batchID)
	if err != nil {
		return err
	}
	return checkpoint.WriteSnapshotFile(*outPath, snapshot)
}

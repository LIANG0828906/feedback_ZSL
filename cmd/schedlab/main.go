// Command schedlab is the local, repeatable diagnostic workbench. It uses only
// the in-repository deterministic generator (fixed seed) and never contacts a
// cluster, cloud account or network service.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"schedlab/internal/domain"
	"schedlab/internal/faultinject"
	"schedlab/internal/gen"
	"schedlab/internal/runtime"
	"schedlab/workbench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	outDir := "artifacts/demo"
	if len(os.Args) > 2 {
		outDir = os.Args[2]
	}
	if err := run(cmd, outDir); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: schedlab <all|load|feasibility|score|scale|incremental|checkpoint|faults> [out-dir]")
}

func run(cmd, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	standard, err := gen.BuildBundle("standard")
	if err != nil {
		return err
	}
	large, err := gen.BuildBundle("large")
	if err != nil {
		return err
	}

	switch cmd {
	case "load":
		return writeJSON(filepath.Join(outDir, "load.json"), workbench.LoadReport(standard, large))
	case "feasibility":
		return writeJSON(filepath.Join(outDir, "feasibility.json"), workbench.FeasibilityReport(standard))
	case "score":
		return writeJSON(filepath.Join(outDir, "score.json"), workbench.ScoreReport(standard, large))
	case "scale":
		return writeJSON(filepath.Join(outDir, "scale.json"), workbench.ScaleReport(standard))
	case "incremental":
		return writeJSON(filepath.Join(outDir, "incremental.json"), workbench.IncrementalReport(standard))
	case "checkpoint":
		return workbench.CheckpointReport(standard, filepath.Join(outDir, "checkpoint"))
	case "faults":
		return writeJSON(filepath.Join(outDir, "faults.json"), runFaults(standard, outDir))
	case "all":
		return runAll(standard, large, outDir)
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func runAll(standard, large *gen.Bundle, outDir string) error {
	steps := map[string]any{
		"load":        workbench.LoadReport(standard, large),
		"feasibility": workbench.FeasibilityReport(standard),
		"score":       workbench.ScoreReport(standard, large),
		"scale":       workbench.ScaleReport(standard),
		"incremental": workbench.IncrementalReport(standard),
	}
	if err := writeJSON(filepath.Join(outDir, "report.json"), steps); err != nil {
		return err
	}
	if err := workbench.CheckpointReport(standard, filepath.Join(outDir, "checkpoint")); err != nil {
		return err
	}
	return writeJSON(filepath.Join(outDir, "faults.json"), runFaults(standard, outDir))
}

func runFaults(b *gen.Bundle, outDir string) []faultinject.InjectionResult {
	truth := b.Truth.(*domain.Project)
	var v3 gen.V3Project
	_ = json.Unmarshal(b.V3JSON, &v3)
	results := []faultinject.InjectionResult{
		faultinject.InjectWeightOutOfRange(&v3),
		faultinject.InjectNegativeResource(&v3),
		faultinject.InjectTopoKeyConflict(&v3),
		faultinject.InjectConcurrencyBudget(truth),
		faultinject.InjectBufferBudget(truth),
		faultinject.InjectBackpressure(truth),
	}
	store, err := runtime.OpenStore(filepath.Join(outDir, "fault-store"))
	if err == nil {
		if r, e := faultinject.InjectTruncatedCheckpoint(store, truth); e == nil {
			results = append(results, r)
		}
		store2, _ := runtime.OpenStore(filepath.Join(outDir, "fault-store2"))
		if r, e := faultinject.InjectStateVersionMismatch(store2, truth); e == nil {
			results = append(results, r)
		}
	}
	return results
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", path)
	return nil
}

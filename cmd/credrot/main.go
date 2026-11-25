package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"credrot/internal/anomaly"
	"credrot/internal/bizerr"
	"credrot/internal/checkpoint"
	"credrot/internal/engine"
	"credrot/internal/generator"
	"credrot/internal/hashutil"
	"credrot/internal/project"
	"credrot/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = runGen(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "analyze":
		err = runAnalyze(os.Args[2:])
	case "inject":
		err = runInject(os.Args[2:])
	case "checkpoint-verify":
		err = runCheckpointVerify(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		printError(err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `credrot - deterministic credential rotation project

Usage:
  credrot gen --seed 42 --out ./samples
  credrot verify --seed 42 [--workdir ./verify-work] [--json]
  credrot analyze --project ./samples/project_v3.json --at-tick 6
  credrot inject --project ./samples/project_v3.json --kind duplicate_credential_ref
  credrot checkpoint-verify --path ./rotation.checkpoint --manifest <version>`)
}

func runGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic seed")
	out := fs.String("out", "samples", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := generator.Generate(*seed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	names := make([]string, 0, len(bundle.Files))
	for name := range bundle.Files {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(*out, name), bundle.Files[name], 0o644); err != nil {
			return err
		}
		fmt.Println(filepath.Join(*out, name))
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic seed")
	workDir := fs.String("workdir", "", "temporary verification work directory")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := verify.Run(*seed, *workDir)
	if err != nil {
		if report != nil {
			printReport(report)
		}
		return err
	}
	if *asJSON {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
		return nil
	}
	printReport(report)
	return nil
}

func printReport(report *verify.Report) {
	fmt.Printf("seed=%d manifest=%s\n", report.Seed, report.Manifest)
	ok := true
	for _, check := range report.Checks {
		status := "PASS"
		if !check.OK {
			status = "FAIL"
			ok = false
		}
		fmt.Printf("[%s] %s", status, check.Name)
		if check.Details != "" {
			fmt.Printf(" - %s", check.Details)
		}
		fmt.Println()
	}
	fmt.Printf("parallel_runs=%d backpressure_events=%d reused_cells=%d cache_reuse=%t checkpoint=%t anomalies=%t\n",
		report.ParallelRuns, report.Backpressure, report.ReusedCells, report.CacheReuse, report.CheckpointOK, report.AnomaliesOK)
	if ok {
		fmt.Println("VERIFIED")
	}
}

func runAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	path := fs.String("project", "", "project JSON file")
	atTick := fs.Int64("at-tick", 6, "gate tick")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("--project is required")
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	loaded, err := project.Load(raw)
	if err != nil {
		return err
	}
	p := loaded.Project
	solver, err := engine.NewSolver(p)
	if err != nil {
		return err
	}
	validity, err := solver.ValidityAt(*atTick)
	if err != nil {
		return err
	}
	propagation, err := engine.Propagate(p)
	if err != nil {
		return err
	}
	schedule, err := engine.Schedule(p, propagation)
	if err != nil {
		return err
	}
	runtime := engine.GateRuntime{
		Confirmed:          map[string]bool{},
		Arrived:            map[string]bool{},
		Committed:          map[string]string{},
		RequestedBatch:     map[string]int64{},
		LastCommittedBatch: map[string]int64{},
		WatermarkKnown:     true,
	}
	gates, err := engine.GateAll(p, *atTick, runtime)
	if err != nil {
		return err
	}
	output := map[string]any{
		"source_schema": loaded.SourceSchema,
		"project":       p,
		"validity":      validity,
		"propagation":   propagation,
		"schedule":      schedule,
		"gates":         gates,
	}
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func runInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	path := fs.String("project", "", "project JSON file")
	kind := fs.String("kind", "", "anomaly kind")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *kind == "" {
		return fmt.Errorf("--project and --kind are required")
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	loaded, err := project.Load(raw)
	if err != nil {
		return err
	}
	before, _ := hashutil.JSONSHA256(loaded.Project)
	mutated, err := anomaly.Inject(loaded.Project, anomaly.Kind(*kind))
	if err != nil {
		return err
	}
	err = project.Validate(mutated)
	after, _ := hashutil.JSONSHA256(loaded.Project)
	code := bizerr.Code(err)
	result := map[string]any{
		"anomaly":          *kind,
		"validation_code":  code,
		"error":            errString(err),
		"source_unchanged": before == after,
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(data))
	return nil
}

func runCheckpointVerify(args []string) error {
	fs := flag.NewFlagSet("checkpoint-verify", flag.ContinueOnError)
	path := fs.String("path", "", "checkpoint path")
	manifest := fs.String("manifest", "", "expected manifest version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *manifest == "" {
		return fmt.Errorf("--path and --manifest are required")
	}
	info, err := checkpoint.Verify(*path, *manifest)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(info, "", "  ")
	fmt.Println(string(data))
	return nil
}

func printError(err error) {
	if be, ok := err.(*bizerr.Error); ok {
		data, _ := json.MarshalIndent(be, "", "  ")
		fmt.Fprintln(os.Stderr, string(data))
		return
	}
	fmt.Fprintln(os.Stderr, err)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

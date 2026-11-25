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
	"sort"
	"strings"
	"syscall"

	"dressbatch"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		writeError(err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a command is required")
	}
	switch args[0] {
	case "generate":
		return runGenerate(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "demo":
		return runDemo(args[1:])
	case "serve":
		return runServe(args[1:])
	case "recover":
		return runRecover(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `dressbatch commands:
  generate  -seed 42 -count 4 -out samples
  verify    -dir samples
  demo      -out artifacts/demo -seed 42 -count 4
  serve     -sample samples/sample_00.json -addr 127.0.0.1:8080
  recover   -sample samples/sample_00.json -db artifacts/demo/checkpoints.db -run demo-char-01`)
}

func runGenerate(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := flags.Int64("seed", 42, "deterministic sample seed")
	count := flags.Int("count", 4, "number of character projects")
	out := flags.String("out", "samples", "output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	files, err := dressbatch.WriteSampleSuite(*out, *seed, *count)
	if err != nil {
		return err
	}
	return writeJSON(map[string]any{
		"seed":    *seed,
		"count":   *count,
		"out_dir": *out,
		"files":   files,
	})
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := flags.String("dir", "samples", "sample directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	paths, err := samplePaths(*dir)
	if err != nil {
		return err
	}
	reports := make([]*dressbatch.TruthReport, 0, len(paths))
	for _, path := range paths {
		project, err := dressbatch.LoadProjectFile(path)
		if err != nil {
			return err
		}
		report, err := dressbatch.VerifyTruth(project)
		if err != nil {
			return err
		}
		if !report.Passed {
			return fmt.Errorf("%s: sample truth verification failed", path)
		}
		reports = append(reports, report)
	}
	return writeJSON(map[string]any{
		"passed":  true,
		"samples": len(reports),
		"reports": reports,
	})
}

func runDemo(args []string) error {
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	out := flags.String("out", "artifacts/demo", "demo output directory")
	seed := flags.Int64("seed", 42, "deterministic sample seed")
	count := flags.Int("count", 4, "number of generated characters")
	workers := flags.Int("workers", 4, "maximum worker parallelism")
	buffer := flags.Int("buffer", 3, "per-worker candidate buffer capacity")
	queueBudget := flags.Int("queue-budget", 64, "pending candidate-part budget")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	sampleDir := filepath.Join(*out, "samples")
	if _, err := dressbatch.WriteSampleSuite(sampleDir, *seed, *count); err != nil {
		return err
	}
	project, err := dressbatch.LoadProjectFile(filepath.Join(sampleDir, "sample_00.json"))
	if err != nil {
		return err
	}
	truthReport, err := dressbatch.VerifyTruth(project)
	if err != nil {
		return err
	}
	candidate, err := firstLegalCandidate(project)
	if err != nil {
		return err
	}
	assembly, err := dressbatch.EvaluateAssembly(project, candidate)
	if err != nil {
		return err
	}
	result, err := dressbatch.Pack(project, assembly.AcceptedPartIDs)
	if err != nil {
		return err
	}
	poolConfig := dressbatch.PoolConfig{
		MaxWorkers:               *workers,
		CandidateBufferPerWorker: *buffer,
		QueueBudget:              *queueBudget,
	}
	schedulerReport, err := dressbatch.ValidateBatchesWithPool(context.Background(), project, result.Batches, poolConfig)
	if err != nil {
		return err
	}

	dbPath := filepath.Join(*out, "checkpoints.db")
	if err := removeSQLiteFiles(dbPath); err != nil {
		return err
	}
	run := dressbatch.NewRunRecord("demo-"+project.ID, project)
	store, err := dressbatch.OpenStore(dbPath)
	if err != nil {
		return err
	}
	if err := store.CommitResult(context.Background(), run, project, result); err != nil {
		store.Close()
		return err
	}
	if err := store.Close(); err != nil {
		return err
	}

	restarted, err := dressbatch.OpenStore(dbPath)
	if err != nil {
		return err
	}
	recovery, err := restarted.RecoverRun(context.Background(), run.RunID, project)
	if err != nil {
		restarted.Close()
		return err
	}
	if recovery.Truncated {
		restarted.Close()
		return fmt.Errorf("clean restart unexpectedly truncated checkpoints")
	}
	if err := compareRecovered(result, recovery); err != nil {
		restarted.Close()
		return err
	}

	mutated, targetPart, err := mutateLayoutOnly(project, result)
	if err != nil {
		restarted.Close()
		return err
	}
	incremental, err := dressbatch.IncrementalRepack(dressbatch.IncrementalRequest{
		OldProject:       project,
		NewProject:       mutated,
		Previous:         result,
		CandidatePartIDs: assembly.AcceptedPartIDs,
		ReusedPrefix:     recovery.ValidBatches,
	})
	if err != nil {
		restarted.Close()
		return err
	}
	if err := restarted.Close(); err != nil {
		return err
	}
	incrementalRun := dressbatch.NewRunRecord("demo-incremental-"+project.ID, mutated)
	replacementStore, err := dressbatch.OpenStore(dbPath)
	if err != nil {
		return err
	}
	if err := replacementStore.ReplacePlan(context.Background(), incrementalRun, mutated, incremental.Result.Batches); err != nil {
		replacementStore.Close()
		return err
	}
	if err := replacementStore.Close(); err != nil {
		return err
	}
	verificationStore, err := dressbatch.OpenStore(dbPath)
	if err != nil {
		return err
	}
	incrementalRecovery, err := verificationStore.RecoverRun(context.Background(), incrementalRun.RunID, mutated)
	if err != nil {
		verificationStore.Close()
		return err
	}
	if err := compareRecovered(incremental.Result, incrementalRecovery); err != nil {
		verificationStore.Close()
		return err
	}
	if err := verificationStore.Close(); err != nil {
		return err
	}

	report := map[string]any{
		"passed":                          true,
		"sample_truth":                    truthReport,
		"initial_batches":                 result.Batches,
		"commit_sequence":                 result.CommitSequence,
		"recovered_batch_ids":             recovery.ValidBatchIDs,
		"scheduler":                       schedulerReport,
		"layout_edit_part":                targetPart,
		"invalidation":                    incremental.Invalidation,
		"incremental_batches":             incremental.Result.Batches,
		"incremental_recovered_batch_ids": incrementalRecovery.ValidBatchIDs,
		"database":                        dbPath,
	}
	reportPath := filepath.Join(*out, "verification.json")
	if err := writeJSONFile(reportPath, report); err != nil {
		return err
	}
	fmt.Printf("demo passed: truth=%d cases, batches=%d, recovered=%d, invalidated=%d, db=%s, report=%s\n",
		len(truthReport.Cases), len(result.Batches), recovery.ValidPrefix, len(incremental.Invalidation.Entries), dbPath, reportPath)
	return nil
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	sample := flags.String("sample", "samples/sample_00.json", "sample project path")
	addr := flags.String("addr", "127.0.0.1:8080", "local listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	project, err := dressbatch.LoadProjectFile(*sample)
	if err != nil {
		return err
	}
	candidate, err := firstLegalCandidate(project)
	if err != nil {
		return err
	}
	result, err := dressbatch.Pack(project, candidate)
	if err != nil {
		return err
	}
	state, err := dressbatch.BuildDiagnosticState(project, result)
	if err != nil {
		return err
	}
	truth, err := dressbatch.VerifyTruth(project)
	if err != nil {
		return err
	}
	state.Truth = truth
	mutated, _, err := mutateLayoutOnly(project, result)
	if err != nil {
		return err
	}
	incremental, err := dressbatch.IncrementalRepack(dressbatch.IncrementalRequest{
		OldProject:       project,
		NewProject:       mutated,
		Previous:         result,
		CandidatePartIDs: candidate,
	})
	if err != nil {
		return err
	}
	state.Result = incremental.Result
	state.Invalidation = &incremental.Invalidation
	state.DependencyGraph = dressbatch.BuildDependencyGraph(mutated, incremental.Result)
	scheduler, err := dressbatch.ValidateBatchesWithPool(context.Background(), project, result.Batches, dressbatch.PoolConfig{
		MaxWorkers:               4,
		CandidateBufferPerWorker: 3,
		QueueBudget:              64,
	})
	if err != nil {
		return err
	}
	state.Scheduler = scheduler

	if _, err := fmt.Fprintf(os.Stderr, "diagnostics listening on http://%s\n", *addr); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dressbatch.ServeDiagnostics(ctx, *addr, state)
}

func runRecover(args []string) error {
	flags := flag.NewFlagSet("recover", flag.ContinueOnError)
	sample := flags.String("sample", "samples/sample_00.json", "sample project path")
	dbPath := flags.String("db", "artifacts/demo/checkpoints.db", "sqlite database path")
	runID := flags.String("run", "", "run id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *runID == "" {
		return fmt.Errorf("-run is required")
	}
	project, err := dressbatch.LoadProjectFile(*sample)
	if err != nil {
		return err
	}
	store, err := dressbatch.OpenStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	report, err := store.RecoverRun(context.Background(), *runID, project)
	if err != nil {
		return err
	}
	return writeJSON(report)
}

func firstLegalCandidate(project *dressbatch.Project) ([]string, error) {
	for _, sample := range project.Truth.Assemblies {
		if sample.ExpectedLegal {
			return append([]string(nil), sample.CandidatePartIDs...), nil
		}
	}
	return nil, fmt.Errorf("project has no legal truth assembly")
}

func mutateLayoutOnly(project *dressbatch.Project, result *dressbatch.PackResult) (*dressbatch.Project, string, error) {
	if len(result.Batches) == 0 {
		return nil, "", fmt.Errorf("cannot mutate an empty pack result")
	}
	last := result.Batches[len(result.Batches)-1]
	targetPart := ""
	for _, partID := range last.PartIDs {
		targetPart = partID
	}
	if targetPart == "" {
		return nil, "", fmt.Errorf("last batch has no part")
	}
	data, err := json.Marshal(project)
	if err != nil {
		return nil, "", err
	}
	var clone dressbatch.Project
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, "", err
	}
	for i := range clone.Parts {
		if clone.Parts[i].ID != targetPart {
			continue
		}
		for j := range clone.Parts[i].Submeshes {
			submesh := &clone.Parts[i].Submeshes[j]
			if submesh.VertexCount > 2 {
				submesh.VertexCount--
				if submesh.IndexCount > 3 {
					submesh.IndexCount -= 3
				}
				return &clone, targetPart, nil
			}
		}
	}
	return nil, "", fmt.Errorf("could not apply a layout-only mutation to part %s", targetPart)
}

func compareRecovered(result *dressbatch.PackResult, recovery *dressbatch.RecoveryReport) error {
	if recovery.ValidPrefix != len(result.Batches) {
		return fmt.Errorf("recovered prefix %d does not match batch count %d", recovery.ValidPrefix, len(result.Batches))
	}
	copyResult := *result
	copyResult.Batches = recovery.ValidBatches
	copyResult.CommitSequence = append([]string(nil), recovery.ValidBatchIDs...)
	return dressbatch.ComparePackResults(result, &copyResult)
}

func samplePaths(dir string) ([]string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{dir}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, "sample_") && strings.HasSuffix(name, ".json") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no sample_*.json files in %s", dir)
	}
	return paths, nil
}

func removeSQLiteFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func writeError(err error) {
	payload := map[string]any{"code": "E_RUNTIME", "message": err.Error()}
	if domainErr, ok := dressbatch.AsDomainError(err); ok {
		payload = map[string]any{
			"code":    domainErr.Code,
			"message": domainErr.Message,
		}
		if domainErr.ObjectID != "" {
			payload["object_id"] = domainErr.ObjectID
		}
		if domainErr.Field != "" {
			payload["field"] = domainErr.Field
		}
		if domainErr.Details != nil {
			payload["details"] = domainErr.Details
		}
	}
	encoded, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Fprintln(os.Stderr, string(encoded))
}

func writeJSON(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func writeJSONFile(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

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
	"syscall"

	orchestrator "example.com/isolation-orchestrator"
)

type verificationReport struct {
	Seed                   int64    `json:"seed"`
	SchemasEquivalent      bool     `json:"schemas_equivalent"`
	TruthMatch             bool     `json:"truth_match"`
	CrossSchemaIdentical   bool     `json:"cross_schema_identical"`
	ConcurrencyInvariant   bool     `json:"concurrency_invariant"`
	BackpressureObserved   bool     `json:"backpressure_observed"`
	IncrementalMatchesFull bool     `json:"incremental_matches_full"`
	SnapshotCacheDropped   bool     `json:"snapshot_cache_dropped"`
	CrashRollbackHeld      bool     `json:"crash_rollback_held"`
	ResumeMatchesFull      bool     `json:"resume_matches_full"`
	CompletedTicks         int      `json:"completed_ticks"`
	IncrementalSources     []string `json:"incremental_sources"`
	IncrementalRecomputed  int      `json:"incremental_recomputed_ticks"`
	IncrementalReused      int      `json:"incremental_reused_ticks"`
	CrashCommittedTick     int64    `json:"crash_last_committed_tick"`
	FinalCheckpointTick    int64    `json:"final_checkpoint_tick"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = commandGenerate(os.Args[2:])
	case "run":
		err = commandRun(os.Args[2:])
	case "verify":
		err = commandVerify(os.Args[2:])
	case "execute":
		err = commandExecute(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  orchestrator gen    [-seed N] [-out DIR]
  orchestrator run    -input FILE [-out FILE] [-workers N] [-domain-cap N]
  orchestrator verify [-seed N] [-out FILE]
  orchestrator execute -input FILE -db FILE [-resume] [-out FILE]`)
}

func commandGenerate(args []string) error {
	flags := flag.NewFlagSet("gen", flag.ContinueOnError)
	seed := flags.Int64("seed", orchestrator.DefaultSeed, "deterministic generator seed")
	out := flags.String("out", "samples", "output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	scenario, err := orchestrator.GenerateScenario(*seed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"cluster-v1.json": scenario.V1,
		"cluster-v2.json": scenario.V2,
		"cluster-v3.json": scenario.V3,
	}
	canonical, err := json.MarshalIndent(scenario.Canonical, "", "  ")
	if err != nil {
		return err
	}
	truth, err := json.MarshalIndent(scenario.Truth, "", "  ")
	if err != nil {
		return err
	}
	files["canonical.json"] = canonical
	files["truth.json"] = truth
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(*out, name), append(content, '\n'), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("generated seed=%d files=%d dir=%s\n", *seed, len(files), *out)
	return nil
}

func commandRun(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	input := flags.String("input", "", "cluster project JSON")
	out := flags.String("out", "", "optional result JSON path")
	workers := flags.Int("workers", 0, "worker budget override")
	domainCap := flags.Int("domain-cap", 0, "per-domain pending limit override")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		return errors.New("-input is required")
	}
	data, err := os.ReadFile(*input)
	if err != nil {
		return err
	}
	project, err := orchestrator.LoadProject(data)
	if err != nil {
		return err
	}
	options := orchestrator.SolveOptions{}
	if *workers > 0 {
		options.WorkerBudget = *workers
	}
	if *domainCap > 0 {
		options.PerDomainPendingLimit = *domainCap
	}
	solver, err := orchestrator.NewSolver(project, options)
	if err != nil {
		return err
	}
	result, err := solver.Solve()
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if *out != "" {
		return os.WriteFile(*out, encoded, 0o644)
	}
	_, err = os.Stdout.Write(encoded)
	return err
}

func commandVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := flags.Int64("seed", orchestrator.DefaultSeed, "deterministic generator seed")
	out := flags.String("out", "", "optional verification report path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	report, err := verify(*seed)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*out, encoded, 0o644); err != nil {
			return err
		}
	}
	_, err = os.Stdout.Write(encoded)
	return err
}

func commandExecute(args []string) error {
	flags := flag.NewFlagSet("execute", flag.ContinueOnError)
	input := flags.String("input", "", "cluster project JSON")
	database := flags.String("db", "", "SQLite checkpoint path")
	resume := flags.Bool("resume", false, "resume from the latest complete SQLite checkpoint")
	out := flags.String("out", "", "optional run summary JSON path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || *database == "" {
		return errors.New("-input and -db are required")
	}
	data, err := os.ReadFile(*input)
	if err != nil {
		return err
	}
	project, err := orchestrator.LoadProject(data)
	if err != nil {
		return err
	}
	store, err := orchestrator.OpenSQLiteStore(*database)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	executor, err := orchestrator.NewExecutor(ctx, project, store, orchestrator.ExecutorOptions{Resume: *resume})
	if err != nil {
		return err
	}
	summary, runErr := executor.Run(ctx)
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, encoded, 0o644); err != nil {
			return err
		}
	} else {
		_, _ = os.Stdout.Write(encoded)
	}
	if runErr != nil {
		var interrupted *orchestrator.InterruptedError
		if errors.As(runErr, &interrupted) {
			return nil
		}
		return runErr
	}
	return nil
}

func verify(seed int64) (verificationReport, error) {
	report := verificationReport{Seed: seed}
	scenario, err := orchestrator.GenerateScenario(seed)
	if err != nil {
		return report, err
	}
	projects := make([]*orchestrator.Project, 0, 3)
	for _, document := range [][]byte{scenario.V1, scenario.V2, scenario.V3} {
		project, err := orchestrator.LoadProject(document)
		if err != nil {
			return report, err
		}
		projects = append(projects, project)
	}
	report.SchemasEquivalent = true
	for _, project := range projects[1:] {
		if !orchestrator.EquivalentProjects(projects[0], project) {
			report.SchemasEquivalent = false
		}
	}

	var baseline *orchestrator.RunResult
	report.CrossSchemaIdentical = true
	for index, project := range projects {
		solver, err := orchestrator.NewSolver(project)
		if err != nil {
			return report, err
		}
		result, err := solver.Solve()
		if err != nil {
			return report, err
		}
		if index == 0 {
			baseline = result
			report.CompletedTicks = len(result.Ticks)
			if err := orchestrator.VerifyTruth(result, scenario.Truth); err != nil {
				return report, err
			}
			report.TruthMatch = true
		} else if !orchestrator.SemanticRunsEqual(baseline, result) {
			report.CrossSchemaIdentical = false
		}
	}

	report.ConcurrencyInvariant = true
	for _, config := range []orchestrator.SolveOptions{
		{WorkerBudget: 1, PerDomainPendingLimit: 1},
		{WorkerBudget: 3, PerDomainPendingLimit: 2},
		{WorkerBudget: 16, PerDomainPendingLimit: 128},
	} {
		solver, err := orchestrator.NewSolver(projects[0], config)
		if err != nil {
			return report, err
		}
		result, err := solver.Solve()
		if err != nil {
			return report, err
		}
		for _, tick := range result.Ticks {
			if len(tick.Backpressure) > 0 {
				report.BackpressureObserved = true
			}
		}
		if !orchestrator.SemanticRunsEqual(baseline, result) {
			report.ConcurrencyInvariant = false
		}
	}

	engine, err := orchestrator.NewIncrementalEngine(projects[0])
	if err != nil {
		return report, err
	}
	incrementalReport, err := engine.Apply(orchestrator.Edit{
		Kind:         orchestrator.EditSetDependencies,
		NodeID:       "b",
		Dependencies: nil,
	})
	if err != nil {
		return report, err
	}
	report.IncrementalSources = incrementalReport.InvalidatedBy
	report.IncrementalRecomputed = len(incrementalReport.RecomputedTicks)
	report.IncrementalReused = len(incrementalReport.ReusedTicks)
	incrementalResult, err := engine.RunResult()
	if err != nil {
		return report, err
	}
	modifiedSolver, err := orchestrator.NewSolver(engine.Project())
	if err != nil {
		return report, err
	}
	modifiedFull, err := modifiedSolver.Solve()
	if err != nil {
		return report, err
	}
	report.IncrementalMatchesFull = orchestrator.SemanticRunsEqual(modifiedFull, incrementalResult)

	replacement, err := orchestrator.GenerateScenario(seed + 1)
	if err != nil {
		return report, err
	}
	replacementReport, err := engine.ReplaceSnapshot(replacement.Canonical)
	if err != nil {
		return report, err
	}
	report.SnapshotCacheDropped = replacementReport.SnapshotChanged && len(replacementReport.RecomputedTicks) > 0

	tempDir, err := os.MkdirTemp("", "isolation-orchestrator-*")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(tempDir)
	path := filepath.Join(tempDir, "checkpoint.db")
	store, err := orchestrator.OpenSQLiteStore(path, orchestrator.WithFailpoint(func(phase string, tick int64) error {
		if phase == "before_global_tick" && tick == 15 {
			return errors.New("simulated crash before global tick")
		}
		return nil
	}))
	if err != nil {
		return report, err
	}
	executor, err := orchestrator.NewExecutor(context.Background(), projects[0], store, orchestrator.ExecutorOptions{})
	if err != nil {
		return report, err
	}
	first, firstErr := executor.Run(context.Background())
	if orchestrator.CodeOf(firstErr) != orchestrator.CodeCheckpoint {
		return report, fmt.Errorf("expected injected checkpoint failure, got %v", firstErr)
	}
	report.CrashCommittedTick = first.LastCommittedTick
	checkpoint, ok, err := store.LatestCheckpoint(context.Background())
	if err != nil {
		return report, err
	}
	report.CrashRollbackHeld = ok && checkpoint.Tick == first.LastCommittedTick
	if err := store.Close(); err != nil {
		return report, err
	}
	reopened, err := orchestrator.OpenSQLiteStore(path)
	if err != nil {
		return report, err
	}
	resumed, err := orchestrator.NewExecutor(context.Background(), projects[0], reopened, orchestrator.ExecutorOptions{Resume: true})
	if err != nil {
		reopened.Close()
		return report, err
	}
	rest, err := resumed.Run(context.Background())
	reopened.Close()
	if err != nil {
		return report, err
	}
	combinedTicks := append(append([]orchestrator.TickResult(nil), first.Ticks...), rest.Ticks...)
	combined := &orchestrator.RunResult{Ticks: combinedTicks, Final: baseline.Final}
	report.ResumeMatchesFull = orchestrator.SemanticRunsEqual(baseline, combined)
	finalStore, err := orchestrator.OpenSQLiteStore(path)
	if err != nil {
		return report, err
	}
	finalCheckpoint, ok, err := finalStore.LatestCheckpoint(context.Background())
	finalStore.Close()
	if err != nil {
		return report, err
	}
	if ok {
		report.FinalCheckpointTick = finalCheckpoint.Tick
	}
	if !report.SchemasEquivalent || !report.TruthMatch || !report.CrossSchemaIdentical ||
		!report.ConcurrencyInvariant || !report.IncrementalMatchesFull || !report.SnapshotCacheDropped ||
		!report.CrashRollbackHeld || !report.ResumeMatchesFull {
		return report, errors.New("verification failed; see report")
	}
	return report, nil
}

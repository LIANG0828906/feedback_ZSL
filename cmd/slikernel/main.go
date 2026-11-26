package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"time"

	sli "slikernel.local/core"
)

type check struct {
	Name    string   `json:"name"`
	Passed  bool     `json:"passed"`
	Details string   `json:"details,omitempty"`
	Codes   []string `json:"codes,omitempty"`
}

type verification struct {
	GeneratedAt string              `json:"generated_at"`
	Seed        int64               `json:"seed"`
	Module      string              `json:"module"`
	Checks      []check             `json:"checks"`
	Summary     map[string]any      `json:"summary"`
	Exceptions  map[string]string   `json:"exception_codes"`
	Signal      *signalVerification `json:"signal,omitempty"`
}

type signalVerification struct {
	GracefulReportExists   bool   `json:"graceful_report_exists"`
	GracefulExitCode       int    `json:"graceful_exit_code"`
	ForcedReportExists     bool   `json:"forced_report_exists"`
	ForcedExitCode         int    `json:"forced_exit_code"`
	CheckpointAfterSIGKILL bool   `json:"checkpoint_after_sigkill_available"`
	Detail                 string `json:"detail"`
}

func main() {
	mode := flag.String("mode", "demo", "demo | crash-demo | child-graceful | child-kill")
	seed := flag.Int64("seed", 20261127, "deterministic generator seed")
	out := flag.String("out", "artifacts", "artifact directory")
	dbPath := flag.String("db", "", "checkpoint database for child modes")
	readyPath := flag.String("ready", "", "ready marker for child modes")
	reportPath := flag.String("report", "", "exit report path for child modes")
	flag.Parse()

	var err error
	switch *mode {
	case "demo":
		err = runDemo(*out, *seed)
	case "crash-demo":
		err = runCrashDemo(*out, *seed)
	case "child-graceful", "child-kill":
		err = runChild(*mode, *seed, *dbPath, *readyPath, *reportPath)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runDemo(out string, seed int64) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	fixture, err := sli.GenerateFixture(seed)
	if err != nil {
		return err
	}
	proof := verification{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Seed:        seed, Module: "slikernel.local/core",
		Summary: map[string]any{}, Exceptions: map[string]string{},
	}

	projects := make([]sli.Project, 4)
	for version := 1; version <= 3; version++ {
		payload, err := sli.EncodeProject(fixture.Project, version)
		if err != nil {
			return fmt.Errorf("encode v%d: %w", version, err)
		}
		if err := writeAtomic(filepath.Join(out, fmt.Sprintf("project-v%d.json", version)), payload); err != nil {
			return err
		}
		projects[version], err = sli.LoadProject(payload)
		if err != nil {
			return fmt.Errorf("load v%d: %w", version, err)
		}
	}
	canonical := sli.CloneProject(fixture.Project)
	canonical.SourceVersion = 0
	schemaParity := true
	for version := 1; version <= 3; version++ {
		loaded := sli.CloneProject(projects[version])
		loaded.SourceVersion = 0
		if !reflect.DeepEqual(canonical, loaded) {
			schemaParity = false
		}
	}
	proof.Checks = append(proof.Checks, check{
		Name: "three_schema_canonical_parity", Passed: schemaParity,
		Details: "services, SLI order and observation sequence are identical",
	})
	proof.Summary["services"] = len(fixture.Project.Services)
	proof.Summary["observations"] = len(fixture.Project.Observations)
	proof.Summary["sli_definitions"] = countSLIs(fixture.Project)

	options := sli.Options{Workers: 4, PerWorkerBuffer: 16, SubmissionOrder: "forward"}
	engine, err := sli.NewEngine(fixture.Project, options)
	if err != nil {
		return err
	}
	report, err := engine.ComputeAll(context.Background())
	if err != nil {
		return err
	}
	truthMatch, detail := reportsCoreEqual(&fixture.Truth, report)
	proof.Checks = append(proof.Checks, check{
		Name: "window_sli_budget_alert_truth", Passed: truthMatch, Details: detail,
	})
	proof.Summary["evaluation_windows"] = len(report.Results)
	proof.Summary["window_errors_with_exhausted_budget"] = countExhausted(report)
	proof.Summary["window_alerts"] = countAlerts(report)
	if err := writeJSON(filepath.Join(out, "report.json"), report); err != nil {
		return err
	}

	target := int64(990_000)
	incremental, err := engine.ApplyEdits(context.Background(), []sli.Edit{{
		ID: "demo-target", Kind: sli.EditSetSLOTarget, Service: "checkout",
		SLI: "checkout-availability", TargetPPM: &target,
	}})
	if err != nil {
		return err
	}
	fullEngine, err := sli.NewEngine(engine.Project(), options)
	if err != nil {
		return err
	}
	fullReport, err := fullEngine.ComputeAll(context.Background())
	if err != nil {
		return err
	}
	incrementalMatch, incrementalDetail := reportsCoreEqual(fullReport, incremental.Report)
	proof.Checks = append(proof.Checks, check{
		Name: "incremental_target_equals_full", Passed: incrementalMatch, Details: incrementalDetail,
	})
	proof.Summary["incremental_recomputed_windows"] = len(incremental.RecomputedEval)
	proof.Summary["incremental_reused_windows"] = len(incremental.ReusedEval)
	proof.Summary["incremental_reused_base_windows"] = incremental.ReusedBaseWindows
	if err := writeJSON(filepath.Join(out, "incremental.json"), incremental); err != nil {
		return err
	}

	variants := []sli.Options{
		{Workers: 1, PerWorkerBuffer: 1, BatchSize: 1, SubmissionOrder: "forward"},
		{Workers: 8, PerWorkerBuffer: 4, BatchSize: 2, SubmissionOrder: "reverse"},
		{Workers: 4, PerWorkerBuffer: 8, SubmissionOrder: "shuffle"},
	}
	concurrentMatch := true
	var backpressureSignals int
	for _, variant := range variants {
		engineVariant, err := sli.NewEngine(fixture.Project, variant)
		if err != nil {
			return err
		}
		variantReport, err := engineVariant.ComputeAll(context.Background())
		if err != nil {
			return err
		}
		equal, _ := reportsCoreEqual(report, variantReport)
		concurrentMatch = concurrentMatch && equal
		backpressureSignals += len(variantReport.Diagnostics.Backpressure)
	}
	proof.Checks = append(proof.Checks, check{
		Name: "concurrency_batching_order_determinism", Passed: concurrentMatch,
		Details: "1/4/8 workers, reverse and deterministic shuffle all match",
	})
	proof.Summary["backpressure_signals"] = backpressureSignals

	checkpointPath := filepath.Join(out, "demo-checkpoint.db")
	_ = sli.RemoveCheckpointFiles(checkpointPath)
	store, err := sli.OpenCheckpoint(checkpointPath)
	if err != nil {
		return err
	}
	meta, err := engine.SaveCheckpoint(context.Background(), store)
	if err != nil {
		_ = store.Close()
		return err
	}
	currentReport, err := engine.CachedReport()
	if err != nil {
		_ = store.Close()
		return err
	}
	snapshot, err := store.Load(context.Background(), engine.RuleVersion())
	if err != nil {
		_ = store.Close()
		return err
	}
	snapshotMatch, snapshotDetail := reportsCoreEqual(currentReport, snapshot.Report)
	proof.Checks = append(proof.Checks, check{
		Name: "sqlite_checkpoint_load", Passed: snapshotMatch, Details: snapshotDetail,
	})
	proof.Summary["checkpoint_committed_seq"] = meta.CommittedSeq
	proof.Summary["checkpoint_base_windows"] = meta.BaseCount
	proof.Summary["checkpoint_eval_windows"] = meta.WindowCount
	_ = store.Close()

	exceptions, err := collectExceptionCodes(fixture.Project)
	if err != nil {
		return err
	}
	proof.Exceptions = exceptions
	proof.Checks = append(proof.Checks, check{
		Name: "stable_exception_codes", Passed: len(exceptions) >= 8,
		Codes: sortedKeys(exceptions),
	})

	allPassed := true
	for _, item := range proof.Checks {
		allPassed = allPassed && item.Passed
	}
	proof.Summary["all_checks_passed"] = allPassed
	if err := writeJSON(filepath.Join(out, "verification.json"), proof); err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(proof, "", "  ")
	fmt.Println(string(encoded))
	if !allPassed {
		return errors.New("one or more verification checks failed")
	}
	return nil
}

func runCrashDemo(out string, seed int64) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	workDir := filepath.Join(out, "signal-demo")
	if err := os.RemoveAll(workDir); err != nil {
		return err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	gracefulDB := filepath.Join(workDir, "graceful.db")
	gracefulReady := filepath.Join(workDir, "graceful.ready")
	gracefulReport := filepath.Join(workDir, "graceful-report.json")
	gracefulExit, gracefulOutput, err := runSignalChild(executable, "child-graceful", seed,
		gracefulDB, gracefulReady, gracefulReport, syscall.SIGTERM)
	if err != nil {
		return fmt.Errorf("graceful child: %w: %s", err, gracefulOutput)
	}
	forcedDB := filepath.Join(workDir, "forced.db")
	forcedReady := filepath.Join(workDir, "forced.ready")
	forcedReport := filepath.Join(workDir, "forced-report.json")
	forcedExit, forcedOutput, err := runSignalChild(executable, "child-kill", seed,
		forcedDB, forcedReady, forcedReport, syscall.SIGKILL)
	if err != nil {
		return fmt.Errorf("forced child: %w: %s", err, forcedOutput)
	}
	_, gracefulErr := os.Stat(gracefulReport)
	_, forcedErr := os.Stat(forcedReport)
	gracefulExists := gracefulErr == nil
	forcedExists := forcedErr == nil

	fixture, err := sli.GenerateFixture(seed)
	if err != nil {
		return err
	}
	recovered := false
	store, err := sli.OpenCheckpoint(forcedDB)
	if err == nil {
		_, loadErr := store.Load(context.Background(), sli.ProjectRuleVersion(fixture.Project))
		recovered = loadErr == nil
		_ = store.Close()
	}
	result := signalVerification{
		GracefulReportExists: gracefulExists, GracefulExitCode: gracefulExit,
		ForcedReportExists: forcedExists, ForcedExitCode: forcedExit,
		CheckpointAfterSIGKILL: recovered,
		Detail:                 "SIGTERM writes an exit report; SIGKILL generates no report; committed checkpoint remains loadable",
	}
	passed := gracefulExists && gracefulExit == 0 && !forcedExists && recovered
	proof := verification{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339), Seed: seed,
		Module: "slikernel.local/core",
		Checks: []check{{
			Name: "graceful_vs_forced_termination", Passed: passed, Details: result.Detail,
		}},
		Summary: map[string]any{"all_checks_passed": passed},
		Signal:  &result,
	}
	if err := writeJSON(filepath.Join(out, "signal-verification.json"), proof); err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(proof, "", "  ")
	fmt.Println(string(encoded))
	if !passed {
		return errors.New("signal distinction verification failed")
	}
	return nil
}

func runSignalChild(executable, mode string, seed int64, dbPath, readyPath, reportPath string, signalValue syscall.Signal) (int, string, error) {
	command := exec.Command(executable,
		"-mode", mode, "-seed", fmt.Sprintf("%d", seed), "-db", dbPath,
		"-ready", readyPath, "-report", reportPath,
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		return -1, output.String(), err
	}
	if err := waitForFile(readyPath, 15*time.Second); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return -1, output.String(), err
	}
	if err := command.Process.Signal(signalValue); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return -1, output.String(), err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = command.Process.Kill()
		<-done
		return -1, output.String(), errors.New("child did not exit after signal")
	}
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	return exitCode, output.String(), nil
}

func runChild(mode string, seed int64, dbPath, readyPath, reportPath string) error {
	fixture, err := sli.GenerateFixture(seed)
	if err != nil {
		return err
	}
	engine, err := sli.NewEngine(fixture.Project, sli.DefaultOptions())
	if err != nil {
		return err
	}
	report, err := engine.ComputeAll(context.Background())
	if err != nil {
		return err
	}
	store, err := sli.OpenCheckpoint(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	var waitContext context.Context
	stop := func() {}
	if mode == "child-graceful" {
		waitContext, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	meta, err := engine.SaveCheckpoint(context.Background(), store)
	if err != nil {
		return err
	}
	if err := writeJSON(readyPath, map[string]any{
		"ready": true, "committed_seq": meta.CommittedSeq,
	}); err != nil {
		return err
	}
	if mode == "child-kill" {
		select {}
	}
	<-waitContext.Done()
	finalMeta, err := engine.SaveCheckpoint(context.Background(), store)
	if err != nil {
		return err
	}
	if err := writeJSON(reportPath, map[string]any{
		"status": "graceful_exit", "signal_handled": true,
		"committed_seq": finalMeta.CommittedSeq, "observation_seq": finalMeta.ObservationSeq,
		"window_count": len(report.Results),
	}); err != nil {
		return err
	}
	return nil
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", path)
}

func collectExceptionCodes(project sli.Project) (map[string]string, error) {
	codes := make(map[string]string)
	add := func(name string, err error) {
		if err != nil {
			codes[name] = string(sli.ErrorCode(err))
		}
	}
	_, err := sli.LoadProject([]byte(`{"schema_version":`))
	add("malformed_json", err)
	_, err = sli.LoadProject([]byte(`{"schema_version":"9"}`))
	add("unsupported_schema", err)
	payload, err := sli.EncodeProject(project, 2)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, err
	}
	services := document["services"].([]any)
	service := services[0].(map[string]any)
	sliDefs := service["sli_defs"].([]any)
	sliDefs[0].(map[string]any)["target_bps"] = 10001
	badRatio, _ := json.Marshal(document)
	_, err = sli.LoadProject(badRatio)
	add("ratio_range", err)
	sliDefs[0].(map[string]any)["target_bps"] = 9950
	sliDefs[1].(map[string]any)["quantile_permille"] = 1001
	badQuantile, _ := json.Marshal(document)
	_, err = sli.LoadProject(badQuantile)
	add("quantile_range", err)
	sliDefs[1].(map[string]any)["quantile_permille"] = 950
	sliDefs[0].(map[string]any)["window_length"] = -1
	badWindow, _ := json.Marshal(document)
	_, err = sli.LoadProject(badWindow)
	add("window_length", err)
	_, err = sli.NewEngine(project, sli.Options{Workers: sli.MaxWorkers + 1, PerWorkerBuffer: 1})
	add("thread_budget", err)
	_, err = sli.NewEngine(project, sli.Options{Workers: 1, PerWorkerBuffer: sli.MaxPerWorkerBuffer + 1})
	add("buffer_budget", err)
	engine, err := sli.NewEngine(project, sli.Options{Workers: 1, PerWorkerBuffer: 1, BatchSize: 1, FailOnBackpressure: true})
	if err == nil {
		_, err = engine.ComputeAll(context.Background())
	}
	add("backpressure", err)
	return codes, nil
}

func reportsCoreEqual(expected, actual *sli.Report) (bool, string) {
	if expected.RuleVersion != actual.RuleVersion {
		return false, "rule version mismatch"
	}
	if len(expected.Results) != len(actual.Results) {
		return false, fmt.Sprintf("result count %d != %d", len(expected.Results), len(actual.Results))
	}
	for index := range expected.Results {
		left := expected.Results[index]
		right := actual.Results[index]
		left.Provenance = sli.Provenance{}
		right.Provenance = sli.Provenance{}
		if !reflect.DeepEqual(left, right) {
			return false, fmt.Sprintf("first mismatch at result %d %s/%s/%d",
				index, left.Service, left.SLI, left.EvalIndex)
		}
	}
	return true, "all windows and downstream decisions match exactly"
}

func countSLIs(project sli.Project) int {
	total := 0
	for _, service := range project.Services {
		total += len(service.SLIs)
	}
	return total
}

func countExhausted(report *sli.Report) int {
	total := 0
	for _, result := range report.Results {
		if result.Budget.Exhausted {
			total++
		}
	}
	return total
}

func countAlerts(report *sli.Report) int {
	total := 0
	for _, result := range report.Results {
		if result.Alert.Alert {
			total++
		}
	}
	return total
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func writeJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(encoded, '\n'))
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

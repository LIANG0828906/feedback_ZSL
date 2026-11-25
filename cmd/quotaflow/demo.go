package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"

	"quotaflow/internal/checkpoint"
	"quotaflow/internal/domain"
	"quotaflow/internal/engine"
	"quotaflow/internal/generator"
	"quotaflow/internal/inject"
	"quotaflow/internal/schema"
)

type demoCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type demoInjection struct {
	Kind   string `json:"kind"`
	Code   string `json:"code"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

type demoReport struct {
	Seed               int64                  `json:"seed"`
	OutputDir          string                 `json:"output_dir"`
	ProjectID          string                 `json:"project_id"`
	ProjectFingerprint string                 `json:"project_fingerprint"`
	Checks             []demoCheck            `json:"checks"`
	Incremental        any                    `json:"incremental"`
	Injections         []demoInjection        `json:"injections"`
	FinalTick          domain.TickObservation `json:"final_tick"`
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic seed")
	out := fs.String("out", "artifacts/demo", "demo output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return domain.NewBizError(domain.ECheckpointIO, *out, "create demo directory: %v", err)
	}
	samples := filepath.Join(*out, "samples")
	project, truth, err := generator.Generate(*seed, samples)
	if err != nil {
		return err
	}
	fmt.Printf("== deterministic samples ==\n%s seed=%d ticks=%d migrations=%d pauses=%d\n",
		project.ID, *seed, project.MaxTick, len(truth.Migrations), len(truth.PauseHistory))
	report := demoReport{
		Seed: *seed, OutputDir: *out, ProjectID: project.ID,
		ProjectFingerprint: project.CanonicalFingerprint(),
	}
	set, err := loadSet(samples)
	if err != nil {
		return err
	}
	allSame := true
	for _, version := range []string{"v2", "v3"} {
		allSame = allSame && set[version].CanonicalFingerprint() == set["v1"].CanonicalFingerprint()
	}
	report.Checks = append(report.Checks, demoCheck{
		Name: "three-schema-equivalence", Passed: allSame,
		Detail: set["v1"].CanonicalFingerprint(),
	})

	eng, err := engine.New(set["v2"], engine.Config{ThreadBudget: 4, QueueLimit: 64})
	if err != nil {
		return err
	}
	if err := eng.RunAll(); err != nil {
		return err
	}
	baseline := eng.Observations()
	truthPass := reflect.DeepEqual(baseline, truth.Ticks)
	report.Checks = append(report.Checks, demoCheck{
		Name: "generator-truth", Passed: truthPass,
		Detail: fmt.Sprintf("ticks=%d", len(truth.Ticks)),
	})
	if !truthPass {
		return domain.NewBizError(domain.ELedgerInvariant, "$.truth", "demo truth mismatch")
	}
	fmt.Println("\n== per-tick ledger, pauses and migrations ==")
	printTickHeader()
	for index := range baseline {
		printTickCompact(&baseline[index])
	}

	weightReport, err := eng.ApplyEdit(engine.Edit{
		ID: "demo-weight-alpha", Kind: engine.EditTenantWeight, Target: "t-alpha",
		FromTick: 5, Weight: 3,
	})
	if err != nil {
		return err
	}
	capacityReport, err := eng.ApplyEdit(engine.Edit{
		ID: "demo-capacity-c1", Kind: engine.EditNodeCapacity, Target: "n-c1",
		FromTick: 3, CapacityMilli: 26000,
	})
	if err != nil {
		return err
	}
	full, err := eng.FullRecompute()
	if err != nil {
		return err
	}
	edited := eng.Observations()
	incrementalPass := reflect.DeepEqual(edited, full.Observations())
	firstDiff := firstDifferentTick(baseline, edited)
	report.Checks = append(report.Checks, demoCheck{
		Name: "incremental-vs-full", Passed: incrementalPass,
		Detail: fmt.Sprintf("first_diff_tick=%d", firstDiff),
	})
	report.Incremental = map[string]any{
		"weight_report": weightReport, "capacity_report": capacityReport,
		"first_diff_tick": firstDiff,
	}
	fmt.Printf("\n== incremental invalidation ==\nweight: first=%d reused=%v recomputed=%v closure=%v\n",
		weightReport.FirstAffectedTick, weightReport.ReusedTicks, weightReport.RecomputedTicks, weightReport.PropagationClosure)
	fmt.Printf("capacity: first=%d reused=%v recomputed=%v closure=%v\n",
		capacityReport.FirstAffectedTick, capacityReport.ReusedTicks, capacityReport.RecomputedTicks, capacityReport.PropagationClosure)
	fmt.Printf("incremental equals full recompute: %v; first changed tick: %d\n", incrementalPass, firstDiff)

	checkpointPath := filepath.Join(*out, "checkpoint.db")
	if err := checkpoint.Remove(checkpointPath); err != nil {
		return err
	}
	// Force an actual child-process termination while tick 7 is between its
	// state rows and the global tick update. No deferred cleanup or exit report
	// runs in the child; the parent must recover only from SQLite state.
	child := exec.Command(os.Args[0], "run",
		"-dir", samples, "-schema", "v2", "-threads", "4", "-queue", "64",
		"-db", checkpointPath, "-crash-tick", "7",
		"-crash-phase", checkpoint.PhaseBeforeGlobal)
	childOutput, childErr := child.CombinedOutput()
	crashCode := 0
	if childErr != nil {
		if exitErr, ok := childErr.(*exec.ExitError); ok {
			crashCode = exitErr.ExitCode()
		} else {
			return domain.NewBizError(domain.ECheckpointIO, checkpointPath, "spawn crash child: %v", childErr)
		}
	}
	if crashCode != 137 {
		return domain.NewBizError(domain.ECheckpointIO, checkpointPath,
			"crash child exit=%d output=%s", crashCode, string(childOutput))
	}
	reopened, _, err := checkpoint.Open(checkpointPath, "quotaflow-checkpoint-v1", project.CanonicalFingerprint())
	if err != nil {
		return err
	}
	loaded, meta, err := reopened.Load()
	if err != nil {
		return err
	}
	if err := reopened.Close(); err != nil {
		return err
	}
	resumePass := meta.Tick == 6 && len(loaded.Pauses) > 0
	resumed, _ := engine.New(project, engine.Config{ThreadBudget: 3, QueueLimit: 64, CheckpointVersion: "quotaflow-checkpoint-v1"})
	if err := resumed.ResumeFrom(loaded); err != nil {
		return err
	}
	if err := resumed.RunAll(); err != nil {
		return err
	}
	baselineFinal := baseline[len(baseline)-1]
	resumedFinal, _ := resumed.Tick(project.MaxTick)
	resumePass = resumePass && reflect.DeepEqual(*resumedFinal, baselineFinal)
	report.Checks = append(report.Checks, demoCheck{
		Name: "checkpoint-interrupted-resume", Passed: resumePass,
		Detail: fmt.Sprintf("observed_tick=%d paused_migrations=%d forced_exit=%d", meta.Tick, len(loaded.Pauses), crashCode),
	})
	fmt.Printf("\n== checkpoint resume ==\nforced_exit=%d restart_tick=%d paused=%d resumed_final_equals_full=%v\n",
		crashCode, meta.Tick, len(loaded.Pauses), resumePass)

	report.Injections = demoProjectInjections(samples, &report.Checks)
	budgetCheck := demoBudgetInjections()
	report.Injections = append(report.Injections, budgetCheck...)
	truncatedCheck, truncatedInjection := demoTruncatedCheckpoint(checkpointPath, *out)
	report.Checks = append(report.Checks, truncatedCheck)
	report.Injections = append(report.Injections, truncatedInjection)
	versionCheck, versionInjection := demoVersionMismatch(checkpointPath)
	report.Checks = append(report.Checks, versionCheck)
	report.Injections = append(report.Injections, versionInjection)
	fmt.Println("\n== injected exceptions ==")
	for _, item := range report.Injections {
		fmt.Printf("%-24s %-28s %s\n", item.Kind, item.Code, item.Path)
	}

	report.FinalTick = *resumedFinal
	failed := false
	for _, check := range report.Checks {
		if !check.Passed {
			failed = true
		}
	}
	checksAfterAppend := len(report.Checks) + 1
	report.Checks = append(report.Checks, demoCheck{
		Name: "all-checks", Passed: !failed, Detail: fmt.Sprintf("checks=%d", checksAfterAppend),
	})
	reportPath := filepath.Join(*out, "demo-report.json")
	raw, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(reportPath, append(raw, '\n'), 0o644); err != nil {
		return domain.NewBizError(domain.ECheckpointIO, reportPath, "write demo report: %v", err)
	}
	fmt.Printf("\n== result ==\nchecks=%d failed=%v report=%s\n", len(report.Checks), failed, reportPath)
	if failed {
		return domain.NewBizError(domain.EInternalInvariant, "demo", "one or more demo checks failed")
	}
	return nil
}

func demoProjectInjections(dir string, checks *[]demoCheck) []demoInjection {
	tests := []struct {
		kind string
		file string
		code string
	}{
		{inject.NegativeQuota, "project-v2.json", domain.ENegativeQuota},
		{inject.ReserveOverQuota, "project-v3.json", domain.EReserveExceedsQuota},
		{inject.FaultDomainCycle, "project-v2.json", domain.EFaultDomainCycle},
		{inject.SnapshotVersion, "project-v1.json", domain.ESchemaVersion},
		{inject.UnknownUnitField, "project-v1.json", domain.ESchemaMixedUnits},
	}
	var out []demoInjection
	for _, test := range tests {
		path := filepath.Join(dir, test.file)
		before, _ := os.ReadFile(path)
		beforeHash := hashBytes(before)
		mutated, err := inject.MutateProject(test.kind, before)
		if err != nil {
			continue
		}
		_, loadErr := schema.LoadBytes(mutated, "injected:"+path)
		after, _ := os.ReadFile(path)
		code := domain.CodeOf(loadErr)
		passed := code == test.code && beforeHash == hashBytes(after)
		*checks = append(*checks, demoCheck{
			Name: "inject-" + test.kind, Passed: passed, Detail: code + " source_unchanged=" + fmt.Sprint(beforeHash == hashBytes(after)),
		})
		bizPath := ""
		if biz, ok := loadErr.(*domain.BizError); ok {
			bizPath = biz.Path
		}
		out = append(out, demoInjection{Kind: test.kind, Code: code, Path: bizPath, Source: test.file})
	}
	return out
}

func demoBudgetInjections() []demoInjection {
	var out []demoInjection
	_, err := engine.New(generator.Scenario(42), engine.Config{ThreadBudget: 0, QueueLimit: 64})
	out = append(out, demoInjection{Kind: "thread-budget", Code: domain.CodeOf(err), Path: "$.thread_budget", Source: "runtime-config"})
	_, err = engine.New(generator.Scenario(42), engine.Config{ThreadBudget: 2, QueueLimit: 0})
	out = append(out, demoInjection{Kind: "queue-budget", Code: domain.CodeOf(err), Path: "$.queue_limit", Source: "runtime-config"})
	return out
}

func demoTruncatedCheckpoint(path, outDir string) (demoCheck, demoInjection) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) < 512 {
		return demoCheck{Name: "checkpoint-truncated", Passed: false, Detail: "cannot read db"}, demoInjection{Kind: "checkpoint-truncated", Code: domain.ECheckpointIO, Path: path}
	}
	truncatedPath := filepath.Join(outDir, "checkpoint-truncated.db")
	if err := os.WriteFile(truncatedPath, raw[:len(raw)/2], 0o644); err != nil {
		return demoCheck{Name: "checkpoint-truncated", Passed: false, Detail: err.Error()}, demoInjection{Kind: "checkpoint-truncated", Code: domain.ECheckpointIO, Path: truncatedPath}
	}
	_, _, openErr := checkpoint.Open(truncatedPath, "quotaflow-checkpoint-v1", "hash")
	code := domain.CodeOf(openErr)
	passed := code == domain.ECheckpointTruncated || code == domain.ECheckpointCorrupt
	return demoCheck{Name: "checkpoint-truncated", Passed: passed, Detail: code},
		demoInjection{Kind: "checkpoint-truncated", Code: code, Path: truncatedPath, Source: path}
}

func demoVersionMismatch(path string) (demoCheck, demoInjection) {
	store, rebuilt, err := checkpoint.Open(path, "demo-version-v2", "different-project-hash")
	if err == nil {
		store.Close()
	}
	passed := err == nil && rebuilt
	return demoCheck{Name: "checkpoint-version-mismatch-rebuild", Passed: passed, Detail: fmt.Sprintf("rebuilt=%v", rebuilt)},
		demoInjection{Kind: "checkpoint-version", Code: domain.ECheckpointVersion, Path: path, Source: "checkpoint metadata"}
}

func hashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func firstDifferentTick(left, right []domain.TickObservation) int64 {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := 0; index < limit; index++ {
		if !reflect.DeepEqual(left[index], right[index]) {
			return left[index].Tick
		}
	}
	if len(left) != len(right) {
		return int64(limit + 1)
	}
	return 0
}

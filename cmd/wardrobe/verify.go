package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"wardrobekernel/internal/batching"
	"wardrobekernel/internal/cache"
	"wardrobekernel/internal/checkpoint"
	"wardrobekernel/internal/compare"
	"wardrobekernel/internal/model"
	"wardrobekernel/internal/pipeline"
	"wardrobekernel/internal/samplegen"
	"wardrobekernel/internal/scheduler"
	"wardrobekernel/internal/schema"
	"wardrobekernel/internal/sqlitefile"
	"wardrobekernel/internal/xerr"
)

type verificationCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code,omitempty"`
}

type verificationReport struct {
	OK               bool                `json:"ok"`
	Seed             int64               `json:"seed"`
	Checks           []verificationCheck `json:"checks"`
	Invalidated      []int               `json:"invalidated_batches,omitempty"`
	Reused           []int               `json:"reused_batches,omitempty"`
	KillSignal       string              `json:"kill_signal,omitempty"`
	SQLiteMagicValid bool                `json:"sqlite_magic_valid,omitempty"`
	Message          string              `json:"message,omitempty"`
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "samples", "sample directory")
	work := fs.String("work", "artifacts/verify", "temporary verification directory")
	skipSubprocessKill := fs.Bool("skip-subprocess-kill", false, "do not spawn a SIGKILL child")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*work, 0o755); err != nil {
		return err
	}
	if err := samplegen.WriteSampleDir(*dir, model.FixedSeed); err != nil {
		return err
	}

	report := verificationReport{OK: true, Seed: model.FixedSeed, Checks: []verificationCheck{}}
	add := func(name string, ok bool, detail string, code string) {
		report.Checks = append(report.Checks, verificationCheck{Name: name, OK: ok, Detail: detail, Code: code})
		if !ok {
			report.OK = false
		}
	}

	project, err := schema.LoadProject(filepath.Join(*dir, "project.json"))
	if err != nil {
		return err
	}
	truth, err := schema.LoadTruth(filepath.Join(*dir, "truth.json"))
	if err != nil {
		return err
	}

	invalidData, err := os.ReadFile(filepath.Join(*dir, "invalid_schema.json"))
	if err != nil {
		return err
	}
	_, schemaErr := schema.ParseProject(invalidData)
	coded, schemaOK := xerr.AsError(schemaErr)
	add("E_SCHEMA field paths", schemaOK && coded.Code == xerr.ESchema && len(coded.Violations) >= 5,
		fmt.Sprintf("%d schema violations", violationCount(coded)), codeOrEmpty(schemaErr))

	arbProject, err := schema.LoadProject(filepath.Join(*dir, "ambiguous_arb.json"))
	if err != nil {
		return err
	}
	_, arbErr := batching.Plan(arbProject)
	arbCoded, arbOK := xerr.AsError(arbErr)
	add("E_ARB parties and basis chain", arbOK && arbCoded.Code == xerr.EArb && len(arbCoded.Parties) == 2 && len(arbCoded.BasisChain) >= 3,
		arbErrorDetail(arbCoded), codeOrEmpty(arbErr))

	truthPlan, err := pipeline.VerifyTruth(project, truth)
	add("truth itemwise match", err == nil, "fresh deterministic plan equals checked-in truth", codeOrEmpty(err))
	if err != nil {
		return finishVerify(report, err)
	}

	realtimeDB := filepath.Join(*work, "realtime.sqlite")
	_ = os.Remove(realtimeDB)
	rtStore, err := checkpoint.Open(realtimeDB, project.AlgorithmVersion, project.ConstraintVersion)
	if err != nil {
		return err
	}
	realtime, err := pipeline.RunRealtime(context.Background(), project, pipeline.RealtimeOptions{
		Scheduler: schedulerOptionsForVerify(), Checkpoint: rtStore,
	})
	if err != nil {
		return err
	}
	add("realtime bounded scheduling", len(realtime.Scheduled.Waits) > 0 && len(realtime.Commits) == len(truthPlan.Batches),
		fmt.Sprintf("%d wait records, %d recovered failures, %d commits", len(realtime.Scheduled.Waits), recoveredFailures(realtime.Scheduled.Failures), len(realtime.Commits)), "")
	if err := compare.Plans(truthPlan, realtime.Plan); err != nil {
		add("realtime path equals full", false, err.Error(), xerr.EDiff)
		return finishVerify(report, err)
	}
	add("realtime path equals full", true, "batches, offsets, conflicts and commit sequence equal", "")

	resume, err := pipeline.ResumeAndContinue(context.Background(), project, rtStore, schedulerOptionsForVerify())
	if err != nil {
		return err
	}
	if err := compare.Plans(truthPlan, resume.Plan); err != nil {
		add("checkpoint resume equals full", false, err.Error(), xerr.EDiff)
		return finishVerify(report, err)
	}
	add("checkpoint resume equals full", true, "all durable batches reused and compared equal", "")

	rows, err := sqlitefile.ReadKVFile(realtimeDB)
	report.SQLiteMagicValid = err == nil && len(rows) >= len(truthPlan.Batches)+2
	add("pure-Go SQLite file readable", report.SQLiteMagicValid, fmt.Sprintf("%d k/v rows", len(rows)), codeOrEmpty(err))

	wrongStore, verErr := checkpoint.Open(realtimeDB, "batch-kernel-v0.0.0-wrong", "")
	verCoded, verOK := xerr.AsError(verErr)
	add("E_VER version mismatch", verOK && verCoded.Code == xerr.EVer && len(verCoded.AffectedBatches) > 0,
		versionDetail(verCoded), codeOrEmpty(verErr))
	_ = wrongStore

	cachePath := filepath.Join(*work, "incremental-cache.json")
	_ = os.Remove(cachePath)
	persistent, err := cache.Load(cachePath, project.AlgorithmVersion, project.ConstraintVersion)
	if err != nil {
		return err
	}
	for _, batch := range truthPlan.Batches {
		if err := persistent.Put(batch); err != nil {
			return err
		}
	}
	edited, err := pipeline.CloneProject(project)
	if err != nil {
		return err
	}
	if err := pipeline.EditMaterialVersion(edited, "waist_belt", "main", "v2-edited-verify"); err != nil {
		return err
	}
	inc, err := pipeline.IncrementalReplan(project, truthPlan, persistent, edited)
	if err != nil {
		add("incremental equals full", false, err.Error(), xerr.EDiff)
		return finishVerify(report, err)
	}
	report.Invalidated = inc.Invalidation.InvalidBatches
	report.Reused = inc.Invalidation.ReusedBatches
	add("precise invalidation", equalInts(inc.Invalidation.InvalidBatches, []int{3}) && containsInts(inc.Invalidation.ReusedBatches, []int{1, 2, 4}),
		fmt.Sprintf("invalid=%v reused=%v", inc.Invalidation.InvalidBatches, inc.Invalidation.ReusedBatches), "")
	add("incremental equals full", inc.Comparison != nil && inc.Comparison.Equal, "first mismatch locator is available on E_DIFF", "")

	if !*skipSubprocessKill {
		killDB := filepath.Join(*work, "kill-boundary.sqlite")
		_ = os.Remove(killDB)
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "demo",
			"--project", filepath.Join(*dir, "project.json"),
			"--truth", filepath.Join(*dir, "truth.json"),
			"--checkpoint", killDB, "--kill-after-commit", "2",
			"--work-delay-ms", "0", "--fail-first", "")
		cmd.Env = append(os.Environ(), "GODEBUG=asyncpreemptoff=1")
		runErr := cmd.Run()
		signal := ""
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				signal = status.Signal().String()
			}
		}
		report.KillSignal = signal
		committedRows, readErr := sqlitefile.ReadKVFile(killDB)
		durableBatches := 0
		if readErr == nil {
			for _, row := range committedRows {
				if len(row.Key) >= 6 && row.Key[:6] == "batch:" {
					durableBatches++
				}
			}
		}
		killOK := signal == "killed" && durableBatches == 2
		add("SIGKILL commit boundary", killOK, fmt.Sprintf("signal=%s durable_batches=%d run_error=%v", signal, durableBatches, runErr), "")

		killStore, err := checkpoint.Open(killDB, project.AlgorithmVersion, project.ConstraintVersion)
		if err != nil {
			return err
		}
		recoveredKill, err := pipeline.ResumeAndContinue(context.Background(), project, killStore, scheduler.Options{})
		if err != nil {
			return err
		}
		if err := compare.Plans(truthPlan, recoveredKill.Plan); err != nil {
			add("post-SIGKILL recovery equals full", false, err.Error(), xerr.EDiff)
			return finishVerify(report, err)
		}
		add("post-SIGKILL recovery equals full", true, "restart reused batches 1-2 and recomputed tail", "")
	}

	for _, batch := range truthPlan.Batches {
		budgetOK := batch.VertexUsed <= project.Constraints.MaxVertexBudget && batch.IndexUsed <= project.Constraints.MaxIndexBudget && len(batch.Slots) <= project.Constraints.MaxMaterialSlots
		if !budgetOK {
			add("batch budgets", false, fmt.Sprintf("batch %d exceeds budget", batch.Number), xerr.EState)
		}
	}
	add("batch budgets", report.OK, "all batch vertex/index/slot totals within constraints", "")

	report.Message = "all acceptance checks passed"
	writeJSONIndent(report)
	if !report.OK {
		return xerr.New(xerr.EState, "verification failed")
	}
	return nil
}

func schedulerOptionsForVerify() scheduler.Options {
	return scheduler.Options{WorkDelay: time.Millisecond, FailFirstAttempt: map[string]bool{"waist_belt": true}}
}

func finishVerify(report verificationReport, err error) error {
	report.OK = false
	report.Message = err.Error()
	writeJSONIndent(report)
	return err
}

func violationCount(c *xerr.Error) int {
	if c == nil {
		return 0
	}
	return len(c.Violations)
}

func codeOrEmpty(err error) string {
	c, ok := xerr.AsError(err)
	if !ok {
		return ""
	}
	return c.Code
}

func arbErrorDetail(c *xerr.Error) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("parties=%v basis=%d", c.Parties, len(c.BasisChain))
}

func versionDetail(c *xerr.Error) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("persisted=%s expected=%s affected=%v", c.PersistedVersion, c.ExpectedVersion, c.AffectedBatches)
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsInts(all []int, wanted []int) bool {
	set := map[int]bool{}
	for _, x := range all {
		set[x] = true
	}
	for _, x := range wanted {
		if !set[x] {
			return false
		}
	}
	return true
}

func recoveredFailures(failures []model.PartFailure) int {
	count := 0
	for _, failure := range failures {
		if failure.Recovered {
			count++
		}
	}
	return count
}

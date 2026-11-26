package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	"wardrobekernel/internal/cache"
	"wardrobekernel/internal/checkpoint"
	"wardrobekernel/internal/compare"
	"wardrobekernel/internal/model"
	"wardrobekernel/internal/pipeline"
	"wardrobekernel/internal/scheduler"
	"wardrobekernel/internal/schema"
	"wardrobekernel/internal/xerr"
)

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	projectPath := fs.String("project", "samples/project.json", "project JSON")
	truthPath := fs.String("truth", "samples/truth.json", "truth JSON")
	checkpointPath := fs.String("checkpoint", "artifacts/checkpoint.sqlite", "SQLite checkpoint path")
	killAfter := fs.Int("kill-after-commit", 0, "SIGKILL process after this durable commit sequence")
	failFirst := fs.String("fail-first", "waist_belt", "part ID whose first preparation attempt fails")
	workDelayMS := fs.Int("work-delay-ms", 2, "simulated per-part work delay")
	keepCheckpoint := fs.Bool("keep-checkpoint", false, "do not reset checkpoint before run")
	if err := fs.Parse(args); err != nil {
		return err
	}

	project, err := schema.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	if *truthPath != "" {
		if truth, err := schema.LoadTruth(*truthPath); err == nil {
			if _, err := pipeline.VerifyTruth(project, truth); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	if !*keepCheckpoint {
		_ = os.Remove(*checkpointPath)
	}
	store, err := checkpoint.Open(*checkpointPath, project.AlgorithmVersion, project.ConstraintVersion)
	if err != nil {
		return err
	}

	failFirstMap := map[string]bool{}
	if *failFirst != "" {
		failFirstMap[*failFirst] = true
	}
	result, err := pipeline.RunRealtime(context.Background(), project, pipeline.RealtimeOptions{
		Scheduler: scheduler.Options{
			WorkDelay:        time.Duration(*workDelayMS) * time.Millisecond,
			FailFirstAttempt: failFirstMap,
		},
		Checkpoint:      store,
		KillAfterCommit: *killAfter,
		Kill: func() {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		},
	})
	if err != nil {
		return err
	}

	report := model.Report{
		OK: true, Seed: project.Seed, AlgorithmVersion: project.AlgorithmVersion,
		ConstraintVersion: project.ConstraintVersion, Assembly: result.Plan.Assembly,
		Batches: result.Plan.Batches, Decisions: result.Plan.Decisions,
		CommitSequence: result.Plan.CommitSequence, WaitRecords: result.Scheduled.Waits,
		Failures: result.Scheduled.Failures, CheckpointBatches: commitNumbers(result.Commits),
		SQLitePath: *checkpointPath, Message: "realtime bounded scheduling and sequential SQLite commit completed",
	}
	writeJSONIndent(report)
	return nil
}

func runResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	projectPath := fs.String("project", "samples/project.json", "project JSON")
	checkpointPath := fs.String("checkpoint", "artifacts/checkpoint.sqlite", "SQLite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := schema.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(*checkpointPath, project.AlgorithmVersion, project.ConstraintVersion)
	if err != nil {
		return err
	}
	recovered, err := pipeline.ResumeAndContinue(context.Background(), project, store, scheduler.Options{})
	if err != nil {
		return err
	}
	full, err := pipeline.FullPlan(project)
	if err != nil {
		return err
	}
	comparison := &model.ComparisonReport{Equal: true, Paths: []string{"checkpoint_resume", "full_empty_cache"}}
	if err := compare.Plans(full, recovered.Plan); err != nil {
		if coded, ok := xerr.AsError(err); ok {
			comparison.Equal = false
			comparison.FirstMismatch = coded.FirstMismatch
		}
		writeJSONIndent(model.Report{OK: false, Comparison: comparison})
		return err
	}

	report := model.Report{
		OK: true, Seed: project.Seed, AlgorithmVersion: project.AlgorithmVersion,
		ConstraintVersion: project.ConstraintVersion, Assembly: recovered.Plan.Assembly,
		Batches: recovered.Plan.Batches, Decisions: recovered.Plan.Decisions,
		CommitSequence:    recovered.Plan.CommitSequence,
		CheckpointBatches: recovered.RecoveredBatches, SQLitePath: *checkpointPath,
		Comparison: comparison,
		Message:    fmt.Sprintf("resumed from %d durable batches; committed %d tail batches", len(recovered.RecoveredBatches), len(recovered.NewCommits)),
	}
	writeJSONIndent(report)
	return nil
}

func runIncrementalDemo(args []string) error {
	fs := flag.NewFlagSet("incremental-demo", flag.ContinueOnError)
	projectPath := fs.String("project", "samples/project.json", "project JSON")
	truthPath := fs.String("truth", "samples/truth.json", "optional truth JSON")
	cachePath := fs.String("cache", "artifacts/incremental-cache.json", "persistent cache JSON path")
	editPart := fs.String("edit-part", "waist_belt", "part to edit")
	editSlot := fs.String("edit-slot", "main", "material slot to edit")
	materialVersion := fs.String("material-version", "v2-edited", "new material version")
	vertexDelta := fs.Int("vertex-delta", 0, "positive geometry footprint delta; when >0 geometry is edited")
	if err := fs.Parse(args); err != nil {
		return err
	}

	project, err := schema.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	if *truthPath != "" {
		if truth, err := schema.LoadTruth(*truthPath); err == nil {
			if _, err := pipeline.VerifyTruth(project, truth); err != nil {
				return err
			}
		}
	}
	oldPlan, err := pipeline.FullPlan(project)
	if err != nil {
		return err
	}

	_ = os.Remove(*cachePath)
	persistent, err := cache.Load(*cachePath, project.AlgorithmVersion, project.ConstraintVersion)
	if err != nil {
		return err
	}
	for _, batch := range oldPlan.Batches {
		if err := persistent.Put(batch); err != nil {
			return err
		}
	}

	edited, err := pipeline.CloneProject(project)
	if err != nil {
		return err
	}
	editDescription := fmt.Sprintf("material %s/%s -> %s", *editPart, *editSlot, *materialVersion)
	if *vertexDelta > 0 {
		if err := pipeline.EditGeometryScale(edited, *editPart, *vertexDelta); err != nil {
			return xerr.New(xerr.EState, err.Error())
		}
		editDescription = fmt.Sprintf("geometry %s vertices += %d", *editPart, *vertexDelta)
	} else {
		if err := pipeline.EditMaterialVersion(edited, *editPart, *editSlot, *materialVersion); err != nil {
			return xerr.New(xerr.EState, err.Error())
		}
	}

	result, err := pipeline.IncrementalReplan(project, oldPlan, persistent, edited)
	report := model.Report{
		OK: err == nil, Seed: edited.Seed, AlgorithmVersion: edited.AlgorithmVersion,
		ConstraintVersion: edited.ConstraintVersion, Message: editDescription,
		CachePath: *cachePath,
	}
	if result != nil {
		report.InvalidatedBatches = result.Invalidation.InvalidBatches
		report.ReusedBatches = result.Invalidation.ReusedBatches
		report.Comparison = result.Comparison
		report.Batches = result.Plan.Batches
		report.Assembly = result.Plan.Assembly
		report.CommitSequence = result.Plan.CommitSequence
	}
	if err != nil {
		writeJSONIndent(report)
		return err
	}
	writeJSONIndent(report)
	return nil
}

func runVersionCheck(args []string) error {
	fs := flag.NewFlagSet("version-check", flag.ContinueOnError)
	checkpointPath := fs.String("checkpoint", "artifacts/checkpoint.sqlite", "SQLite checkpoint path")
	algorithmVersion := fs.String("algorithm-version", model.AlgorithmVersion+"-wrong", "expected algorithm version")
	constraintVersion := fs.String("constraint-version", "", "expected constraint version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := checkpoint.Open(*checkpointPath, *algorithmVersion, *constraintVersion)
	if err != nil {
		return err
	}
	writeJSONIndent(map[string]any{"ok": true, "batches": store.LastSequence()})
	return nil
}

func runDiffDemo(args []string) error {
	fs := flag.NewFlagSet("diff-demo", flag.ContinueOnError)
	projectPath := fs.String("project", "samples/project.json", "project JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := schema.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	full, err := pipeline.FullPlan(project)
	if err != nil {
		return err
	}
	bad, err := pipeline.ClonePlan(full)
	if err != nil {
		return err
	}
	if len(bad.Batches) == 0 || len(bad.Batches[0].Placements) == 0 || len(bad.Batches[0].Placements[0].Submeshes) == 0 {
		return xerr.New(xerr.EState, "plan does not contain a submesh to mutate")
	}
	bad.Batches[0].Placements[0].Submeshes[0].VertexStart += 7
	return compare.Plans(full, bad)
}

func commitNumbers(commits []model.CommitRecord) []int {
	out := make([]int, 0, len(commits))
	for _, c := range commits {
		out = append(out, c.BatchNumber)
	}
	return out
}

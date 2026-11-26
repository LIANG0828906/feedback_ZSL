package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"strokelab"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "version":
		fmt.Printf("strokediag %s algorithm=%s checkpoint=%s\n",
			"v1", strokelab.AlgorithmVersion, strokelab.CheckpointFormat)
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		printError(err)
		os.Exit(exitCode(err))
	}
}

func usage() {
	fmt.Print(`strokediag — deterministic stroke reconstruction and diagnostics

  strokediag generate -seed 424242 -out artifacts/demo
  strokediag run -dir artifacts/demo/v3-grouped-128 -mode full -out artifacts/diag
  strokediag run -dir SAMPLE -mode incremental -checkpoint artifacts/run.ckpt -out artifacts/diag
  strokediag run -dir SAMPLE -mode resume -checkpoint artifacts/run.ckpt -out artifacts/diag
  strokediag run -dir SAMPLE -mode incremental -checkpoint artifacts/run.ckpt -hard-abort-after 3
  strokediag verify -seed 424242 -out artifacts/acceptance

No network, container, external driver, or account is used. Checkpoints are
standard-library CRC32-framed append-only journals with fsync per transaction.
`)
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", 424242, "deterministic seed")
	out := fs.String("out", "artifacts/demo", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	suite, err := strokelab.GenerateSuite(*seed)
	if err != nil {
		return err
	}
	manifest, err := strokelab.WriteSuite(*out, suite)
	if err != nil {
		return err
	}
	fmt.Printf("generated %d samples with seed %d in %s\n", len(manifest.Samples), *seed, *out)
	for _, s := range manifest.Samples {
		fmt.Printf("  %-18s %-14s %3dx%-3d strokes=%2d order=%s\n",
			s.Name, s.Version, s.Width, s.Height, s.Strokes, short(s.OrderVersion))
	}
	fmt.Printf("suite manifest: %s\n", filepath.Join(*out, "suite.json"))
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dir := fs.String("dir", "", "sample directory containing project.json")
	mode := fs.String("mode", "full", "full|incremental|resume")
	checkpointPath := fs.String("checkpoint", "", "checkpoint journal path")
	out := fs.String("out", "artifacts/diag", "diagnostic output directory")
	workers := fs.Int("workers", 3, "maximum worker parallelism")
	queue := fs.Int("queue", 8, "pending block queue budget")
	pixels := fs.Int("pixels", 4096, "pixel buffer budget per worker")
	retries := fs.Int("retries", 1, "retries for a failed block")
	stopAfter := fs.Int("stop-after", 0, "stop after N new committed strokes (library-safe)")
	hardAbort := fs.Int("hard-abort-after", 0, "exit(17) after N new commits to simulate SIGKILL")
	reuseCachePath := fs.String("reuse-cache", "", "load a prior block cache and reuse unchanged blocks")
	cacheOutPath := fs.String("cache-out", "", "write the resulting block cache")
	editStroke := fs.String("edit-stroke", "", "stroke id to edit before running")
	editWeight := fs.Int("edit-weight", 0, "new weight in pp10000")
	editHardness := fs.Int("edit-hardness", 0, "new hardness in permille")
	editStage := fs.Int("edit-stage", 0, "new stage priority")
	editSeq := fs.Int("edit-seq", 0, "new one-based operation sequence")
	editStartX := fs.Int("edit-start-x", 0, "new start x")
	editStartY := fs.Int("edit-start-y", 0, "new start y")
	editEndX := fs.Int("edit-end-x", 0, "new end x")
	editEndY := fs.Int("edit-end-y", 0, "new end y")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("-dir is required")
	}
	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	sample, err := strokelab.LoadSampleDir(*dir)
	if err != nil {
		return err
	}
	project := sample.Project
	var plan *strokelab.InvalidationPlan
	if *editStroke != "" {
		edit := strokelab.Edit{StrokeID: *editStroke}
		if visited["edit-weight"] {
			v := *editWeight
			edit.Weight = &v
		}
		if visited["edit-hardness"] {
			v := *editHardness
			edit.Hardness = &v
		}
		if visited["edit-stage"] {
			v := *editStage
			edit.Stage = &v
		}
		if visited["edit-seq"] {
			v := *editSeq
			edit.Seq = &v
		}
		if visited["edit-start-x"] || visited["edit-start-y"] {
			var old *strokelab.Stroke
			for i := range project.Strokes {
				if project.Strokes[i].ID == *editStroke {
					old = &project.Strokes[i]
					break
				}
			}
			if old == nil {
				return fmt.Errorf("unknown edit stroke %q", *editStroke)
			}
			p := old.Start
			if visited["edit-start-x"] {
				p.X = *editStartX
			}
			if visited["edit-start-y"] {
				p.Y = *editStartY
			}
			edit.Start = &p
		}
		if visited["edit-end-x"] || visited["edit-end-y"] {
			var old *strokelab.Stroke
			for i := range project.Strokes {
				if project.Strokes[i].ID == *editStroke {
					old = &project.Strokes[i]
					break
				}
			}
			if old == nil {
				return fmt.Errorf("unknown edit stroke %q", *editStroke)
			}
			p := old.End
			if visited["edit-end-x"] {
				p.X = *editEndX
			}
			if visited["edit-end-y"] {
				p.Y = *editEndY
			}
			edit.End = &p
		}
		edited, err := strokelab.ApplyEdit(project, edit)
		if err != nil {
			return err
		}
		plan, err = strokelab.PlanInvalidation(project, edited, edit)
		if err != nil {
			return err
		}
		sample.Project = edited
		project = edited
	}
	engine, err := strokelab.NewEngine(project)
	if err != nil {
		return err
	}
	opts := strokelab.RunOptions{
		Mode: strokelab.RunMode(*mode), RunID: fmt.Sprintf("cli-%d", time.Now().UnixNano()),
		Scheduler: strokelab.SchedulerConfig{
			MaxParallelism: *workers, MaxPixelsPerWorker: *pixels,
			QueueBudget: *queue, MaxRetries: *retries,
		},
		StopAfterCommits: *stopAfter,
	}
	if *reuseCachePath != "" {
		cache, err := strokelab.LoadBlockCache(*reuseCachePath)
		if err != nil {
			return err
		}
		opts.ReuseCache = cache
	}
	if *mode != string(strokelab.RunFull) {
		cpPath := *checkpointPath
		if cpPath == "" {
			cpPath = filepath.Join(*out, "checkpoint.ckpt")
		}
		cp, err := strokelab.OpenCheckpoint(cpPath)
		if err != nil {
			return err
		}
		defer cp.Close()
		opts.Checkpoint = cp
		*checkpointPath = cpPath
	}
	if *hardAbort > 0 {
		opts.StopAfterCommits = *hardAbort
	}
	ctx := context.Background()
	eval, runErr := engine.Run(ctx, opts)
	if *hardAbort > 0 && errors.Is(runErr, strokelab.ErrSimulatedAbort) {
		fmt.Printf("HARD ABORT simulated after %d committed strokes; no report promised\n",
			eval.CommittedStrokes)
		os.Exit(17)
	}
	if eval == nil {
		return runErr
	}
	if runErr != nil && !errors.Is(runErr, strokelab.ErrSimulatedAbort) &&
		strokelab.ErrorCode(runErr) != strokelab.CodeTopology {
		return runErr
	}
	if sum := strokelab.SummarizeInvalidation(plan); plan != nil {
		fmt.Println(sum)
	}
	reportPath, diag, err := strokelab.WriteEvaluationDiagnostics(*out, sample, eval, plan, nil)
	if err != nil {
		return err
	}
	if *cacheOutPath != "" && eval.Cache != nil {
		if err := eval.Cache.Save(*cacheOutPath); err != nil {
			return err
		}
	}
	fmt.Printf("report: %s\n", reportPath)
	fmt.Printf("composite=%s mode=%s blocks=%d reused=%d recomputed=%d\n",
		short(strokelab.HashHex(eval.Composite)), eval.Mode, len(eval.Blocks),
		len(eval.ReusedBlocks), len(eval.RecomputedBlocks))
	if eval.Topology != nil {
		fmt.Printf("topology=%v differences=%d issues=%d\n",
			eval.Topology.Passed, eval.Topology.DifferencePixels, len(eval.Topology.Issues))
	}
	fmt.Printf("checkpoint records=%d committed_strokes=%d complete=%v tail_ignored=%v path=%s\n",
		eval.Checkpoint.Records, eval.Checkpoint.CommittedStrokes,
		eval.Checkpoint.Complete, eval.Checkpoint.TailIgnored, eval.Checkpoint.Path)
	if diag != nil {
		fmt.Printf("queue_records=%d attribution_differences=%d\n",
			len(diag.Queue), len(diag.DiffPreview))
	}
	if errors.Is(runErr, strokelab.ErrSimulatedAbort) {
		fmt.Println("partial run stopped; rerun with -mode resume and the same checkpoint")
		return nil
	}
	if *cacheOutPath != "" {
		fmt.Printf("block cache: %s\n", *cacheOutPath)
	}
	if runErr != nil {
		return runErr
	}
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := fs.Int64("seed", 424242, "deterministic seed")
	out := fs.String("out", "artifacts/acceptance", "verification output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := strokelab.RunAcceptance(*seed, *out)
	if report != nil {
		status := "FAIL"
		if report.AllPassed {
			status = "PASS"
		}
		fmt.Printf("acceptance: %s (seed=%d sample=%s)\n", status, report.Seed, report.SelectedSample)
		fmt.Printf("  full=%s\n  incremental=%s\n  resume=%s\n",
			short(report.Full.CompositeHash), short(report.Incremental.CompositeHash),
			short(report.Resume.CompositeHash))
		fmt.Printf("  interrupted=%d checkpoint_records=%d resumed_commits=%d complete=%v\n",
			report.InterruptedCommits, report.CheckpointAfterStop.Records,
			report.CheckpointResumed.CommittedStrokes, report.CheckpointResumed.Complete)
		fmt.Printf("  invalidation_affected=%d recomputed=%d cache_reuse_equal=%v\n",
			len(report.ExpectedAffected), len(report.RecomputedAfterEdit),
			report.EditedReuseCompare != nil && report.EditedReuseCompare.Equal)
		fmt.Printf("  verification=%s\n", report.Artifacts["verification_json"])
		fmt.Printf("  resume_report=%s\n  edited_report=%s\n",
			report.Artifacts["resume_report"], report.Artifacts["edited_report"])
	}
	return err
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

func printError(err error) {
	var ce *strokelab.CodedError
	if errors.As(err, &ce) {
		fmt.Fprintf(os.Stderr, "%s: %s\n", ce.Code, ce.Message)
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "flag provided") || strings.Contains(msg, "flag needs") {
		fmt.Fprintln(os.Stderr, msg)
		return
	}
	fmt.Fprintln(os.Stderr, msg)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	switch strokelab.ErrorCode(err) {
	case strokelab.CodeTopology:
		return 2
	case strokelab.CodeCanceled:
		return 130
	default:
		return 1
	}
}

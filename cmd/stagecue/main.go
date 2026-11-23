package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"stageworkbench/pkg/generator"
	"stageworkbench/pkg/stage"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = commandGenerate(os.Args[2:])
	case "validate":
		err = commandValidate(os.Args[2:])
	case "run":
		err = commandRun(os.Args[2:])
	case "verify":
		err = commandVerify(os.Args[2:])
	case "presets":
		writeJSON(generator.Presets())
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		if domain, ok := stage.AsDomainError(err); ok {
			_ = json.NewEncoder(os.Stderr).Encode(domain)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`stagecue - deterministic stage lighting workbench

Commands:
  generate  Generate a local deterministic project and truth files
  validate  Validate a project and print a structural summary
  run       Run realtime/resume/full scheduling with SQLite checkpointing
  verify    Compare realtime, checkpoint-resume, and full recompute paths
  presets   List built-in scale presets

Examples:
  stagecue generate --preset small --seed 42 --out samples/small-42
  stagecue validate --project samples/small-42/project.json
  stagecue run --project samples/small-42/project.json --mode realtime --db /tmp/show.db
  stagecue verify --project samples/small-42/project.json --workers 4 --buffer 4194304`)
}

func commandGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	preset := fs.String("preset", "small", "tiny|small|medium|large|stress")
	seed := fs.Int64("seed", 42, "fixed random seed")
	out := fs.String("out", "", "output directory")
	projectID := fs.String("id", "", "project id")
	cues := fs.Int("cues", 0, "override cue count")
	lights := fs.Int("lights", 0, "override light count")
	groups := fs.Int("groups", 0, "override group count")
	duration := fs.Int64("duration-ms", 0, "override duration")
	fps := fs.Int("fps", 0, "override fps")
	window := fs.Int64("window-ms", 0, "override logical window")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join("samples", fmt.Sprintf("%s-%d", *preset, *seed))
	}
	project, err := generator.Generate(generator.Options{
		Preset:     *preset,
		Seed:       *seed,
		ProjectID:  *projectID,
		CueCount:   *cues,
		LightCount: *lights,
		GroupCount: *groups,
		DurationMS: *duration,
		FPS:        *fps,
		WindowMS:   *window,
	})
	if err != nil {
		return err
	}
	if err := generator.SaveProject(*out, project); err != nil {
		return err
	}
	writeJSON(map[string]any{
		"out":           *out,
		"project_id":    project.ID,
		"cues":          len(project.Cues),
		"lights":        len(project.Lights),
		"groups":        len(project.Groups),
		"truth_samples": len(project.Truth.Samples),
		"content_hash":  stage.ContentHash(project),
	})
	return nil
}

func commandValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	path := fs.String("project", "", "project.json path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := generator.LoadProject(*path)
	if err != nil {
		return err
	}
	writeJSON(map[string]any{
		"valid":        true,
		"project_id":   project.ID,
		"lights":       len(project.Lights),
		"groups":       len(project.Groups),
		"cues":         len(project.Cues),
		"duration_ms":  project.DurationMS,
		"fps":          project.FPS,
		"content_hash": stage.ContentHash(project),
	})
	return nil
}

func commandRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	projectPath := fs.String("project", "", "project.json path")
	dbPath := fs.String("db", "workbench.db", "SQLite checkpoint path")
	mode := fs.String("mode", "realtime", "realtime|resume|full")
	workers := fs.Int("workers", 4, "maximum worker concurrency")
	buffer := fs.Int64("buffer", 16<<20, "per-worker memory budget")
	memory := fs.Int64("memory", 0, "total memory budget (0 = workers * buffer)")
	stopAfter := fs.Int("stop-after", 0, "pause after N newly committed windows")
	forceMode := fs.String("force-mode", "", "exact|merged")
	window := fs.Int64("window-ms", 0, "override logical window")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := generator.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	params := stage.DefaultParams()
	params.MaxWorkers = *workers
	params.PerWorkerBufferBytes = *buffer
	params.MemoryBudgetBytes = *memory
	params.WindowMS = *window
	params.ForceMode = stage.Mode(*forceMode)
	runner, err := stage.NewRunner(project, params)
	if err != nil {
		return err
	}
	started := time.Now()
	result, err := runner.Run(context.Background(), stage.RunOptions{
		Mode:           stage.RunMode(*mode),
		CheckpointPath: *dbPath,
		StopAfter:      *stopAfter,
	})
	if err != nil {
		return err
	}
	writeJSON(map[string]any{
		"project_id":      result.ProjectID,
		"mode":            result.Mode,
		"decision":        result.Decision,
		"progress":        result.Progress,
		"partial":         result.Partial,
		"commit_order":    result.CommitOrder,
		"budget_records":  result.BudgetLog,
		"invalidated":     result.CheckpointInvalidated,
		"invalid_reason":  result.InvalidationReason,
		"elapsed_ms":      time.Since(started).Milliseconds(),
		"windows_written": len(result.CommitOrder),
	})
	return nil
}

func commandVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	projectPath := fs.String("project", "", "project.json path")
	workDir := fs.String("work-dir", "", "directory for temporary SQLite checkpoints")
	workers := fs.Int("workers", 4, "maximum worker concurrency")
	buffer := fs.Int64("buffer", 16<<20, "per-worker memory budget")
	memory := fs.Int64("memory", 0, "total memory budget (0 = workers * buffer)")
	forceMode := fs.String("force-mode", "", "exact|merged")
	window := fs.Int64("window-ms", 0, "override logical window")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := generator.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	params := stage.DefaultParams()
	params.MaxWorkers = *workers
	params.PerWorkerBufferBytes = *buffer
	params.MemoryBudgetBytes = *memory
	params.WindowMS = *window
	params.ForceMode = stage.Mode(*forceMode)
	report, err := stage.VerifyThreePaths(context.Background(), project, params, *workDir)
	if err != nil {
		if report.FirstDivergence != nil {
			_ = json.NewEncoder(os.Stderr).Encode(report)
		}
		return err
	}
	writeJSON(report)
	fmt.Fprintln(os.Stderr, report.Summary())
	return nil
}

func writeJSON(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

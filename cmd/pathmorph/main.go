package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"pathmorph/morph"
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
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "experiment":
		err = cmdExperiment(os.Args[2:])
	case "stress":
		err = cmdStress(os.Args[2:])
	case "ui":
		err = cmdUI(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `pathmorph - deterministic morphing vector path compositor

Commands:
  generate    generate a deterministic scene and analytic truth manifest
  validate    load and validate a scene and truth manifest
  run         render a scene, export grayscale frames and coverage statistics
  experiment  run truth, quality, error-chain and three-path checks
  stress      generate local concurrency/memory pressure scenes
  ui          launch the local diagnostic web interface

Use "pathmorph <command> -h" for command flags.`)
}

func fail(err error) {
	if e, ok := morph.AsError(err); ok {
		data, _ := json.MarshalIndent(e, "", "  ")
		fmt.Fprintln(os.Stderr, string(data))
	} else {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(1)
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "data", "output directory")
	id := fs.String("id", "demo", "scene id")
	seed := fs.Int64("seed", 42, "deterministic seed")
	paths := fs.Int("paths", 8, "number of paths")
	keyframes := fs.Int("keyframes", 3, "keyframes per path")
	segments := fs.Int("segments", 7, "closed Bezier segments per path")
	samples := fs.Int("samples", 9, "minimum samples (midpoints are added)")
	scale := fs.Float64("scale", 12, "pixels per world unit")
	quantum := fs.Float64("quantum", 0.001, "control-point quantization")
	refGrid := fs.Int("reference-grid", 96, "dense truth sampling grid")
	extent := fs.Float64("coord-extent", 48, "coordinate half-extent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := morph.DefaultGenerateOptions()
	opts.ID, opts.Seed, opts.Paths, opts.Keyframes = *id, *seed, *paths, *keyframes
	opts.Segments, opts.Samples, opts.Scale, opts.Quantum = *segments, *samples, *scale, *quantum
	opts.ReferenceGrid, opts.CoordExtent = *refGrid, *extent
	scene, truth, err := morph.GenerateScene(opts)
	if err != nil {
		return err
	}
	sp, tp, err := morph.WriteGenerated(*out, scene, truth)
	if err != nil {
		return err
	}
	fmt.Printf("scene=%s\ntruth=%s\npaths=%d keyframes=%d samples=%d\n", sp, tp, len(scene.Paths), opts.Keyframes, len(scene.SampleTimes))
	return nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	scenePath := fs.String("scene", "", "scene JSON")
	truthPath := fs.String("truth", "", "truth JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scenePath == "" {
		return fmt.Errorf("-scene is required")
	}
	scene, err := morph.LoadScene(*scenePath)
	if err != nil {
		return err
	}
	if err := morph.ValidateScene(scene); err != nil {
		return err
	}
	msg := fmt.Sprintf("scene valid: id=%s paths=%d samples=%d", scene.ID, len(scene.Paths), len(scene.SampleTimes))
	if *truthPath != "" {
		truth, err := morph.LoadTruth(*truthPath)
		if err != nil {
			return err
		}
		if err := morph.ValidateTruth(scene, truth); err != nil {
			return err
		}
		msg += fmt.Sprintf(" truth valid: samples=%d", len(truth.Samples))
	}
	fmt.Println(msg)
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	scenePath := fs.String("scene", "", "scene JSON")
	truthPath := fs.String("truth", "", "truth JSON")
	out := fs.String("out", "out/run", "output directory")
	checkpoint := fs.String("checkpoint", "", "checkpoint directory (default OUT/checkpoints)")
	mode := fs.String("mode", "auto", "auto|fine|approx")
	workers := fs.Int("workers", 4, "worker count")
	grid := fs.Int("grid", 4, "subsample grid")
	midTol := fs.Float64("mid-tolerance", 0.02, "midpoint flattening tolerance")
	budget := fs.Int64("budget", 256<<20, "memory budget bytes")
	perThread := fs.Int64("per-thread-bytes", 64<<20, "per-worker pixel buffer bytes")
	interp := fs.String("interpolation", "linear", "linear|smoothstep|smootherstep")
	maxChunk := fs.Int("max-chunk-paths", 32, "maximum path chunk")
	stopAfter := fs.Int("stop-after", 0, "stop after N commits (for checkpoint drills)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scenePath == "" {
		return fmt.Errorf("-scene is required")
	}
	scene, err := morph.LoadScene(*scenePath)
	if err != nil {
		return err
	}
	if err := morph.ValidateScene(scene); err != nil {
		return err
	}
	var truth *morph.Truth
	if *truthPath != "" {
		truth, err = morph.LoadTruth(*truthPath)
		if err != nil {
			return err
		}
		if err := morph.ValidateTruth(scene, truth); err != nil {
			return err
		}
	}
	cfg := morph.DefaultConfigForScene(scene)
	cfg.Eval.Interpolation = *interp
	cfg.Eval.MidTolerance = *midTol
	cfg.Render.Grid = *grid
	cfg.Adaptive.BaseGrid = *grid
	cfg.Adaptive.MemoryBudgetBytes = *budget
	switch *mode {
	case "fine":
		cfg.Adaptive.ForceFine = true
	case "approx":
		cfg.Adaptive.ForceFine = false
		cfg.Adaptive.WorkThreshold = 1
	case "auto":
		// Keep the automatic policy.
	default:
		return fmt.Errorf("invalid -mode %q (want auto|fine|approx)", *mode)
	}
	cfg.Runtime.Workers = *workers
	cfg.Runtime.PerThreadPixelBytes = *perThread
	cfg.Runtime.MaxChunkPaths = *maxChunk
	if *checkpoint == "" {
		*checkpoint = filepath.Join(*out, "checkpoints")
	}
	cfg.Checkpoint = *checkpoint
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	engine, err := morph.NewEngine(scene, cfg)
	if err != nil {
		return err
	}
	summary, err := engine.Run(context.Background(), morph.RunOptions{StopAfterCommits: *stopAfter})
	if err != nil {
		return err
	}
	results := engine.Results()
	if err := morph.ExportResults(filepath.Join(*out, "frames"), results); err != nil {
		return err
	}
	_ = morph.WriteJSONAtomic(filepath.Join(*out, "run_summary.json"), summary)
	truthOK := "not-checked"
	if truth != nil {
		tr, err := morph.ValidateResultsAgainstTruth(scene, truth, results, cfg, 1e-6, 0.12)
		if err != nil {
			_ = morph.WriteJSONAtomic(filepath.Join(*out, "truth_report.json"), tr)
			return err
		}
		_ = morph.WriteJSONAtomic(filepath.Join(*out, "truth_report.json"), tr)
		truthOK = "true"
	}
	fmt.Printf("mode=%s grid=%d committed=%d/%d elapsed_ms=%d truth_ok=%v frames=%s\n",
		summary.Quality.Mode, summary.Quality.Grid, summary.CommittedSamples, summary.TotalSamples,
		summary.ElapsedMillis, truthOK, filepath.Join(*out, "frames"))
	return nil
}

func cmdExperiment(args []string) error {
	fs := flag.NewFlagSet("experiment", flag.ContinueOnError)
	out := fs.String("out", "artifacts/experiment", "output directory")
	scenePath := fs.String("scene", "", "existing scene JSON (optional)")
	truthPath := fs.String("truth", "", "existing truth JSON (optional)")
	seed := fs.Int64("seed", 42, "seed when generating")
	paths := fs.Int("paths", 8, "paths when generating")
	keyframes := fs.Int("keyframes", 3, "keyframes when generating")
	samples := fs.Int("samples", 9, "samples when generating")
	scale := fs.Float64("scale", 12, "scale when generating")
	workers := fs.Int("workers", 4, "worker count")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var scene *morph.Scene
	var truth *morph.Truth
	var err error
	if *scenePath != "" {
		scene, err = morph.LoadScene(*scenePath)
		if err != nil {
			return err
		}
		if *truthPath == "" {
			return fmt.Errorf("-truth is required with -scene")
		}
		truth, err = morph.LoadTruth(*truthPath)
		if err != nil {
			return err
		}
	} else {
		opts := morph.DefaultGenerateOptions()
		opts.ID, opts.Seed, opts.Paths, opts.Keyframes = "experiment", *seed, *paths, *keyframes
		opts.Samples, opts.Scale = *samples, *scale
		opts.ReferenceGrid = 96
		scene, truth, err = morph.GenerateScene(opts)
		if err != nil {
			return err
		}
	}
	if err := morph.ValidateScene(scene); err != nil {
		return err
	}
	if err := morph.ValidateTruth(scene, truth); err != nil {
		return err
	}
	cfg := morph.DefaultConfigForScene(scene)
	cfg.Runtime.Workers = *workers
	cfg.Adaptive.BaseGrid = cfg.Render.Grid
	cfg.Adaptive.BaseMidTolerance = cfg.Eval.MidTolerance
	rep, err := morph.RunExperiment(*out, scene, truth, cfg)
	if rep != nil {
		fmt.Printf("experiment report: %s\ntruth=%v quality=%v three_path=%v errors=%v all_pass=%v\n",
			filepath.Join(*out, "experiment.json"), rep.TruthCheck != nil && rep.TruthCheck.Pass,
			rep.QualityCheck != nil && rep.QualityCheck.Pass, rep.ThreePath != nil && rep.ThreePath.Pass,
			rep.ErrorChainCovered, rep.AllPass)
	}
	return err
}

func cmdStress(args []string) error {
	fs := flag.NewFlagSet("stress", flag.ContinueOnError)
	out := fs.String("out", "artifacts/stress", "output directory")
	seed := fs.Int64("seed", 4242, "deterministic seed")
	run := fs.Bool("run", false, "also render all stress levels")
	if err := fs.Parse(args); err != nil {
		return err
	}
	started := time.Now()
	rep, err := morph.GenerateStressScenarios(*out, *seed, *run)
	if err != nil {
		if rep != nil {
			fmt.Fprintf(os.Stderr, "partial stress report: %s\n", filepath.Join(*out, "stress_report.json"))
		}
		return err
	}
	fmt.Printf("stress scenes=%d out=%s elapsed_ms=%d\n", len(rep.Levels), *out, time.Since(started).Milliseconds())
	for _, l := range rep.Levels {
		fmt.Printf("  %-12s paths=%4d keyframes=%d samples=%d mode=%s grid=%d work=%d\n", l.Name, l.Paths, l.Keyframes, l.Samples, l.Decision.Mode, l.Decision.Grid, l.WorkUnits)
	}
	return nil
}

func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	scenePath := fs.String("scene", "", "scene JSON")
	truthPath := fs.String("truth", "", "truth JSON (optional but enables quality checks)")
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	work := fs.String("work", "artifacts/ui", "checkpoint/export directory")
	checkpoint := fs.String("checkpoint", "", "main checkpoint directory")
	mode := fs.String("mode", "auto", "auto|fine|approx")
	workers := fs.Int("workers", 4, "worker count")
	grid := fs.Int("grid", 4, "subsample grid")
	compare := fs.Bool("compare-fine", true, "precompute a fine reference for diff heatmaps")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scenePath == "" {
		return fmt.Errorf("-scene is required")
	}
	scene, err := morph.LoadScene(*scenePath)
	if err != nil {
		return err
	}
	if err := morph.ValidateScene(scene); err != nil {
		return err
	}
	var truth *morph.Truth
	if *truthPath != "" {
		truth, err = morph.LoadTruth(*truthPath)
		if err != nil {
			return err
		}
		if err := morph.ValidateTruth(scene, truth); err != nil {
			return err
		}
	}
	cfg := morph.DefaultConfigForScene(scene)
	cfg.Runtime.Workers = *workers
	cfg.Render.Grid = *grid
	cfg.Adaptive.BaseGrid = *grid
	switch *mode {
	case "fine":
		cfg.Adaptive.ForceFine = true
	case "approx":
		cfg.Adaptive.WorkThreshold = 1
	case "auto":
	default:
		return fmt.Errorf("invalid -mode %q (want auto|fine|approx)", *mode)
	}
	if *checkpoint != "" {
		cfg.Checkpoint = *checkpoint
	}
	return morph.ServeDiagnostics(morph.UIOptions{
		Addr: *addr, Scene: scene, Truth: truth, Config: cfg, WorkDir: *work, CompareFine: *compare,
	})
}

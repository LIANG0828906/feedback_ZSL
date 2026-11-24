package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"audioeval/internal/compare"
	"audioeval/internal/engine"
	"audioeval/internal/model"
	"audioeval/internal/report"
	"audioeval/internal/sample"
	"audioeval/internal/verify"
)

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	if len(args) == 0 {
		return 1, fmt.Errorf("usage: audioeval <generate|run|verify|demo> [flags]")
	}
	switch args[0] {
	case "generate":
		return runGenerate(args[1:])
	case "run":
		return runEvaluate(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "demo":
		return runDemo(args[1:])
	default:
		return 1, fmt.Errorf("unknown command %q", args[0])
	}
}

func runGenerate(args []string) (int, error) {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	dir := fs.String("dir", "", "sample output directory")
	sampleID := fs.String("sample-id", "game-sfx", "sample id")
	seed := fs.Int64("seed", 424242, "deterministic generator seed")
	sampleRate := fs.Int("sample-rate", 48000, "sample rate")
	channels := fs.Int("channels", 2, "channel count")
	frameLength := fs.Int("frame-length", 256, "frame length in sampling points")
	frameHop := fs.Int("frame-hop", 128, "frame hop in sampling points")
	frameCount := fs.Int("frame-count", 8, "frame count")
	quality := fs.String("quality", string(model.QualityQ3), "quality level: Q1..Q5")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}
	if *dir == "" {
		return 1, fmt.Errorf("--dir is required")
	}
	generated, err := sample.Generate(sample.GenerateOptions{
		Dir:         *dir,
		SampleID:    *sampleID,
		Seed:        *seed,
		SampleRate:  *sampleRate,
		Channels:    *channels,
		FrameLength: *frameLength,
		FrameHop:    *frameHop,
		FrameCount:  *frameCount,
		Quality:     model.QualityLevel(*quality),
	})
	if err != nil {
		return 1, err
	}
	return 0, printJSON(generated.Manifest)
}

func runEvaluate(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	sampleDir := fs.String("sample-dir", "", "sample directory")
	runDir := fs.String("run-dir", "", "checkpoint and report directory")
	mode := fs.String("mode", string(engine.ModeRealtime), "mode: realtime, resume or full")
	window := fs.Int("window", 64, "evaluation window")
	windowHop := fs.Int("window-hop", 16, "evaluation window hop")
	blocksPerFrame := fs.Int("blocks-per-frame", 4, "fixed blocks per frame")
	bandWidth := fs.Int("band-width", 0, "band width; 0 selects window+hop-1")
	threads := fs.Int("threads", 4, "maximum worker threads")
	bufferSamples := fs.Int("buffer-samples", 8192, "per-thread sample buffer budget")
	queueBudget := fs.Int("queue", 2, "task queue budget")
	cancelAfter := fs.Int("cancel-after", 0, "cancel after this many new block commits")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}
	if *sampleDir == "" || *runDir == "" {
		return 1, fmt.Errorf("--sample-dir and --run-dir are required")
	}
	loaded, err := sample.Load(*sampleDir)
	if err != nil {
		return 1, err
	}
	params := model.EvalParams{
		Window:                 *window,
		WindowHop:              *windowHop,
		BlocksPerFrame:         *blocksPerFrame,
		BandWidth:              *bandWidth,
		MaxWorkers:             *threads,
		PerThreadBufferSamples: *bufferSamples,
		QueueBudget:            *queueBudget,
		CancelAfterCommits:     *cancelAfter,
	}
	result, err := engine.Evaluate(context.Background(), loaded, engine.RunOptions{
		Mode:   engine.Mode(*mode),
		RunDir: *runDir,
		Params: params,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fmt.Printf("partial checkpoint retained: committed=%d/%d complete=false\n",
				result.Report.Resume.CommittedBlocks, result.Report.Resume.TotalBlocks)
			return 2, nil
		}
		return 1, err
	}
	fmt.Printf("complete=true sample=%s frames=%d blocks=%d mode=%s\n",
		result.Report.SampleID, len(result.Report.Frames), len(result.Report.CommitSequence), result.Report.Mode)
	return 0, nil
}

func runVerify(args []string) (int, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	sampleDir := fs.String("sample-dir", "", "sample directory")
	runDir := fs.String("run-dir", "", "checkpoint and report directory")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}
	if *sampleDir == "" || *runDir == "" {
		return 1, fmt.Errorf("--sample-dir and --run-dir are required")
	}
	result, err := verify.Verify(verify.Options{SampleDir: *sampleDir, RunDir: *runDir})
	if err != nil {
		return 1, err
	}
	if err := printJSON(result); err != nil {
		return 1, err
	}
	if !result.OK {
		return 1, nil
	}
	return 0, nil
}

type validationSummary struct {
	OK          bool                       `json:"ok"`
	MainSample  string                     `json:"main_sample"`
	Runs        map[string]string          `json:"runs"`
	Comparisons map[string]string          `json:"comparisons"`
	Verifier    verify.Result              `json:"verifier"`
	Budget      map[string]any             `json:"budget"`
	Monotonic   []qualityPoint             `json:"monotonic_snr"`
	ErrorCodes  []string                   `json:"error_codes_covered_by_tests"`
	Details     map[string]json.RawMessage `json:"details,omitempty"`
}

type qualityPoint struct {
	Quality string `json:"quality"`
	PSNRUDb int64  `json:"mean_psnr_udb"`
}

func runDemo(args []string) (int, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	root := fs.String("dir", filepath.Join("artifacts", "demo"), "demo workspace")
	seed := fs.Int64("seed", 424242, "deterministic generator seed")
	frameCount := fs.Int("frame-count", 8, "frame count")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}
	if err := os.RemoveAll(*root); err != nil {
		return 1, err
	}
	mainSampleDir := filepath.Join(*root, "samples", "q3")
	mainGenerated, err := sample.Generate(sample.GenerateOptions{
		Dir:         mainSampleDir,
		SampleID:    "game-sfx-q3",
		Seed:        *seed,
		SampleRate:  48000,
		Channels:    2,
		FrameLength: 256,
		FrameHop:    128,
		FrameCount:  *frameCount,
		Quality:     model.QualityQ3,
	})
	if err != nil {
		return 1, err
	}
	loaded, err := sample.Load(mainSampleDir)
	if err != nil {
		return 1, err
	}
	base := model.EvalParams{
		Window:                 64,
		WindowHop:              16,
		BlocksPerFrame:         4,
		MaxWorkers:             4,
		PerThreadBufferSamples: 8192,
		QueueBudget:            1,
	}
	realtimeParams := base
	realtimeParams.MaxWorkers = 4
	realtimeParams.QueueBudget = 1
	realtimeResult, err := engine.Evaluate(context.Background(), loaded, engine.RunOptions{
		Mode:   engine.ModeRealtime,
		RunDir: filepath.Join(*root, "runs", "realtime"),
		Params: realtimeParams,
	})
	if err != nil {
		return 1, fmt.Errorf("realtime run: %w", err)
	}

	fullParams := base
	fullParams.MaxWorkers = 1
	fullParams.QueueBudget = 0
	fullResult, err := engine.Evaluate(context.Background(), loaded, engine.RunOptions{
		Mode:   engine.ModeFull,
		RunDir: filepath.Join(*root, "runs", "full"),
		Params: fullParams,
	})
	if err != nil {
		return 1, fmt.Errorf("full run: %w", err)
	}

	resumeDir := filepath.Join(*root, "runs", "resume")
	cancelParams := realtimeParams
	cancelParams.CancelAfterCommits = 3
	canceledResult, err := engine.Evaluate(context.Background(), loaded, engine.RunOptions{
		Mode:   engine.ModeResume,
		RunDir: resumeDir,
		Params: cancelParams,
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		return 1, fmt.Errorf("resume seed run did not cancel at the declared boundary: %v", err)
	}
	if canceledResult.Report.Complete {
		return 1, fmt.Errorf("partial run incorrectly reported a complete result")
	}
	resumeParams := realtimeParams
	resumeResult, err := engine.Evaluate(context.Background(), loaded, engine.RunOptions{
		Mode:   engine.ModeResume,
		RunDir: resumeDir,
		Params: resumeParams,
	})
	if err != nil {
		return 1, fmt.Errorf("resume run: %w", err)
	}
	if !resumeResult.Report.Resume.Resumed || resumeResult.Report.Resume.LoadedBlocks == 0 {
		return 1, fmt.Errorf("resume run did not load persisted blocks")
	}

	comparisons := map[string]string{}
	if mismatch := compare.Reports(fullResult.Report, realtimeResult.Report); mismatch != nil {
		return 1, fmt.Errorf("realtime/full mismatch: %w", mismatch)
	}
	comparisons["realtime_vs_full"] = "match"
	if mismatch := compare.Reports(fullResult.Report, resumeResult.Report); mismatch != nil {
		return 1, fmt.Errorf("resume/full mismatch: %w", mismatch)
	}
	comparisons["resume_vs_full"] = "match"

	verification, err := verify.Verify(verify.Options{SampleDir: mainSampleDir, RunDir: filepath.Join(*root, "runs", "resume")})
	if err != nil {
		return 1, fmt.Errorf("independent verification: %w", err)
	}
	if !verification.OK {
		return 1, fmt.Errorf("independent verification failed: %+v", verification.FirstViolation)
	}

	monotonic, err := checkQualityMonotonicity(*root, *seed, *frameCount, base)
	if err != nil {
		return 1, err
	}
	if err := copyFile(filepath.Join(*root, "runs", "full", "report.json"), filepath.Join(*root, "report.json")); err != nil {
		return 1, err
	}
	if err := copyFile(filepath.Join(*root, "runs", "full", "heatmap.png"), filepath.Join(*root, "heatmap.png")); err != nil {
		return 1, err
	}

	summary := validationSummary{
		OK:         true,
		MainSample: mainGenerated.Manifest.SampleID,
		Runs: map[string]string{
			"realtime": filepath.Join(*root, "runs", "realtime"),
			"resume":   resumeDir,
			"full":     filepath.Join(*root, "runs", "full"),
		},
		Comparisons: comparisons,
		Verifier:    verification,
		Budget: map[string]any{
			"max_workers":         realtimeParams.MaxWorkers,
			"queue_budget":        realtimeParams.QueueBudget,
			"blocked_tasks":       len(realtimeResult.Report.QueueEvents),
			"queue_events":        realtimeResult.Report.QueueEvents,
			"partial_committed":   canceledResult.Report.Resume.CommittedBlocks,
			"resume_loaded":       resumeResult.Report.Resume.LoadedBlocks,
			"resume_total_blocks": resumeResult.Report.Resume.TotalBlocks,
		},
		Monotonic: monotonic,
		ErrorCodes: []string{
			"E_PARA via malformed fields/unsupported quality/window bounds",
			"E_DECODE via damaged PCM and hash mismatches",
			"E_STATE via illegal block transitions and checkpoint version changes",
			"E_BAND via insufficient block edge bands",
		},
	}
	if err := report.WriteJSON(filepath.Join(*root, "validation.json"), summary); err != nil {
		return 1, err
	}
	fmt.Printf("demo complete: %s\n", *root)
	fmt.Printf("three paths match; verifier blocks=%d frames=%d; queue waits=%d; PSNR monotonic Q1<Q3<Q5\n",
		verification.CheckedBlocks, verification.CheckedFrames, len(realtimeResult.Report.QueueEvents))
	return 0, nil
}

func checkQualityMonotonicity(root string, seed int64, frameCount int, base model.EvalParams) ([]qualityPoint, error) {
	qualities := []model.QualityLevel{model.QualityQ1, model.QualityQ3, model.QualityQ5}
	points := make([]qualityPoint, 0, len(qualities))
	for _, quality := range qualities {
		dir := filepath.Join(root, "samples", strings.ToLower(string(quality)))
		if _, err := sample.Generate(sample.GenerateOptions{
			Dir:         dir,
			SampleID:    "game-sfx-" + strings.ToLower(string(quality)),
			Seed:        seed,
			SampleRate:  48000,
			Channels:    2,
			FrameLength: 256,
			FrameHop:    128,
			FrameCount:  frameCount,
			Quality:     quality,
		}); err != nil {
			return nil, err
		}
		loaded, err := sample.Load(dir)
		if err != nil {
			return nil, err
		}
		params := base
		params.MaxWorkers = 1
		params.QueueBudget = 0
		result, err := engine.Evaluate(context.Background(), loaded, engine.RunOptions{
			Mode:   engine.ModeFull,
			RunDir: filepath.Join(root, "runs", "snr-"+strings.ToLower(string(quality))),
			Params: params,
		})
		if err != nil {
			return nil, err
		}
		var total int64
		for _, frame := range result.Report.Frames {
			total += frame.PSNRUDb
		}
		points = append(points, qualityPoint{
			Quality: string(quality),
			PSNRUDb: total / int64(len(result.Report.Frames)),
		})
	}
	for i := 1; i < len(points); i++ {
		if points[i-1].PSNRUDb >= points[i].PSNRUDb {
			return nil, fmt.Errorf("quality monotonicity failed: %s=%d udB, %s=%d udB",
				points[i-1].Quality, points[i-1].PSNRUDb, points[i].Quality, points[i].PSNRUDb)
		}
	}
	return points, nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func copyFile(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o644)
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/pipeline"
	"ocrworkbench/internal/sample"
	"ocrworkbench/internal/store"
	"ocrworkbench/internal/verify"
)

func main() {
	dataDir := flag.String("data", "data", "deterministic sample directory")
	dbPath := flag.String("db", "bench.db", "realtime SQLite checkpoint database")
	outDir := flag.String("out", "exports", "run JSON export directory")
	seed := flag.Int64("seed", 42, "sample generator seed if data is absent")
	flag.Parse()

	manifest, err := sample.LoadManifest(*dataDir)
	if err != nil {
		if !os.IsNotExist(err) {
			fail(err)
		}
		manifest, err = sample.Generate(sample.Options{OutputDir: *dataDir, Seed: *seed})
		if err != nil {
			fail(err)
		}
	}
	ctx := context.Background()
	base := pipeline.Runner{
		Detector: core.DefaultDetectorParams(),
		Segment:  core.DefaultSegmentParams(),
	}

	realtimeStore, err := store.Open(*dbPath)
	if err != nil {
		fail(err)
	}
	realtimeRunner := base
	realtimeRunner.Store = realtimeStore
	realtime, err := realtimeRunner.Run(ctx, manifest, *dataDir, "realtime")
	if err != nil {
		fail(fmt.Errorf("realtime: %w", err))
	}
	_ = realtimeStore.Close()

	resumeStore, err := store.Open(*dbPath + ".resume")
	if err != nil {
		fail(err)
	}
	resumeRunner := base
	resumeRunner.Store = resumeStore
	resumeRunner.Faults.CrashAfterTileCommits = 2
	resumed, resumeErr := resumeRunner.Run(ctx, manifest, *dataDir, "resume")
	if errors.Is(resumeErr, pipeline.ErrInjectedCrash) {
		resumeRunner.Faults.CrashAfterTileCommits = 0
		resumeRunner.Faults.CrashAfterLineCommits = 2
		resumed, resumeErr = resumeRunner.Run(ctx, manifest, *dataDir, "resume")
	}
	if errors.Is(resumeErr, pipeline.ErrInjectedCrash) {
		resumeRunner.Faults.CrashAfterLineCommits = 0
		resumed, resumeErr = resumeRunner.Run(ctx, manifest, *dataDir, "resume")
	}
	if resumeErr != nil {
		fail(fmt.Errorf("resume: %w", resumeErr))
	}
	_ = resumeStore.Close()

	fullRunner := base
	full, err := fullRunner.Run(ctx, manifest, *dataDir, "full")
	if err != nil {
		fail(fmt.Errorf("full: %w", err))
	}

	var issues []verify.Issue
	issues = append(issues, verify.CompareRuns(realtime, resumed)...)
	issues = append(issues, verify.CompareRuns(realtime, full)...)
	issues = append(issues, verify.VerifyRun(realtime, manifest)...)
	issues = append(issues, verify.VerifyRun(resumed, manifest)...)
	issues = append(issues, verify.VerifyRun(full, manifest)...)

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fail(err)
	}
	for name, run := range map[string]core.RunResult{"realtime": realtime, "resume": resumed, "full": full} {
		if err := verify.WriteRun(filepath.Join(*outDir, name+".json"), run); err != nil {
			fail(err)
		}
	}
	images := 0
	lines := 0
	empty := 0
	chars := 0
	minIoU := 1.0
	minConfidence := 1.0
	maxConfidence := 0.0
	for _, image := range realtime.Images {
		images++
		lines += len(image.Lines)
		empty += image.Stats.EmptyLineCount
		chars += image.Stats.CharacterCount
	}
	for _, metric := range verify.Summarize(realtime, manifest) {
		if metric.MinIoU < minIoU {
			minIoU = metric.MinIoU
		}
		if metric.MinConfidence < minConfidence {
			minConfidence = metric.MinConfidence
		}
		if metric.MaxConfidence > maxConfidence {
			maxConfidence = metric.MaxConfidence
		}
	}
	fmt.Printf("images=%d lines=%d chars=%d empty=%d min_iou=%.4f confidence=[%.4f,%.4f] template=%s params=%s\n",
		images, lines, chars, empty, minIoU, minConfidence, maxConfidence, realtime.TemplateVersion, realtime.ParameterVersion[:12])
	if len(issues) > 0 {
		fmt.Println(verify.FormatIssues(issues))
		os.Exit(1)
	}
	fmt.Printf("OK: realtime, resume and full are identical; exports written to %s\n", *outDir)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ocrbench:", err)
	os.Exit(1)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"vectorlab/engine"
)

type verificationReport struct {
	GeneratedAt     string                `json:"generatedAt"`
	Seed            int64                 `json:"seed"`
	SceneID         string                `json:"sceneId"`
	LoadVersions    []string              `json:"loadVersions"`
	RoundTrip       []roundTripResult     `json:"roundTrip"`
	Truth           string                `json:"truthVerification"`
	Quality         qualityReport         `json:"quality"`
	RunComparison   *engine.DiffReport    `json:"runComparison"`
	Resume          engine.SnapshotStatus `json:"resumeSnapshot"`
	LossyExport     []engine.Warning      `json:"lossyExport"`
	RunReports      []runSummary          `json:"runs"`
	AllChecksPassed bool                  `json:"allChecksPassed"`
	Notes           []string              `json:"notes"`
}

type roundTripResult struct {
	Source string `json:"source"`
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

type qualityReport struct {
	FineMode       string                 `json:"fineMode"`
	ApproxMode     string                 `json:"approxMode"`
	FineDecision   engine.QualityDecision `json:"fineDecision"`
	ApproxDecision engine.QualityDecision `json:"approxDecision"`
	Metrics        []engine.ScaleMetric   `json:"metrics"`
	OK             bool                   `json:"ok"`
	Error          string                 `json:"error,omitempty"`
}

type runSummary struct {
	Mode           string   `json:"mode"`
	Frames         int      `json:"frames"`
	CommitSequence []int    `json:"commitSequence"`
	Hashes         []string `json:"hashes"`
	Partial        bool     `json:"partial"`
	ElapsedMs      int64    `json:"elapsedMs"`
}

func main() {
	dataDir := flag.String("data", "artifacts/sample", "sample directory")
	outDir := flag.String("out", "artifacts/verify", "verification output directory")
	dbPath := flag.String("db", "", "SQLite snapshot path (default <out>/snapshots.db)")
	workers := flag.Int("workers", 3, "maximum worker count")
	bufferBytes := flag.Int64("buffer", 8*1024*1024, "per-worker pixel buffer limit")
	generate := flag.Bool("generate", true, "generate the deterministic sample first")
	flag.Parse()
	if *dbPath == "" {
		*dbPath = filepath.Join(*outDir, "snapshots.db")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err)
	}
	if *generate {
		if _, err := engine.GenerateSample(*dataDir); err != nil {
			fatal(err)
		}
	}
	report := verificationReport{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Seed: engine.SampleSeed}
	scenes := map[string]*engine.Scene{}
	for _, file := range []string{"scene_v1.json", "scene_v2.json", "scene_v3.json"} {
		scene, err := engine.LoadSceneFile(filepath.Join(*dataDir, file))
		if err != nil {
			fatal(fmt.Errorf("load %s: %w", file, err))
		}
		scenes[file] = scene
		report.LoadVersions = append(report.LoadVersions, string(scene.SourceVersion))
	}
	canonical := scenes["scene_v3.json"]
	report.SceneID = canonical.ID
	for _, file := range []string{"scene_v1.json", "scene_v2.json"} {
		if err := engine.SemanticDiff(canonical, scenes[file]); err != nil {
			fatal(fmt.Errorf("cross-version semantics: %w", err))
		}
	}
	for _, file := range []string{"scene_v3.json", "scene_v1.json", "scene_v2.json"} {
		scene := scenes[file]
		for _, target := range []engine.Version{engine.Version1, engine.Version2, engine.Version3} {
			item := roundTripResult{Source: string(scene.SourceVersion), Target: string(target), OK: true}
			if _, _, err := engine.RoundTripScene(scene, target, engine.RoundFloor); err != nil {
				item.OK = false
				item.Error = err.Error()
			}
			report.RoundTrip = append(report.RoundTrip, item)
		}
	}
	truth, err := engine.LoadTruthFile(filepath.Join(*dataDir, "truth.json"))
	if err != nil {
		fatal(err)
	}
	fineDecision, err := engine.SelectQuality(canonical, 512*1024*1024, *workers)
	if err != nil {
		fatal(err)
	}
	if fineDecision.Mode != "fine" {
		fatal(fmt.Errorf("sufficient budget did not select fine mode: %+v", fineDecision))
	}
	fineScene := engine.ApplyQuality(canonical, fineDecision)
	fineRun, err := engine.Run(context.Background(), canonical, engine.RunOptions{
		Mode: engine.ModePreview, MaxWorkers: *workers, BufferBytes: *bufferBytes, BudgetBytes: 512 * 1024 * 1024, Quality: &fineDecision,
	})
	if err != nil {
		fatal(err)
	}
	if err := engine.VerifyTruth(fineScene, truth, fineRun.Frames, engine.DefaultTruthTolerance()); err != nil {
		report.Truth = err.Error()
	} else if err := engine.VerifyFlattenAgainstTruth(fineScene, truth, fineRun.Frames); err != nil {
		report.Truth = err.Error()
	} else {
		report.Truth = "ok"
	}
	lowBudget := int64(1)
	approxDecision, err := engine.SelectQuality(canonical, lowBudget, *workers)
	if err != nil {
		fatal(err)
	}
	qr := qualityReport{
		FineMode: fineDecision.Mode, ApproxMode: approxDecision.Mode,
		FineDecision: fineDecision, ApproxDecision: approxDecision, OK: true,
	}
	for i, t := range canonical.Samples {
		fine, err := engine.RenderFrame(fineScene, t, i, fineDecision)
		if err != nil {
			fatal(err)
		}
		approx, err := engine.RenderFrame(engine.ApplyQuality(canonical, approxDecision), t, i, approxDecision)
		if err != nil {
			fatal(err)
		}
		metric := engine.CompareQualityMetrics(fine, approx)
		qr.Metrics = append(qr.Metrics, metric)
		if err := engine.CheckScaleMetric(metric); err != nil {
			qr.OK = false
			qr.Error = err.Error()
		}
	}
	report.Quality = qr
	preview, err := engine.Run(context.Background(), canonical, engine.RunOptions{
		Mode: engine.ModePreview, MaxWorkers: *workers, BufferBytes: *bufferBytes, BudgetBytes: 512 * 1024 * 1024, Quality: &fineDecision,
	})
	if err != nil {
		fatal(err)
	}
	full, err := engine.Run(context.Background(), canonical, engine.RunOptions{
		Mode: engine.ModeFull, DBPath: *dbPath, MaxWorkers: *workers, BufferBytes: *bufferBytes,
		BudgetBytes: 512 * 1024 * 1024, Quality: &fineDecision,
	})
	if err != nil {
		fatal(err)
	}
	resumeDB := filepath.Join(*outDir, "resume.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(resumeDB + suffix)
	}
	first, firstErr := engine.Run(context.Background(), canonical, engine.RunOptions{
		Mode: engine.ModeResume, DBPath: resumeDB, MaxWorkers: *workers, BufferBytes: *bufferBytes,
		BudgetBytes: 512 * 1024 * 1024, Quality: &fineDecision, StopAfter: 2,
	})
	if firstErr != nil && !errors.Is(firstErr, context.Canceled) {
		fatal(firstErr)
	}
	resumed, err := engine.Run(context.Background(), canonical, engine.RunOptions{
		Mode: engine.ModeResume, DBPath: resumeDB, MaxWorkers: *workers, BufferBytes: *bufferBytes,
		BudgetBytes: 512 * 1024 * 1024, Quality: &fineDecision,
	})
	if err != nil {
		fatal(err)
	}
	report.Resume = resumed.SnapshotStatus
	diff, err := engine.CompareRuns(preview, full, resumed)
	report.RunComparison = diff
	if err != nil {
		fatal(err)
	}
	for _, item := range []struct {
		run *engine.RunReport
		dir string
	}{
		{preview, "preview"},
		{full, "full"},
		{first, "resume-interrupted"},
		{resumed, "resume"},
	} {
		run := item.run
		if run == nil {
			continue
		}
		sum := runSummary{Mode: string(run.Mode), Frames: len(run.Frames), CommitSequence: run.CommitSequence, Partial: run.Partial, ElapsedMs: run.ElapsedMillis}
		for _, f := range run.Frames {
			sum.Hashes = append(sum.Hashes, f.ContentHash)
		}
		report.RunReports = append(report.RunReports, sum)
		_ = engine.WriteRunArtifacts(run, filepath.Join(*outDir, "runs", item.dir))
	}
	lossyScene := canonical.Clone()
	lossyScene.Paths[0].Blend = engine.BlendReplace
	lossy, err := engine.ExportScene(lossyScene, engine.Version1, engine.RoundFloor)
	if err != nil {
		fatal(err)
	}
	report.LossyExport = lossy.Warnings
	if len(lossy.Warnings) == 0 {
		fatal(fmt.Errorf("expected E_LOSSY warning for replace blend exported to v1"))
	}
	if _, err := engine.VerifyExport(lossyScene, lossy); err != nil {
		fatal(err)
	}
	report.AllChecksPassed = report.Truth == "ok" && report.Quality.OK && report.RunComparison != nil && report.RunComparison.Consistent && allRoundTripsOK(report.RoundTrip)
	report.Notes = []string{
		"All timestamps use 1000 ticks/ms; the sample clock is 100 fps, so v2 frame conversion is exact.",
		"Truth coverage is an 8x AA reference; renderer verifies a 4x fine grid within the declared 2% absolute tolerance.",
		"Approximate mode is exercised with a 1-byte budget and compared against fine mode frame by frame.",
		"Interrupted resume is validated by stopping after two commits, reopening SQLite, and finishing the remaining times.",
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "verification.json"), data, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("load: %v\n", report.LoadVersions)
	fmt.Printf("truth: %s\n", report.Truth)
	fmt.Printf("quality: fine=%s approx=%s ok=%v\n", report.Quality.FineMode, report.Quality.ApproxMode, report.Quality.OK)
	fmt.Printf("runs: preview=%d full=%d resumed=%d consistent=%v\n", len(preview.Frames), len(full.Frames), len(resumed.Frames), report.RunComparison.Consistent)
	fmt.Printf("lossy warnings: %d\n", len(report.LossyExport))
	fmt.Printf("report: %s\n", filepath.Join(*outDir, "verification.json"))
	if !report.AllChecksPassed {
		os.Exit(2)
	}
}

func allRoundTripsOK(results []roundTripResult) bool {
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "vectorlab:", err)
	os.Exit(1)
}

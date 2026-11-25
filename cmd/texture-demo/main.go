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

	"texturekernel"
	"texturekernel/pipeline"
	"texturekernel/sample"
	"texturekernel/store"
	"texturekernel/verify"
)

type summary struct {
	Sample struct {
		Seed      int64  `json:"seed"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		BitDepth  int    `json:"bit_depth"`
		Layout    string `json:"layout"`
		Chunks    int    `json:"chunks"`
		Entries   int    `json:"entries"`
		PlanHash  string `json:"plan_fingerprint"`
		TruthHash string `json:"truth_fingerprint"`
	} `json:"sample"`
	Versions      []versionCheck         `json:"versions"`
	Paths         map[string]pathSummary `json:"paths"`
	Invalidations int                    `json:"invalidations"`
	Verify        verify.Report          `json:"verify"`
	FirstDiff     string                 `json:"first_diff,omitempty"`
}

type versionCheck struct {
	Version   string `json:"version"`
	Traces    int    `json:"traces"`
	RoundTrip bool   `json:"round_trip"`
	Losses    int    `json:"losses"`
}

type pathSummary struct {
	FinalHash      string                 `json:"final_hash"`
	StageHashes    []pipeline.StageDigest `json:"stage_hashes"`
	Commits        int                    `json:"commits"`
	ReusedChunks   int                    `json:"reused_chunks"`
	ComputedChunks int                    `json:"computed_chunks"`
	Wiped          bool                   `json:"checkpoint_wiped"`
	QueueEvents    int                    `json:"queue_events"`
}

func main() {
	var (
		outDir      = flag.String("out", "artifacts/demo", "output directory")
		seed        = flag.Int64("seed", 42, "deterministic sample seed")
		width       = flag.Int("width", 512, "sample width")
		height      = flag.Int("height", 384, "sample height")
		bitDepth    = flag.Int("bit-depth", 8, "source bit depth: 8 or 16")
		layout      = flag.String("layout", "RGBA", "source layout: RGBA or BGRA")
		chunkWidth  = flag.Int("chunk-width", 128, "tile width")
		chunkHeight = flag.Int("chunk-height", 96, "tile height")
		workers     = flag.Int("workers", 4, "worker pool size")
		bufferBytes = flag.Int64("pixel-buffer", 16<<20, "pixel buffer limit per worker")
		cancelAfter = flag.Int("cancel-after", 3, "cancel the resume run after N committed chunks; 0 disables")
	)
	flag.Parse()

	layoutChannels, err := texture.ParseLayout(*layout)
	if err != nil {
		fatal(err)
	}
	opts := sample.Options{
		Seed: *seed, Width: *width, Height: *height, BitDepth: *bitDepth,
		Layout: layoutChannels, ChunkWidth: *chunkWidth, ChunkHeight: *chunkHeight,
		Workers: *workers, ParamVersion: texture.DefaultParamVersion,
		AlgorithmVersion: texture.AlgorithmVersion,
	}
	bundle, err := sample.Generate(opts)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err)
	}
	if err := sample.WriteBundle(*outDir, bundle); err != nil {
		fatal(err)
	}
	chunks, err := texture.BuildChunks(bundle.Source.Width, bundle.Source.Height, *chunkWidth, *chunkHeight)
	if err != nil {
		fatal(err)
	}
	result := summary{}
	result.Sample.Seed = bundle.Options.Seed
	result.Sample.Width = bundle.Source.Width
	result.Sample.Height = bundle.Source.Height
	result.Sample.BitDepth = bundle.Source.BitDepth
	result.Sample.Layout = texture.LayoutString(bundle.Source.Layout)
	result.Sample.Chunks = len(chunks)
	result.Sample.Entries = len(bundle.Plan.Entries)
	result.Sample.PlanHash = bundle.Plan.Fingerprint
	result.Sample.TruthHash = bundle.Truth.Fingerprint

	for _, version := range []string{"1", "2", "3"} {
		check, err := checkVersion(bundle, version)
		if err != nil {
			fatal(err)
		}
		result.Versions = append(result.Versions, check)
	}

	ctx := context.Background()
	cfg := pipeline.Config{
		ChunkWidth: *chunkWidth, ChunkHeight: *chunkHeight, Workers: *workers,
		PixelBufferPerWorker: *bufferBytes,
	}
	fullStore, err := store.Open(filepath.Join(*outDir, "full.db"))
	if err != nil {
		fatal(err)
	}
	defer fullStore.Close()
	fullRunner, err := pipeline.NewRunner(fullStore, cfg)
	if err != nil {
		fatal(err)
	}
	full, err := fullRunner.Run(ctx, bundle.Source, bundle.Plan, pipeline.Options{Mode: pipeline.ModeFull, RunID: "demo-full"})
	if err != nil {
		fatal(err)
	}
	if full.FinalHash != bundle.Truth.ExpectedFinalHash {
		fatal(fmt.Errorf("full final hash %s does not match truth %s", full.FinalHash, bundle.Truth.ExpectedFinalHash))
	}

	incStore, err := store.Open(filepath.Join(*outDir, "incremental.db"))
	if err != nil {
		fatal(err)
	}
	defer incStore.Close()
	incRunner, err := pipeline.NewRunner(incStore, cfg)
	if err != nil {
		fatal(err)
	}
	if _, err := incRunner.Run(ctx, bundle.Source, bundle.BasePlan, pipeline.Options{Mode: pipeline.ModeFull, RunID: "demo-base"}); err != nil {
		fatal(err)
	}
	incremental, err := incRunner.Run(ctx, bundle.Source, bundle.Plan, pipeline.Options{
		Mode: pipeline.ModeIncremental, RunID: "demo-incremental",
		PreviousRunID: "demo-base", PreviousPlan: bundle.BasePlan,
	})
	if err != nil {
		fatal(err)
	}
	invalidations, err := texture.Invalidate(bundle.BasePlan, bundle.Plan, *chunkWidth, *chunkHeight)
	if err != nil {
		fatal(err)
	}
	result.Invalidations = len(invalidations)

	resumePath := filepath.Join(*outDir, "resume.db")
	resumeStore, err := store.Open(resumePath)
	if err != nil {
		fatal(err)
	}
	var cancel context.CancelFunc
	resumeCtx, cancelRun := context.WithCancel(ctx)
	cancel = cancelRun
	committedBeforeCancel := 0
	resumeCfg := cfg
	effectiveCancel := *cancelAfter
	if effectiveCancel >= len(chunks) {
		effectiveCancel = 0
	}
	if effectiveCancel > 0 {
		resumeCfg.OnCommit = func(ref pipeline.CommitRef) {
			committedBeforeCancel++
			if committedBeforeCancel == effectiveCancel {
				cancel()
			}
		}
	}
	resumeRunner, err := pipeline.NewRunner(resumeStore, resumeCfg)
	if err != nil {
		fatal(err)
	}
	partial, err := resumeRunner.Run(resumeCtx, bundle.Source, bundle.Plan, pipeline.Options{Mode: pipeline.ModeFull, RunID: "demo-resume"})
	if effectiveCancel > 0 {
		if !errors.Is(err, texture.ErrCanceled) {
			fatal(fmt.Errorf("expected cancellation after %d commits, got %v", effectiveCancel, err))
		}
		if partial == nil {
			fatal(errors.New("missing partial result"))
		}
		fmt.Printf("cancel boundary: persisted %d/%d chunks, final image intentionally absent\n",
			len(partial.CommitSequence), len(chunks))
	} else if err != nil {
		fatal(err)
	}
	if err := resumeStore.Close(); err != nil {
		fatal(err)
	}
	resumedStore, err := store.Open(resumePath)
	if err != nil {
		fatal(err)
	}
	defer resumedStore.Close()
	resumedRunner, err := pipeline.NewRunner(resumedStore, cfg)
	if err != nil {
		fatal(err)
	}
	resumed, err := resumedRunner.Run(ctx, bundle.Source, bundle.Plan, pipeline.Options{Mode: pipeline.ModeResume, RunID: "demo-resume"})
	if err != nil {
		fatal(err)
	}

	report, err := pipeline.ComparePaths(bundle.Plan,
		pipeline.PathSnapshot{Name: "full", Final: full.Final, StageHashes: full.StageHashes, CommitSequence: full.CommitSequence},
		pipeline.PathSnapshot{Name: "incremental", Final: incremental.Final, StageHashes: incremental.StageHashes, CommitSequence: incremental.CommitSequence},
		pipeline.PathSnapshot{Name: "resume", Final: resumed.Final, StageHashes: resumed.StageHashes, CommitSequence: resumed.CommitSequence},
	)
	if err != nil {
		if ke, ok := texture.AsKernelError(err); ok && ke.Diff != nil {
			result.FirstDiff = ke.Diff.String()
		}
		fatal(err)
	}
	verifyReport, err := verify.VerifyCheckpoint(ctx, fullStore, "demo-full", bundle.Plan, bundle.Truth)
	if err != nil {
		fatal(err)
	}
	result.Verify = *verifyReport
	result.Paths = map[string]pathSummary{
		"full":        summaryFor(full),
		"incremental": summaryFor(incremental),
		"resume":      summaryFor(resumed),
	}
	writeJSON(filepath.Join(*outDir, "result.json"), result)

	fmt.Printf("sample: %dx%d %d-bit %s, %d entries, %d tiles\n",
		bundle.Source.Width, bundle.Source.Height, bundle.Source.BitDepth,
		texture.LayoutString(bundle.Source.Layout), len(bundle.Plan.Entries), len(chunks))
	fmt.Printf("plan fingerprint: %s\n", bundle.Plan.Fingerprint)
	for _, version := range result.Versions {
		fmt.Printf("plan v%s: traces=%d round_trip=%v losses=%d\n", version.Version, version.Traces, version.RoundTrip, version.Losses)
	}
	fmt.Printf("invalidation manifest: %d entry/tile pairs\n", len(invalidations))
	fmt.Printf("full:        commits=%d final=%s\n", len(full.CommitSequence), full.FinalHash)
	fmt.Printf("incremental: commits=%d computed=%d reused=%d final=%s\n",
		len(incremental.CommitSequence), incremental.ComputedChunks, incremental.ReusedChunks, incremental.FinalHash)
	fmt.Printf("resume:      commits=%d restored=%d wiped=%v final=%s\n",
		len(resumed.CommitSequence), resumed.ReusedChunks, resumed.CheckpointWiped, resumed.FinalHash)
	fmt.Printf("three-path compare: OK (%s), commit sequence length=%d\n", report.FinalHash, len(report.CommitSequence))
	fmt.Printf("independent verify: %d chunks, %d samples, final=%s\n",
		verifyReport.Chunks, verifyReport.SamplesChecked, verifyReport.FinalHash)
	if verifyReport.TruthFingerprint != bundle.Truth.Fingerprint {
		fatal(errors.New("verifier truth fingerprint mismatch"))
	}
	if !strings.EqualFold(report.FinalHash, bundle.Truth.ExpectedFinalHash) {
		fatal(errors.New("three-path hash does not match analytical truth"))
	}
	fmt.Printf("artifacts: %s\n", *outDir)
}

func checkVersion(bundle *sample.Bundle, version string) (versionCheck, error) {
	file := "plan-v" + version + ".json"
	data, ok := bundle.PlanFiles[file]
	if !ok {
		return versionCheck{}, fmt.Errorf("missing %s", file)
	}
	loaded, err := texture.LoadPlan(data)
	if err != nil {
		return versionCheck{}, err
	}
	if err := texture.EquivalentForExport(bundle.Plan, loaded); err != nil {
		return versionCheck{}, err
	}
	lossCount := 0
	for _, targetVersion := range []string{"1", "2", "3"} {
		exported, losses, exportErr := texture.ExportPlan(loaded, targetVersion)
		if exportErr != nil && !texture.HasCode(exportErr, texture.CodeLossy) {
			return versionCheck{}, exportErr
		}
		lossCount += len(losses)
		reloaded, err := texture.LoadPlan(exported)
		if err != nil {
			return versionCheck{}, err
		}
		if err := texture.EquivalentForExport(loaded, reloaded); err != nil {
			return versionCheck{}, err
		}
	}
	traceCount := len(loaded.Traces)
	for _, entry := range loaded.Entries {
		traceCount += len(entry.Traces)
	}
	return versionCheck{Version: version, Traces: traceCount, RoundTrip: true, Losses: lossCount}, nil
}

func summaryFor(result *pipeline.Result) pathSummary {
	if result == nil {
		return pathSummary{}
	}
	return pathSummary{
		FinalHash: result.FinalHash, StageHashes: result.StageHashes,
		Commits: len(result.CommitSequence), ReusedChunks: result.ReusedChunks,
		ComputedChunks: result.ComputedChunks, Wiped: result.CheckpointWiped,
		QueueEvents: len(result.QueueEvents),
	}
}

func writeJSON(path string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "texture-demo: %v\n", err)
	os.Exit(1)
}

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	fog "volumefog"
)

type report struct {
	GeneratedAt       string                      `json:"generated_at"`
	GoVersion         string                      `json:"go_version"`
	Seed              int64                       `json:"seed"`
	SceneFile         string                      `json:"scene_file"`
	DBPath            string                      `json:"db_path"`
	DBBackend         string                      `json:"db_backend"`
	TruthCheck        bool                        `json:"truth_check"`
	TruthError        string                      `json:"truth_error,omitempty"`
	Preview           frameSummary                `json:"preview"`
	Exact             frameSummary                `json:"exact"`
	PreviewExact      bool                        `json:"preview_exact_consistent"`
	PreviewExactError string                      `json:"preview_exact_error,omitempty"`
	ThreePaths        *fog.ThreePathReport        `json:"three_paths"`
	StepBudget        []fog.StepBudgetProbe       `json:"step_budget"`
	Faults            map[string]*fog.FaultReport `json:"faults"`
	Invalidation      invalidationSummary         `json:"invalidation"`
	Passed            bool                        `json:"passed"`
	Failures          []string                    `json:"failures,omitempty"`
}

type frameSummary struct {
	Mode              string  `json:"mode"`
	ElapsedMS         float64 `json:"elapsed_ms"`
	WithinLatency     bool    `json:"within_latency"`
	LatencyLimitMS    int     `json:"latency_limit_ms"`
	CacheHits         int     `json:"cache_hits"`
	ComputedChunks    int     `json:"computed_chunks"`
	InvalidatedChunks int     `json:"invalidated_chunks"`
	StepsUsed         int     `json:"steps_used"`
	ShadowSamplesUsed int     `json:"shadow_samples_used"`
	Chunks            int     `json:"chunks"`
}

type invalidationSummary struct {
	Media          string             `json:"media"`
	Voxel          string             `json:"voxel"`
	OldValue       int                `json:"old_value"`
	NewValue       int                `json:"new_value"`
	AffectedChunks int                `json:"affected_chunks"`
	CacheHits      int                `json:"unaffected_cache_hits"`
	Recomputed     int                `json:"recomputed_chunks"`
	Invalidations  []fog.Invalidation `json:"invalidations"`
}

func summarize(f *fog.Frame) frameSummary {
	if f == nil {
		return frameSummary{}
	}
	return frameSummary{
		Mode: f.Mode, ElapsedMS: f.ElapsedMS, WithinLatency: f.WithinLatency,
		LatencyLimitMS: f.LatencyLimitMS, CacheHits: f.Stats.CacheHits,
		ComputedChunks: f.Stats.ComputedChunks, InvalidatedChunks: f.Stats.InvalidatedChunks,
		StepsUsed: f.Stats.StepsUsed, ShadowSamplesUsed: f.Stats.ShadowSamplesUsed,
		Chunks: len(f.Chunks),
	}
}

func main() {
	outDir := flag.String("out", "artifacts/validation", "validation output directory")
	seed := flag.Int64("seed", fog.DefaultSeed, "scene seed")
	flag.Parse()
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	dbPath := filepath.Join(*outDir, "checkpoints.db")
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(p)
	}
	scene, err := fog.GenerateScene(fog.GenerateOptions{
		Seed: *seed, Width: 32, Height: 16, ChunkSize: 8,
	})
	if err != nil {
		log.Fatalf("generate: %v", err)
	}
	sceneFile := filepath.Join(*outDir, "scene.json")
	if err := writeScene(sceneFile, scene); err != nil {
		log.Fatal(err)
	}
	wb, err := fog.NewWorkbench(scene, dbPath, *outDir)
	if err != nil {
		log.Fatal(err)
	}
	defer wb.Close()
	rep := report{
		GeneratedAt: time.Now().Format(time.RFC3339), GoVersion: runtime.Version(),
		Seed: scene.Seed, SceneFile: sceneFile, DBPath: dbPath,
		DBBackend: wb.Store.Backend(), Faults: map[string]*fog.FaultReport{},
	}
	addFailure := func(format string, args ...any) {
		rep.Failures = append(rep.Failures, fmt.Sprintf(format, args...))
	}
	if err := wb.ValidateTruth(); err != nil {
		rep.TruthError = err.Error()
		addFailure("truth: %v", err)
	} else {
		rep.TruthCheck = true
	}
	preview, perr := wb.Preview()
	rep.Preview = summarize(preview)
	if perr != nil {
		addFailure("preview: %v", perr)
	}
	exact, eerr := wb.Exact()
	rep.Exact = summarize(exact)
	if eerr != nil {
		rep.PreviewExactError = eerr.Error()
		addFailure("exact/consistency: %v", eerr)
	} else {
		rep.PreviewExact = true
	}
	if preview != nil && exact != nil {
		if err := fog.ComparePreviewExact(preview, exact); err != nil {
			rep.PreviewExact = false
			rep.PreviewExactError = err.Error()
			addFailure("preview/exact: %v", err)
		} else {
			rep.PreviewExact = true
		}
	}
	probeRay := longestSampleRay(scene)
	if probes, err := fog.ProbeStepBudgets(scene, probeRay,
		[]int{8, 16, 32, 64, 96, 128}, scene.Recipe.ShadowSamples); err != nil {
		addFailure("step budget: %v", err)
	} else {
		rep.StepBudget = probes
		for i := 1; i < len(probes); i++ {
			if probes[i].Transmittance > probes[i-1].Transmittance+1e-15 {
				addFailure("step transmittance increased at %d", probes[i].Steps)
			}
			if probes[i].ErrorBound > probes[i-1].ErrorBound+1e-15 {
				addFailure("error bound did not tighten at %d", probes[i].Steps)
			}
			if probes[i].RadianceDeviation > probes[i].ErrorBound+1e-12 {
				addFailure("radiance deviation exceeds bound at %d", probes[i].Steps)
			}
		}
	}
	rep.ThreePaths = wb.CompareThreePaths()
	if !rep.ThreePaths.Passed {
		addFailure("three paths: %s", rep.ThreePaths.Error)
	}
	for _, kind := range []string{"decode", "order", "dens", "block"} {
		rep.Faults[kind] = wb.DiagnoseFault(kind, 0, 0)
	}
	if rep.Faults["decode"].Error == nil || rep.Faults["decode"].Error.Code != fog.E_DECODE {
		addFailure("decode fault chain")
	}
	if rep.Faults["order"].Error == nil || rep.Faults["order"].Error.Code != fog.E_ORDER {
		addFailure("order fault chain")
	}
	if rep.Faults["dens"].Error == nil || rep.Faults["dens"].Error.Code != fog.E_DENS {
		addFailure("density fault chain")
	}
	if rep.Faults["block"].Error == nil || rep.Faults["block"].Error.Code != fog.E_BLOCK || !rep.Faults["block"].RetryOK {
		addFailure("block fault/retry chain")
	}
	if err := runInvalidation(&rep, wb); err != nil {
		addFailure("invalidation: %v", err)
	}
	rep.Passed = len(rep.Failures) == 0
	data, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "validation-report.json"), data, 0o644); err != nil {
		log.Fatal(err)
	}
	text := summaryText(rep)
	if err := os.WriteFile(filepath.Join(*outDir, "validation-summary.txt"), []byte(text), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Print(text)
	if !rep.Passed {
		os.Exit(1)
	}
}

func writeScene(path string, scene *fog.Scene) error {
	data, err := fog.MarshalSceneJSON(scene)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func runInvalidation(rep *report, wb *fog.Workbench) error {
	frame := wb.LastExact
	if frame == nil {
		return fmt.Errorf("no exact frame")
	}
	var dep fog.DependencyRef
	found := false
	for _, ch := range frame.Chunks {
		for _, d := range ch.Dependencies {
			if d.Kind == "density" {
				dep, found = d, true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		return fmt.Errorf("no density dependency")
	}
	m, _ := wb.Scene.MediumByID(dep.MediaID)
	x, y, z, ok := m.DensityXYZ(dep.Voxel)
	if !ok {
		return fmt.Errorf("bad voxel index")
	}
	old := m.Density[dep.Voxel]
	newValue := (old + 73) % (fog.MaxDensity + 1)
	invs, after, err := wb.EditAndRecomputeVoxel(dep.MediaID, x, y, z, newValue)
	if err != nil {
		return err
	}
	rep.Invalidation = invalidationSummary{
		Media: dep.MediaID, Voxel: fmt.Sprintf("%d,%d,%d", x, y, z),
		OldValue: old, NewValue: newValue, AffectedChunks: len(invs),
		CacheHits: after.Stats.CacheHits, Recomputed: after.Stats.ComputedChunks,
		Invalidations: invs,
	}
	if len(invs) == 0 {
		return fmt.Errorf("edit affected no chunk")
	}
	if after.Stats.CacheHits != len(frame.Chunks)-len(invs) {
		return fmt.Errorf("cache hits=%d want %d", after.Stats.CacheHits, len(frame.Chunks)-len(invs))
	}
	return nil
}

func summaryText(rep report) string {
	status := "PASS"
	if !rep.Passed {
		status = "FAIL"
	}
	var b string
	b += fmt.Sprintf("Volume Fog acceptance: %s\n", status)
	b += fmt.Sprintf("Go: %s\nSeed: %d\nSQLite backend: %s\n", rep.GoVersion, rep.Seed, rep.DBBackend)
	b += fmt.Sprintf("Truth: %v\n", rep.TruthCheck)
	b += fmt.Sprintf("Preview: %.2fms (cap %dms), steps=%d shadow=%d\n",
		rep.Preview.ElapsedMS, rep.Preview.LatencyLimitMS, rep.Preview.StepsUsed, rep.Preview.ShadowSamplesUsed)
	b += fmt.Sprintf("Exact: %.2fms, chunks=%d, steps=%d shadow=%d\n",
		rep.Exact.ElapsedMS, rep.Exact.Chunks, rep.Exact.StepsUsed, rep.Exact.ShadowSamplesUsed)
	b += fmt.Sprintf("Preview/exact consistent: %v\n", rep.PreviewExact)
	if rep.ThreePaths != nil {
		b += fmt.Sprintf("Three paths: passed=%v chunks=%d sequences=%v / %v / %v\n",
			rep.ThreePaths.Passed, rep.ThreePaths.Chunks, rep.ThreePaths.FullSequence,
			rep.ThreePaths.RealtimeSequence, rep.ThreePaths.ResumeSequence)
		b += fmt.Sprintf("Realtime budget waits=%d delayed=%d peak=%d/%d\n",
			rep.ThreePaths.RealtimeBudget.WaitCount, rep.ThreePaths.RealtimeBudget.DelayedChunks,
			rep.ThreePaths.RealtimeBudget.Peak, rep.ThreePaths.RealtimeBudget.Capacity)
	}
	for _, p := range rep.StepBudget {
		b += fmt.Sprintf("  steps=%3d T=%.9f dT=%.3g dL=%.3g bound=%.3g\n",
			p.Steps, p.Transmittance, p.TransmittanceDeviation, p.RadianceDeviation, p.ErrorBound)
	}
	for _, kind := range []string{"decode", "order", "dens", "block"} {
		if f := rep.Faults[kind]; f != nil {
			code := "ok"
			chunk := ""
			if f.Error != nil {
				code = string(f.Error.Code)
			}
			if f.Chunk != nil {
				chunk = fmt.Sprintf(" chunk=(%d,%d)", f.Chunk.X, f.Chunk.Y)
			}
			b += fmt.Sprintf("Fault %-6s: %s retry_ok=%v%s\n", kind, code, f.RetryOK, chunk)
		}
	}
	b += fmt.Sprintf("Invalidation: media=%s voxel=%s %d->%d affected=%d cache_hits=%d recomputed=%d\n",
		rep.Invalidation.Media, rep.Invalidation.Voxel, rep.Invalidation.OldValue,
		rep.Invalidation.NewValue, rep.Invalidation.AffectedChunks,
		rep.Invalidation.CacheHits, rep.Invalidation.Recomputed)
	if len(rep.Failures) > 0 {
		b += "Failures:\n"
		for _, f := range rep.Failures {
			b += "  - " + f + "\n"
		}
	}
	return b
}

func longestSampleRay(scene *fog.Scene) fog.Ray {
	best := fog.SampleRays(scene)[0]
	bestSegments := -1
	for _, r := range fog.SampleRays(scene) {
		segments := len(fog.RaySegments(scene.Media, r))
		if segments > bestSegments {
			best, bestSegments = r, segments
		}
	}
	return best
}

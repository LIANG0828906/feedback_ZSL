package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"volumediag/internal/diag"
	"volumediag/internal/engine"
	"volumediag/internal/model"
	"volumediag/internal/scene"
	"volumediag/internal/volume"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "render":
		err = runRender(os.Args[2:])
	case "inspect":
		err = runInspect(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if de := diag.As(err); de != nil {
			fmt.Fprintln(os.Stderr, diag.ErrorJSON(de))
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  volumediag generate -out DIR [-seed N] [-width N] [-height N]
  volumediag verify -scene DIR [-db FILE] [-report FILE]
  volumediag render -scene DIR -db FILE [-mode full|preview|resume]
  volumediag inspect -scene DIR -db FILE`)
}

func runGenerate(args []string) error {
	opts := scene.DefaultGenerateOptions()
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "artifacts/sample", "output directory")
	fs.Int64Var(&opts.Seed, "seed", opts.Seed, "deterministic seed")
	fs.IntVar(&opts.Width, "width", opts.Width, "viewport width")
	fs.IntVar(&opts.Height, "height", opts.Height, "viewport height")
	fs.IntVar(&opts.ReferenceSteps, "reference-steps", opts.ReferenceSteps, "truth integration steps")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := scene.Generate(*out, opts)
	if err != nil {
		return err
	}
	fmt.Printf("generated=%s\nsource_hash=%s\nscene_hash=%s\npixels=%d probes=%d\n",
		*out, bundle.SourceHash, bundle.SceneHash, len(bundle.Truth.Pixels), len(bundle.Truth.Rays))
	return nil
}

func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	sceneDir := fs.String("scene", "artifacts/sample", "scene directory")
	dbPath := fs.String("db", "artifacts/render.db", "SQLite checkpoint path")
	mode := fs.String("mode", "full", "full, preview, resume or incremental")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := model.LoadBundle(*sceneDir)
	if err != nil {
		return err
	}
	params := volume.DefaultParams()
	if *mode == "preview" {
		params = volume.PreviewParams()
	}
	eng, err := engine.Open(bundle, params, *dbPath)
	if err != nil {
		return err
	}
	defer eng.Close()
	report, err := eng.Render(context.Background(), engine.Mode(*mode), engine.RenderOptions{})
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(b))
	if !report.Complete && !report.Paused {
		return diag.New(diag.EBlock, "render did not complete")
	}
	return nil
}

func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	sceneDir := fs.String("scene", "artifacts/sample", "scene directory")
	dbPath := fs.String("db", "artifacts/render.db", "SQLite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := model.LoadBundle(*sceneDir)
	if err != nil {
		return err
	}
	eng, err := engine.Open(bundle, volume.DefaultParams(), *dbPath)
	if err != nil {
		return err
	}
	defer eng.Close()
	b, _ := json.MarshalIndent(eng.State(), "", "  ")
	fmt.Println(string(b))
	return nil
}

type stepReport struct {
	Name            string                `json:"name"`
	Edit            engine.EditRequest    `json:"edit"`
	AffectedBlocks  int                   `json:"affected_blocks"`
	ReusedBlocks    int                   `json:"reused_blocks"`
	Invalidated     []engine.Invalidation `json:"invalidated"`
	Render          *engine.RenderReport  `json:"render"`
	Compare         *engine.CompareReport `json:"compare"`
	CompareDuration float64               `json:"compare_duration_ms"`
}

type previewReport struct {
	SameQuality              *engine.CompareReport `json:"same_quality_compare"`
	ReducedSameParamsCompare *engine.CompareReport `json:"reduced_same_params_compare"`
	ReducedParams            volume.Params         `json:"reduced_params"`
	ReducedQuality           volume.Quality        `json:"reduced_quality"`
	ActualMaxPixelDiff       float64               `json:"actual_max_pixel_diff"`
	ActualMaxVolumeDiff      float64               `json:"actual_max_volume_diff"`
	WithinBound              bool                  `json:"within_bound"`
	ReducedRender            *engine.RenderReport  `json:"reduced_render"`
}

type resumeReport struct {
	PausedReport   *engine.RenderReport  `json:"paused_report"`
	RestoredBlocks int                   `json:"restored_blocks"`
	Invalidated    bool                  `json:"invalidated"`
	ResumedReport  *engine.RenderReport  `json:"resumed_report"`
	Compare        *engine.CompareReport `json:"compare"`
	Equal          bool                  `json:"equal"`
}

type verifyReport struct {
	StartedAt  time.Time            `json:"started_at"`
	DurationMS float64              `json:"duration_ms"`
	SourceHash string               `json:"source_hash"`
	SceneHash  string               `json:"scene_hash"`
	Full       *engine.RenderReport `json:"full"`
	Truth      *engine.TruthReport  `json:"truth"`
	Steps      []stepReport         `json:"incremental_steps"`
	Preview    previewReport        `json:"preview"`
	Resume     resumeReport         `json:"resume"`
	Passed     bool                 `json:"passed"`
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	sceneDir := fs.String("scene", "artifacts/sample", "scene directory")
	dbPath := fs.String("db", "artifacts/verification.db", "verification SQLite path")
	reportPath := fs.String("report", "artifacts/verification.json", "JSON run report")
	workers := fs.Int("workers", 4, "max worker count")
	step := fs.Float64("step", 0.05, "ray march step")
	batch := fs.Int("batch-size", 8, "checkpoint batch size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	started := time.Now()
	bundle, err := model.LoadBundle(*sceneDir)
	if err != nil {
		return err
	}
	params := volume.DefaultParams()
	params.Step = *step
	params.MaxWorkers = *workers
	params.BatchSize = *batch
	removeDB(*dbPath)
	eng, err := engine.Open(bundle, params, *dbPath)
	if err != nil {
		return err
	}
	defer eng.Close()
	ctx := context.Background()
	full, err := eng.Render(ctx, engine.ModeFull, engine.RenderOptions{})
	if err != nil {
		return err
	}
	if !full.Complete {
		return diag.New(diag.EBlock, "verification full render incomplete")
	}
	truth, err := eng.VerifyTruth(ctx)
	if err != nil {
		return fmt.Errorf("truth verification: %w", err)
	}
	report := &verifyReport{
		StartedAt:  started,
		SourceHash: bundle.SourceHash,
		SceneHash:  bundle.SceneHash,
		Full:       full,
		Truth:      truth,
	}

	newIntensity := model.C3(5.1, 3.9, 2.9)
	spotDir := model.V3(-0.9, -1.0, -0.25)
	newScatter := model.C3(0.082, 0.072, 0.064)
	phaseG := 0.27
	depths := [][]float64{
		{0.00, 0.01, -0.09, 0.00},
		{0.01, 0.07, 0.00, -0.05},
		{0.00, 0.01, 0.04, 0.02},
	}
	edits := []struct {
		name string
		req  engine.EditRequest
	}{
		{"edit_point_intensity", engine.EditRequest{LightID: "point_key", Intensity: &newIntensity}},
		{"edit_spot_direction", engine.EditRequest{LightID: "spot_rim", Direction: &spotDir}},
		{"edit_medium_scattering_and_phase", engine.EditRequest{Medium: &engine.MediumEdit{Scattering: &newScatter, PhaseG: &phaseG}}},
		{"edit_occluder_depth_mesh", engine.EditRequest{OccluderID: "occluder_back", Depths: depths}},
	}
	allPassed := true
	for _, item := range edits {
		editReport, editErr := eng.ApplyEdit(item.req)
		if editErr != nil {
			return fmt.Errorf("%s: %w", item.name, editErr)
		}
		inc, renderErr := eng.Render(ctx, engine.ModeIncremental, engine.RenderOptions{})
		if renderErr != nil {
			return fmt.Errorf("%s render: %w", item.name, renderErr)
		}
		if !inc.Complete {
			return fmt.Errorf("%s incremental render incomplete", item.name)
		}
		cmpStart := time.Now()
		cmp, cmpErr := eng.VerifyAgainstFull(ctx)
		cmpDuration := float64(time.Since(cmpStart).Microseconds()) / 1000
		if cmpErr != nil {
			return fmt.Errorf("%s compare: %w", item.name, cmpErr)
		}
		report.Steps = append(report.Steps, stepReport{
			Name:            item.name,
			Edit:            item.req,
			AffectedBlocks:  editReport.AffectedBlocks,
			ReusedBlocks:    editReport.ReusedBlocks,
			Invalidated:     editReport.Invalidated,
			Render:          inc,
			Compare:         cmp,
			CompareDuration: cmpDuration,
		})
		allPassed = allPassed && cmp.Equal
	}

	base, err := engine.Open(bundle, params, "")
	if err != nil {
		return err
	}
	previewSame, err := base.Render(ctx, engine.ModePreview, engine.RenderOptions{})
	if err != nil {
		return err
	}
	previewCompare, err := base.VerifyAgainstFull(ctx)
	if err != nil {
		return fmt.Errorf("preview same-quality compare: %w", err)
	}
	_ = previewSame
	_ = base.Close()

	reducedParams := volume.PreviewParams()
	reducedParams.MaxWorkers = *workers
	reducedParams.BatchSize = *batch
	previewEng, err := engine.Open(bundle, reducedParams, "")
	if err != nil {
		return err
	}
	reduced, err := previewEng.Render(ctx, engine.ModePreview, engine.RenderOptions{})
	if err != nil {
		return err
	}
	reducedSelfCompare, err := previewEng.VerifyAgainstFull(ctx)
	if err != nil {
		return fmt.Errorf("reduced preview same-params compare: %w", err)
	}
	fullBase, err := engine.Open(bundle, params, "")
	if err != nil {
		return err
	}
	if _, err := fullBase.Render(ctx, engine.ModeFull, engine.RenderOptions{}); err != nil {
		return err
	}
	maxPixel, maxVolume := maxCacheDifference(previewEng.Cache(), fullBase.Cache())
	bound := reduced.Quality.ErrorBoundPerPixel
	within := maxPixel <= bound+1e-12
	report.Preview = previewReport{
		SameQuality:              previewCompare,
		ReducedSameParamsCompare: reducedSelfCompare,
		ReducedParams:            reducedParams,
		ReducedQuality:           reduced.Quality,
		ActualMaxPixelDiff:       maxPixel,
		ActualMaxVolumeDiff:      maxVolume,
		WithinBound:              within,
		ReducedRender:            reduced,
	}
	allPassed = allPassed && previewCompare.Equal && reducedSelfCompare.Equal && within
	_ = previewEng.Close()
	_ = fullBase.Close()

	resumeDB := *dbPath + ".resume"
	removeDB(resumeDB)
	resumeEng, err := engine.Open(bundle, params, resumeDB)
	if err != nil {
		return err
	}
	paused, err := resumeEng.Render(ctx, engine.ModeResume, engine.RenderOptions{PauseAfterBatches: 3})
	if err != nil {
		return err
	}
	if !paused.Paused {
		return diag.New(diag.EBlock, "resume fixture did not pause")
	}
	partialCount := len(resumeEng.Cache())
	if err := resumeEng.Close(); err != nil {
		return err
	}
	resumeEng, err = engine.Open(bundle, params, resumeDB)
	if err != nil {
		return err
	}
	state := resumeEng.State()
	resumed, err := resumeEng.Render(ctx, engine.ModeResume, engine.RenderOptions{})
	if err != nil {
		return err
	}
	resumeCompare, err := resumeEng.VerifyAgainstFull(ctx)
	if err != nil {
		return fmt.Errorf("resume compare: %w", err)
	}
	report.Resume = resumeReport{
		PausedReport:   paused,
		RestoredBlocks: state.CacheCount,
		Invalidated:    state.Restored.Invalidated,
		ResumedReport:  resumed,
		Compare:        resumeCompare,
		Equal:          resumeCompare.Equal && state.CacheCount == partialCount && resumed.Complete,
	}
	allPassed = allPassed && report.Resume.Equal
	_ = resumeEng.Close()

	report.Passed = allPassed
	report.DurationMS = float64(time.Since(started).Microseconds()) / 1000
	if err := writeJSON(*reportPath, report); err != nil {
		return err
	}
	fmt.Printf("verify passed=%v duration_ms=%.3f report=%s\n", report.Passed, report.DurationMS, *reportPath)
	fmt.Printf("truth pixels=%d rays=%d max_pixel_error=%.6g max_scatter_error=%.6g\n",
		truth.PixelsCompared, truth.RaysCompared, truth.MaxPixelError, truth.MaxScatterError)
	for _, step := range report.Steps {
		fmt.Printf("step=%s affected=%d reused=%d equal=%v\n", step.Name, step.AffectedBlocks, step.ReusedBlocks, step.Compare.Equal)
	}
	fmt.Printf("preview_equal=%v reduced_same_params_equal=%v reduced_diff=%.6g bound=%.6g within_bound=%v\n",
		previewCompare.Equal, reducedSelfCompare.Equal, maxPixel, bound, within)
	fmt.Printf("resume_equal=%v restored_blocks=%d\n", report.Resume.Equal, report.Resume.RestoredBlocks)
	if !report.Passed {
		return diag.New(diag.EDiff, "verification did not pass")
	}
	return nil
}

func maxCacheDifference(a, b map[volume.BlockKey]*volume.BlockResult) (maxPixel, maxVolume float64) {
	for key, ab := range a {
		bb := b[key]
		if bb == nil {
			continue
		}
		for i := range ab.Pixels {
			for c := 0; c < 3; c++ {
				maxVolume = math.Max(maxVolume, math.Abs(ab.Pixels[i].Volume[c]-bb.Pixels[i].Volume[c]))
			}
		}
		for i := range ab.Composite {
			for c := 0; c < 3; c++ {
				maxPixel = math.Max(maxPixel, math.Abs(ab.Composite[i][c]-bb.Composite[i][c]))
			}
		}
	}
	return maxPixel, maxVolume
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func removeDB(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

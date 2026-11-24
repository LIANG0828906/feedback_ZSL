package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"occlusionlab/internal/generator"
	"occlusionlab/internal/model"
	"occlusionlab/internal/verify"
)

func main() {
	seed := flag.Int64("seed", 1042, "deterministic project seed")
	raster := flag.Int("raster", 128, "occluder raster resolution")
	workers := flag.Int("workers", 2, "maximum culling workers")
	buffer := flag.Int("buffer-cells", 128*128, "raster cells per worker")
	resident := flag.Int("resident", 15, "maximum resident chunk slots")
	budget := flag.Int("load-budget", 1000, "per-window load cost budget")
	tolerancePx := flag.Float64("tolerance-px", 2, "declared projection tolerance in pixels")
	out := flag.String("out", "artifacts/validation-report.json", "JSON report path")
	flag.Parse()

	cfg := generator.DefaultConfig(*seed)
	cfg.TolerancePxQ = int64(*tolerancePx * float64(int64(1)<<cfg.Quantization.ScreenFracBits))
	project, appErr := generator.Generate(cfg)
	if appErr != nil {
		fmt.Fprintln(os.Stderr, appErr)
		os.Exit(1)
	}
	params := model.Params{
		Quantization:      project.Meta.Precision,
		RasterResolution:  *raster,
		MaxWorkers:        *workers,
		RasterBufferCells: *buffer,
		MaxResident:       *resident,
		LoadCostBudget:    *budget,
		TolerancePxQ:      project.Meta.TolerancePxQ,
		Version:           1,
	}
	report, appErr := verify.Run(context.Background(), project, params, "")
	if appErr != nil {
		fmt.Fprintln(os.Stderr, appErr)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	data = append(data, '\n')
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	bits := project.Meta.Precision.ScreenFracBits
	fmt.Printf("seed=%d project=%s windows=%d chunks=%d raster=%d workers=%d\n",
		report.Seed, report.ProjectHash[:12], report.WindowCount, report.ChunkCount,
		report.RasterResolution, params.MaxWorkers)
	fmt.Printf("projection: %d/%d within tolerance, max_error=%s px, max_bound=%s px, bound_violations=%d\n",
		report.Projection.WithinTolerance, report.Projection.KnownCount,
		model.FormatQ(report.Projection.MaxErrorQ, bits),
		model.FormatQ(report.Projection.MaxBoundQ, bits),
		report.Projection.BoundViolations)
	fmt.Printf("visibility: truth_visible=%d truth_hidden=%d observed_v/u/o=%d/%d/%d conflicts=%d conservative_match=%d\n",
		report.Visibility.TruthVisible, report.Visibility.TruthHidden,
		report.Visibility.ObservedVisible, report.Visibility.ObservedUncertain,
		report.Visibility.ObservedOccluded, report.Visibility.Conflicts,
		report.Visibility.ConservativeMatch)
	fmt.Printf("error: residual=%s projection=%s plane_world=%s coverage=%s depth_world=%s cumulative=%s\n",
		model.FormatQ(report.ErrorBounds.MaxResidualQ, bits),
		model.FormatQ(report.ErrorBounds.MaxProjectionErrorQ, bits),
		model.FormatQ(report.ErrorBounds.MaxPlaneErrorWorldQ, 8),
		model.FormatQ(report.ErrorBounds.MaxCoverageErrorQ, bits),
		model.FormatQ(report.ErrorBounds.MaxDepthErrorWorldQ, 8),
		model.FormatQ(report.ErrorBounds.MaxCumulativeBoundQ, bits))
	fmt.Printf("paths_equal=%v checkpoint_verified=%v differences=%d\n",
		report.ThreePaths.Equal, report.ThreePaths.CheckpointVerified,
		len(report.ThreePaths.Differences))
	for _, check := range report.Checks {
		status := "PASS"
		if !check.Passed {
			status = "FAIL"
		}
		fmt.Printf("  [%s] %-38s %s\n", status, check.Name, check.Detail)
	}
	fmt.Printf("report=%s passed=%v\n", *out, report.Passed)
	if !report.Passed {
		os.Exit(1)
	}
}

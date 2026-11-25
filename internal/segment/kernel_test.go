package segment_test

import (
	"testing"

	"maskpreview/internal/core"
	"maskpreview/internal/sample"
	"maskpreview/internal/segment"
)

func TestGeneratedSamplesExactAgainstTruth(t *testing.T) {
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.GenerateConfig{Seed: 424242, OutDir: dir, Sizes: []int{64, 96, 144}})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, spec := range manifest.Samples {
		spec := spec
		t.Run(spec.ID, func(t *testing.T) {
			loaded, err := sample.LoadSample(dir, spec)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			snapshot, err := core.NewSnapshot(spec.ID, spec.ImageSHA256, spec.DefaultParams, spec.Seeds, spec.Quantization)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			exact, err := segment.Exact(loaded.Image, snapshot)
			if err != nil {
				t.Fatalf("exact: %v", err)
			}
			if len(exact.Regions) != spec.RegionCount {
				t.Fatalf("region count = %d, want %d", len(exact.Regions), spec.RegionCount)
			}
			if got := core.HashBytes(exact.Mask); got != spec.MaskHash {
				t.Fatalf("final mask hash = %s, truth = %s", got, spec.MaskHash)
			}
			report, err := segment.CompareToTruth(exact.Regions, spec.Regions, spec.PreviewTolerance)
			if err != nil {
				t.Fatalf("compare truth: %v", err)
			}
			t.Logf("truth deviation: max_centroid=%.3f max_area_rel=%.4f", report.MaxCentroidDeviation, report.MaxAreaRelative)
			if !report.WithinTolerance {
				t.Fatalf("truth deviation exceeds declared tolerance: %+v", report)
			}
			for _, region := range exact.Regions {
				if len(region.Contour) < 4 || region.Contour[0] != region.Contour[len(region.Contour)-1] {
					t.Fatalf("region %d contour is not closed: %v", region.ID, region.Contour)
				}
			}
			preview, err := segment.Preview(loaded.Image, snapshot, spec.PreviewDelayMS)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			if !preview.WithinBudget {
				t.Fatalf("preview took %.3fms, budget %dms", preview.ElapsedMS, spec.PreviewDelayMS)
			}
			comparison, err := segment.ComparePreview(exact, preview, spec.PreviewTolerance)
			if err != nil {
				t.Fatalf("compare preview: %v\nexact=%+v\npreview=%+v", err, exact.Regions, preview.ProjectedRegions)
			}
			t.Logf("preview: factor=%d iterations=%d elapsed=%.3fms count=%d centroid=%.3f area_rel=%.4f",
				preview.DownsampleFactor, preview.GrowthIterations, preview.ElapsedMS,
				comparison.PreviewRegionCount, comparison.CentroidDeviation, comparison.AreaRelative)
			if !comparison.OK {
				t.Fatalf("preview diverged: cmp=%+v exact=%+v preview=%+v", comparison, exact.Regions, preview.ProjectedRegions)
			}
		})
	}
}

func TestPreviewDivergenceIsStructured(t *testing.T) {
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.GenerateConfig{Seed: 7, OutDir: dir, Sizes: []int{64}})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	spec := manifest.Samples[0]
	loaded, err := sample.LoadSample(dir, spec)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	snapshot, err := core.NewSnapshot(spec.ID, spec.ImageSHA256, spec.DefaultParams, spec.Seeds, spec.Quantization)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	exact, err := segment.Exact(loaded.Image, snapshot)
	if err != nil {
		t.Fatalf("exact: %v", err)
	}
	preview, err := segment.Preview(loaded.Image, snapshot, spec.PreviewDelayMS)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	preview.ProjectedRegions = append([]segment.RegionStat(nil), preview.ProjectedRegions...)
	preview.ProjectedRegions[0].Centroid.X += 20
	preview.ProjectedRegions[0].Area *= 2
	cmp, err := segment.ComparePreview(exact, preview, spec.PreviewTolerance)
	if core.CodeOf(err) != core.CodeDiverge {
		t.Fatalf("error = %v, want E_DIVERGE", err)
	}
	if cmp == nil || !cmp.HasPixelDifference || cmp.FirstRegionID == 0 || cmp.DifferenceX < 0 || cmp.DifferenceY < 0 {
		t.Fatalf("divergence detail missing: %+v", cmp)
	}
	appErr := err.(*core.Error)
	for _, key := range []string{"region_count_delta", "centroid_deviation", "area_relative", "first_region_id", "difference_x", "difference_y"} {
		if _, ok := appErr.Data[key]; !ok {
			t.Fatalf("E_DIVERGE missing %q: %v", key, err)
		}
	}
}

func TestRegionCountMismatchListsExtras(t *testing.T) {
	predicted := []segment.RegionStat{
		{ID: 1, Area: 20, Centroid: core.PointF{X: 2, Y: 2}},
		{ID: 2, Area: 30, Centroid: core.PointF{X: 20, Y: 20}},
	}
	truth := []core.TruthRegion{{ID: 1, Area: 21, Centroid: core.PointF{X: 2, Y: 2}}}
	_, err := segment.CompareToTruth(predicted, truth, core.PreviewTolerance{CentroidPx: 3, AreaRelative: 0.2})
	if core.CodeOf(err) != core.CodeRegion {
		t.Fatalf("error = %v, want E_REGION", err)
	}
	appErr := err.(*core.Error)
	if appErr.Data["extra_regions"] == nil || appErr.Data["missing_regions"] == nil {
		t.Fatalf("missing extra/missing region lists: %v", err)
	}
}

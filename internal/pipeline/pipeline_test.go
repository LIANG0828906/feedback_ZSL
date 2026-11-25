package pipeline

import (
	"testing"

	"iconseg/internal/core"
	"iconseg/internal/sample"
)

func TestGeneratedIconsExactAndPreview(t *testing.T) {
	icons := sample.Generate(20261125)
	params := core.DefaultParams()
	for _, icon := range icons {
		icon := icon
		t.Run(icon.Manifest.ImageID, func(t *testing.T) {
			snap, err := core.NewSnapshot(icon.Manifest.Seed, icon.Manifest.ImageID, params)
			if err != nil {
				t.Fatal(err)
			}
			exact, err := Exact(icon, snap)
			if err != nil {
				t.Fatalf("exact: %+v", err)
			}
			if !exact.Match.ReferenceMatch {
				t.Fatalf("mask does not match truth: %+v", exact.Match)
			}
			if exact.Match.MaxHoleDelta > icon.Manifest.HoleCountTolerance {
				t.Fatalf("hole delta=%d tolerance=%d", exact.Match.MaxHoleDelta, icon.Manifest.HoleCountTolerance)
			}
			t.Logf("exact: components=%d mismatch=%.4f centroid=%.3f holes=%d merge=%d flood=%d elapsed=%s",
				len(exact.Components), exact.Match.MaskMismatchRatio, exact.Match.MaxCentroidDelta,
				exact.Match.MaxHoleDelta, exact.MergeCount, exact.FloodSamples, exact.Elapsed)
			preview, err := Preview(icon, snap)
			if err != nil {
				t.Fatalf("preview: %+v", err)
			}
			t.Logf("preview: factor=%d components=%d elapsed=%s within=%v",
				preview.Downsample, len(preview.Components), preview.Elapsed, preview.WithinLatency)
			report, err := CheckConsistency(preview, exact, icon.Manifest)
			t.Logf("consistency: %+v err=%v", report.Measured, err)
			if err != nil {
				t.Fatalf("consistency: %+v", err)
			}
		})
	}
}

func TestPreviewVersionMixingReturnsVersionError(t *testing.T) {
	icon := sample.Generate(20261125)[0]
	params := core.DefaultParams()
	snap1, err := core.NewSnapshot(icon.Manifest.Seed, icon.Manifest.ImageID, params)
	if err != nil {
		t.Fatal(err)
	}
	params.FloodThreshold += 11
	snap2, err := core.NewSnapshot(icon.Manifest.Seed, icon.Manifest.ImageID, params)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := Preview(icon, snap2)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := Exact(icon, snap1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckConsistency(preview, exact, icon.Manifest); err == nil {
		t.Fatal("mixed snapshot versions were accepted")
	} else if core.AsAppError(err).Code != core.CodeVersion {
		t.Fatalf("code=%s want E_VERSION", core.AsAppError(err).Code)
	}
}

func TestPreviewDivergenceReportsMeasurements(t *testing.T) {
	icon := sample.Generate(20261125)[0]
	params := core.DefaultParams()
	snap, err := core.NewSnapshot(icon.Manifest.Seed, icon.Manifest.ImageID, params)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := Exact(icon, snap)
	if err != nil {
		t.Fatal(err)
	}
	preview := &PreviewResult{
		Snapshot: snap, Mask: make([]byte, len(exact.Mask)), Downsample: 2,
	}
	_, err = CheckConsistency(preview, exact, icon.Manifest)
	if err == nil {
		t.Fatal("divergent preview was accepted")
	}
	app := core.AsAppError(err)
	if app.Code != core.CodeDiverge {
		t.Fatalf("code=%s want E_DIVERGE", app.Code)
	}
	if app.Divergence == nil || app.Divergence.DifferentPixels == 0 ||
		app.Divergence.PreviewRegionCount != 0 || app.Divergence.ExactRegionCount == 0 {
		t.Fatalf("divergence measurements incomplete: %+v", app.Divergence)
	}
}

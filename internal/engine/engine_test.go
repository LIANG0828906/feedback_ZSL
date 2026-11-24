package engine

import (
	"context"
	"path/filepath"
	"testing"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/frameio"
	"gameintegrity/internal/model"
	"gameintegrity/internal/sample"
	"gameintegrity/internal/scale"
)

func setupTiny(t *testing.T) (*frameio.Dataset, model.Config) {
	t.Helper()
	root := t.TempDir()
	spec := sample.Spec{
		ID: "tiny", Width: 80, Height: 64, Channels: 3, FrameCount: 16,
		BlockSize: 8, Replacement: model.Interval{Start: 6, End: 8},
		SegmentSeconds: 1, Tolerance: 1,
	}
	if err := sample.WriteSample(root, spec, 20261124); err != nil {
		t.Fatal(err)
	}
	ds, err := frameio.Open(root, "tiny")
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.DefaultConfig(ds.Manifest.Width, ds.Manifest.Height, ds.Manifest.Channels, ds.Manifest.FrameCount, ds.Manifest.BlockSize, ds.Manifest.SamplingBudget)
	cfg.SegmentSeconds = ds.Manifest.SegmentSeconds
	return ds, cfg
}

func TestThreePathsExactAndTruthAligned(t *testing.T) {
	ds, cfg := setupTiny(t)
	ctx := context.Background()
	full, err := Verify(ctx, ds, cfg, Options{Path: "full", Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if !full.Replacement.WithinTolerance {
		t.Fatalf("full replacement = %+v", full.Replacement)
	}
	realtimeWork := filepath.Join(t.TempDir(), "realtime")
	realtime, err := Verify(ctx, ds, cfg, Options{Path: "realtime", Mode: "auto", WorkDir: realtimeWork})
	if err != nil {
		t.Fatal(err)
	}
	resumeWork := filepath.Join(t.TempDir(), "resume")
	partial, err := Verify(ctx, ds, cfg, Options{Path: "realtime", Mode: "auto", WorkDir: resumeWork, StopAfterCommitted: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !partial.Paused || partial.CommittedSegments != 1 {
		t.Fatalf("partial report = paused:%v committed:%d", partial.Paused, partial.CommittedSegments)
	}
	resumed, err := Verify(ctx, ds, cfg, Options{Path: "resume", Mode: "auto", WorkDir: resumeWork})
	if err != nil {
		t.Fatal(err)
	}
	if err := Compare(full, realtime); err != nil {
		t.Fatalf("full vs realtime: %v", err)
	}
	if err := Compare(full, resumed); err != nil {
		t.Fatalf("full vs resume: %v", err)
	}
	if err := Compare(realtime, resumed); err != nil {
		t.Fatalf("realtime vs resume: %v", err)
	}
}

func TestFineAndCandidateIntervalsAgree(t *testing.T) {
	ds, cfg := setupTiny(t)
	fine, err := Verify(context.Background(), ds, cfg, Options{Path: "full", Mode: "fine"})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := Verify(context.Background(), ds, cfg, Options{Path: "full", Mode: "candidate"})
	if err != nil {
		t.Fatal(err)
	}
	if err := scale.CheckIntervalAgreement(fine.Replacement.Detected, candidate.Replacement.Detected, ds.Truth.Tolerance); err != nil {
		t.Fatal(err)
	}
	if len(candidate.Scale.RefinedFrames) >= len(fine.Scale.RefinedFrames) {
		t.Fatalf("candidate mode did not reduce refined frames: candidate=%d fine=%d",
			len(candidate.Scale.RefinedFrames), len(fine.Scale.RefinedFrames))
	}
}

func TestCompareReturnsFirstDiffWithValues(t *testing.T) {
	ds, cfg := setupTiny(t)
	report, err := Verify(context.Background(), ds, cfg, Options{Path: "full", Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	other := *report
	other.Frames = append([]model.FrameResult(nil), report.Frames...)
	other.Frames[3].Summary = "tampered"
	err = Compare(report, &other)
	e := apperr.As(err)
	if e == nil || e.Code != apperr.Diff || e.Frame != 3 || e.Field != "summary" {
		t.Fatalf("error = %#v", e)
	}
	if e.Left == e.Right {
		t.Fatalf("left/right values were not both reported: %#v", e)
	}
}

func TestCheckpointParameterChangeInvalidatesEverything(t *testing.T) {
	ds, cfg := setupTiny(t)
	work := filepath.Join(t.TempDir(), "resume")
	if _, err := Verify(context.Background(), ds, cfg, Options{Path: "realtime", Mode: "auto", WorkDir: work, StopAfterCommitted: 1}); err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.ResidualThreshold += 0.001
	report, err := Verify(context.Background(), ds, changed, Options{Path: "resume", Mode: "auto", WorkDir: work})
	if err != nil {
		t.Fatal(err)
	}
	if report.Invalidation == nil || len(report.Invalidation.Fields) == 0 {
		t.Fatal("parameter mismatch did not invalidate the snapshot")
	}
	found := false
	for _, field := range report.Invalidation.Fields {
		if field == "param_version" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fields = %v", report.Invalidation.Fields)
	}
	if report.CommittedSegments != report.TotalSegments {
		t.Fatalf("recomputed report incomplete: %d/%d", report.CommittedSegments, report.TotalSegments)
	}
}

func TestTighterThresholdsDoNotIncreaseAbnormalBlocks(t *testing.T) {
	ds, cfg := setupTiny(t)
	loose := cfg
	loose.ResidualThreshold = 0.01
	loose.NoiseThreshold = 0.01
	tight := cfg
	tight.ResidualThreshold = 0.05
	tight.NoiseThreshold = 0.05
	looseReport, err := Verify(context.Background(), ds, loose, Options{Path: "full", Mode: "fine"})
	if err != nil {
		t.Fatal(err)
	}
	tightReport, err := Verify(context.Background(), ds, tight, Options{Path: "full", Mode: "fine"})
	if err != nil {
		t.Fatal(err)
	}
	count := func(r *model.RunReport) int {
		n := 0
		for i := range r.Frames {
			n += len(r.Frames[i].AbnormalBlocks)
		}
		return n
	}
	if got, base := count(tightReport), count(looseReport); got > base {
		t.Fatalf("tight abnormal blocks = %d, loose = %d", got, base)
	}
}

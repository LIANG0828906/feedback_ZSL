package audiofp_test

import (
	"context"
	"path/filepath"
	"testing"

	"audiofp"
	"audiofp/samplegen"
)

func TestThreePathsProduceIdenticalResults(t *testing.T) {
	ctx := context.Background()
	m := samplegen.GenerateDefault()
	params := audiofp.DefaultParams()
	limits := audiofp.ResourceLimits{MaxWorkers: 4, PerWorkerBufferSamples: 32768, MaxQueuedJobs: 8}

	full, err := audiofp.AnnotateFull(ctx, m, params, limits)
	if err != nil {
		t.Fatal(err)
	}
	realtime, err := audiofp.AnnotateRealtime(ctx, m, params, limits)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "three-path.db")
	resume, err := audiofp.AnnotateResume(ctx, m, params, limits, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := audiofp.EnsureIdentical(full, realtime, resume)
	if err != nil {
		t.Fatalf("three paths differ: %v", err)
	}
	if !report.Consistent || report.CheckedFrames != len(full.Frames) {
		t.Fatalf("bad consistency report: %#v", report)
	}
}

func TestEnsureIdenticalLocatesFirstFrameField(t *testing.T) {
	full := annotatedSample(t)
	modified := *full
	modified.Frames = append([]audiofp.Fingerprint(nil), full.Frames...)
	frameIndex := 3
	modified.Frames[frameIndex].Bands = append([]int(nil), full.Frames[frameIndex].Bands...)
	if modified.Frames[frameIndex].Bands[0] == 255 {
		modified.Frames[frameIndex].Bands[0] = 254
	} else {
		modified.Frames[frameIndex].Bands[0]++
	}

	_, err := audiofp.EnsureIdentical(full, &modified)
	if !audiofp.HasCode(err, audiofp.CodeDiff) {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	de, _ := audiofp.AsError(err)
	if de.FrameIndex != frameIndex || de.Field != "frames[3].bands[0]" {
		t.Fatalf("unexpected diff location: frame=%d field=%s", de.FrameIndex, de.Field)
	}
}

func TestEnsureIdenticalLocatesEventField(t *testing.T) {
	full := annotatedSample(t)
	modified := *full
	modified.Intervals = append([]audiofp.Interval(nil), full.Intervals...)
	modified.Intervals[1].Category = "wrong-category"
	_, err := audiofp.EnsureIdentical(full, &modified)
	if !audiofp.HasCode(err, audiofp.CodeDiff) {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	de, _ := audiofp.AsError(err)
	if de.Field != "events[1].category" {
		t.Fatalf("expected events[1].category, got %s", de.Field)
	}
}

func TestRealtimeQualityErrorStillComparable(t *testing.T) {
	// Tight tolerance is a domain quality failure, not a computation divergence.
	m := samplegen.GenerateDefault()
	params := audiofp.DefaultParams()
	m.Manifest.Quality.ToleranceSamples = 0
	result, err := audiofp.AnnotateRealtime(context.Background(), m, params, audiofp.DefaultResourceLimits())
	if !audiofp.HasCode(err, audiofp.CodeQual) {
		t.Fatalf("expected E_QUAL, got %v", err)
	}
	if result == nil || result.Quality == nil {
		t.Fatalf("quality report should be returned with E_QUAL")
	}
}

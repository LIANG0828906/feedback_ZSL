package verify

import (
	"context"
	"math"
	"testing"

	"spatialworkbench/internal/generator"
	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

func TestThreePathsExactAndResume(t *testing.T) {
	sample := generator.Generate(generator.Options{Seed: 2026, Samples: 1, Prefix: "verify"}).Samples[0]
	params := model.DefaultParams(sample)
	params.MaxWorkers = 3
	params.BufferSamplesPerWorker = 8192
	resumeAt := params.TotalFrames(len(sample.Dry.Samples)) / 2
	report, err := RunThreePaths(context.Background(), sample, params, t.TempDir(), resumeAt)
	if err != nil {
		t.Fatalf("three-path run: %v", err)
	}
	if !report.Equal || len(report.Compared) != 3 {
		t.Fatalf("report not equal: %#v", report.Compared)
	}
	for _, comparison := range report.Compared {
		if !comparison.Equal || comparison.Error != "" {
			t.Fatalf("comparison failed: %#v", comparison)
		}
	}
	if report.ResumeAt != resumeAt {
		t.Fatalf("resumeAt=%d want %d", report.ResumeAt, resumeAt)
	}
	assertOutputLength(t, report.Realtime.Output, len(sample.Dry.Samples))
	assertOutputLength(t, report.Resume.Output, len(sample.Dry.Samples))
	assertOutputLength(t, report.Recompute.Output, len(sample.Dry.Samples))
	if len(report.Realtime.CommitSequence) != params.TotalFrames(len(sample.Dry.Samples)) {
		t.Fatalf("commit sequence length %d", len(report.Realtime.CommitSequence))
	}
}

func assertOutputLength(t *testing.T, output [][]float64, want int) {
	t.Helper()
	if len(output) == 0 {
		t.Fatal("empty output")
	}
	for ch := range output {
		if len(output[ch]) != want {
			t.Fatalf("channel %d length %d, want %d", ch, len(output[ch]), want)
		}
	}
}

func TestCompareLocatesFirstMismatch(t *testing.T) {
	sample := generator.Generate(generator.Options{Seed: 8, Samples: 1}).Samples[0]
	params := model.DefaultParams(sample)
	run, err := RenderAll(sample, params)
	if err != nil {
		t.Fatal(err)
	}
	changed := run
	changed.Output = make([][]float64, len(run.Output))
	for ch := range run.Output {
		changed.Output[ch] = append([]float64(nil), run.Output[ch]...)
	}
	changed.Output[1][3] = changed.Output[1][3] + 1
	diff := Compare("left", "right", run, changed)
	if diff == nil || diff.Code != werr.CodeDiff {
		t.Fatalf("expected E_DIFF, got %#v", diff)
	}
	if diff.Channel != "ch1" || diff.Field != "output.sample[3]" || diff.Left == nil || diff.Right == nil {
		t.Fatalf("bad diff context: %#v", diff)
	}
	if math.Float64bits(diff.Left.(float64)) == math.Float64bits(diff.Right.(float64)) {
		t.Fatal("diff values are equal")
	}
}

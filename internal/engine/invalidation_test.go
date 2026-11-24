package engine_test

import (
	"fmt"
	"testing"

	"integritykernel/internal/core"
	"integritykernel/internal/engine"
	"integritykernel/internal/loader"
	"integritykernel/internal/pipeline"
	"integritykernel/internal/samplegen"
)

func TestPreciseInvalidationAndFullEquality(t *testing.T) {
	dir := t.TempDir()
	if _, err := samplegen.Generate(samplegen.Options{OutputDir: dir, Frames: 24, Force: true}); err != nil {
		t.Fatal(err)
	}
	seq, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	params := core.DefaultParams()
	params.MaxParallel = 3
	eng, err := engine.New(seq, engine.Options{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(); err != nil {
		t.Fatal(err)
	}
	before := keyMap(eng.BuildReport())

	blockParams := params
	blockParams.BlockSize = 12
	invalidations, err := eng.ApplyParams(blockParams)
	if err != nil {
		t.Fatal(err)
	}
	assertInvalidationCounts(t, invalidations, map[string]int{
		core.EvidenceNoise:   seq.Manifest.FrameCount,
		core.EvidenceVerdict: seq.Manifest.FrameCount,
	})
	if err := eng.Run(); err != nil {
		t.Fatal(err)
	}
	afterBlock := keyMap(eng.BuildReport())
	for frame := 0; frame < seq.Manifest.FrameCount; frame++ {
		for _, evidenceType := range []string{core.EvidenceWatermark, core.EvidenceResidual} {
			id := engine.NodeID(frame, evidenceType)
			if before[id] != afterBlock[id] {
				t.Fatalf("%s should be reused after block_size change", id)
			}
		}
		for _, evidenceType := range []string{core.EvidenceNoise, core.EvidenceVerdict} {
			id := engine.NodeID(frame, evidenceType)
			if before[id] == afterBlock[id] {
				t.Fatalf("%s should change after block_size change", id)
			}
		}
	}

	thresholdParams := blockParams
	thresholdParams.VerdictThreshold = 0.65
	thresholdInvalidations, err := eng.ApplyParams(thresholdParams)
	if err != nil {
		t.Fatal(err)
	}
	assertInvalidationCounts(t, thresholdInvalidations, map[string]int{core.EvidenceVerdict: seq.Manifest.FrameCount})
	if err := eng.Run(); err != nil {
		t.Fatal(err)
	}

	full, err := engine.New(seq, engine.Options{Params: thresholdParams})
	if err != nil {
		t.Fatal(err)
	}
	if err := full.Run(); err != nil {
		t.Fatal(err)
	}
	if diff := pipeline.CompareReports(full.BuildReport(), eng.BuildReport()); diff != nil {
		t.Fatalf("incremental/full mismatch: %#v", diff)
	}
}

func TestRegionCorrectionInvalidatesWatermarkAndVerdict(t *testing.T) {
	dir := t.TempDir()
	seq := generateSequence(t, dir)
	wrong := seq.Manifest.WatermarkRegion
	wrong.X -= 3
	eng, err := engine.New(seq, engine.Options{Region: &wrong})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(); err != nil {
		t.Fatal(err)
	}
	invalidations, err := eng.CorrectRegion(seq.Manifest.WatermarkRegion)
	if err != nil {
		t.Fatal(err)
	}
	assertInvalidationCounts(t, invalidations, map[string]int{
		core.EvidenceWatermark: seq.Manifest.FrameCount,
		core.EvidenceVerdict:   seq.Manifest.FrameCount,
	})
}

func TestInvalidParameterReturnsEPAra(t *testing.T) {
	dir := t.TempDir()
	seq := generateSequence(t, dir)
	eng, err := engine.New(seq, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	bad := eng.Params()
	bad.BlockSize = 1
	_, err = eng.ApplyParams(bad)
	ke, ok := core.AsError(err)
	if !ok || ke.Code != core.CodePara || ke.Field != "block_size" {
		t.Fatalf("expected E_PARA block_size, got %#v", err)
	}
}

func TestSingleEvidenceFailureIsolatedAndRetryable(t *testing.T) {
	dir := t.TempDir()
	seq := generateSequence(t, dir)
	params := core.DefaultParams()
	params.MaxParallel = 1
	eng, err := engine.New(seq, engine.Options{
		Params: params,
		Fault: func(frame int, evidenceType string) error {
			if frame == 5 && evidenceType == core.EvidenceNoise {
				return fmt.Errorf("injected block statistic failure")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(); err != nil {
		t.Fatal(err)
	}
	failed := failureMap(eng.BuildReport())
	if failed[engine.NodeID(5, core.EvidenceNoise)] == nil {
		t.Fatalf("expected noise frame 5 failure: %#v", failed)
	}
	if failed[engine.NodeID(5, core.EvidenceVerdict)] == nil {
		t.Fatalf("expected downstream verdict frame 5 to be blocked: %#v", failed)
	}
	if failed[engine.NodeID(4, core.EvidenceVerdict)] != nil || failed[engine.NodeID(6, core.EvidenceVerdict)] != nil {
		t.Fatalf("failure polluted neighboring evidence: %#v", failed)
	}

	eng.SetFault(nil)
	if err := eng.Run(); err != nil {
		t.Fatal(err)
	}
	report := eng.BuildReport()
	if len(report.Failures) != 0 {
		t.Fatalf("retry did not clear failures: %#v", report.Failures)
	}
	full, err := engine.New(seq, engine.Options{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if err := full.Run(); err != nil {
		t.Fatal(err)
	}
	if diff := pipeline.CompareReports(full.BuildReport(), report); diff != nil {
		t.Fatalf("retried evidence differs from full recompute: %#v", diff)
	}
}

func generateSequence(t *testing.T, dir string) *loader.Sequence {
	t.Helper()
	if _, err := samplegen.Generate(samplegen.Options{OutputDir: dir, Frames: 24, Force: true}); err != nil {
		t.Fatal(err)
	}
	seq, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func keyMap(report *core.Report) map[string]string {
	out := map[string]string{}
	for _, record := range report.CommitSequence {
		out[engine.NodeID(record.Frame, record.Type)] = record.CacheKey
	}
	return out
}

func assertInvalidationCounts(t *testing.T, records []core.InvalidationRecord, expected map[string]int) {
	t.Helper()
	counts := map[string]int{}
	for _, record := range records {
		counts[record.Type]++
	}
	for evidenceType, expectedCount := range expected {
		if counts[evidenceType] != expectedCount {
			t.Fatalf("expected %d %s invalidations, got %d; counts=%#v records=%#v", expectedCount, evidenceType, counts[evidenceType], counts, records)
		}
	}
	if len(counts) != len(expected) {
		t.Fatalf("unexpected invalidation types: counts=%#v records=%#v", counts, records)
	}
}

func failureMap(report *core.Report) map[string]*core.FailureRecord {
	out := map[string]*core.FailureRecord{}
	for i := range report.Failures {
		record := report.Failures[i]
		out[engine.NodeID(record.Frame, record.Type)] = &record
	}
	return out
}

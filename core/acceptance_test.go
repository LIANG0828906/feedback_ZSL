package dialoguecal

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func fixtureOrFail(t *testing.T) *Fixture {
	t.Helper()
	fx, err := GenerateFixture(42)
	if err != nil {
		t.Fatalf("GenerateFixture: %v", err)
	}
	return fx
}

func TestThreeSchemaVersionsLoadIdenticalProject(t *testing.T) {
	fx := fixtureOrFail(t)
	loads := make([]*LoadResult, 0, 3)
	for _, raw := range [][]byte{fx.V1JSON, fx.V2JSON, fx.V3JSON} {
		loaded, err := LoadProjectJSON(raw)
		if err != nil {
			t.Fatalf("LoadProjectJSON: %v", err)
		}
		loads = append(loads, loaded)
	}
	if !reflect.DeepEqual(loads[0].Project, loads[1].Project) {
		t.Fatal("v1 and v2 canonical projects differ")
	}
	if !reflect.DeepEqual(loads[0].Project, loads[2].Project) {
		t.Fatal("v1 and v3 canonical projects differ")
	}
	frames1, err := BuildFrames(loads[0].Project)
	if err != nil {
		t.Fatal(err)
	}
	frames2, err := BuildFrames(loads[1].Project)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(frames1, frames2) || !reflect.DeepEqual(frames1, fx.Frames) {
		t.Fatal("frame sequences differ across schema versions")
	}
}

func TestFrameFeaturesAndCalibrationMatchTruth(t *testing.T) {
	fx := fixtureOrFail(t)
	engine, err := NewEngine(fx.Project)
	if err != nil {
		t.Fatal(err)
	}
	report, err := engine.ComputeAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != len(fx.Truth.Frames) {
		t.Fatalf("frame count %d != truth %d", len(report.Results), len(fx.Truth.Frames))
	}
	comparisons := CompareResults(report.Results, fx.Truth)
	failures := 0
	for _, cmp := range comparisons {
		if !cmp.AllWithin {
			failures++
			t.Errorf("frame %d outside tolerance: %+v", cmp.GlobalIndex, cmp)
		}
	}
	if failures != 0 {
		t.Fatalf("%d frames outside declared tolerance", failures)
	}
	states := map[CalibrationState]int{}
	for _, result := range report.Results {
		states[result.Calibration.State]++
	}
	if states[StateUncalibrated] != len(fx.Project.Segments) {
		t.Fatalf("expected one uncalibrated first frame per segment, got %v", states)
	}
	if states[StateCalibrated] == 0 || states[StateRecalibrate] == 0 {
		t.Fatalf("expected calibrated and recalibrating states, got %v", states)
	}
}

func TestCriticalSuitesAndOverflow(t *testing.T) {
	suite := GenerateCriticalSuite()
	if suite == nil {
		t.Fatal("nil critical suite")
	}
	for _, testCase := range suite.Cases {
		if !testCase.AllWithin {
			for _, cmp := range testCase.Comparisons {
				if !cmp.AllWithin {
					t.Errorf("%s frame %d mismatch %+v", testCase.ID, cmp.GlobalIndex, cmp)
				}
			}
		}
	}
	for _, check := range suite.OverflowChecks {
		if !check.OK {
			t.Fatalf("overflow check %s failed: %+v", check.Name, check)
		}
	}
}

func TestIncrementalGainAndPhonemeClosuresMatchFull(t *testing.T) {
	fx := fixtureOrFail(t)

	t.Run("gain_recomputes_one_segment_tail", func(t *testing.T) {
		engine, err := NewEngine(fx.Project)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.ComputeAll(); err != nil {
			t.Fatal(err)
		}
		edit := Edit{Kind: EditGain, SegmentID: "seg-1", GainPermille: 950}
		incremental, err := engine.RecomputeWithEdits(edit)
		if err != nil {
			t.Fatal(err)
		}
		if len(incremental.RecomputedSeqs) != 20 || len(incremental.ReusedSeqs) != 60 {
			t.Fatalf("unexpected counts recomputed=%d reused=%d", len(incremental.RecomputedSeqs), len(incremental.ReusedSeqs))
		}
		if len(incremental.Closures) != 1 || incremental.Closures[0].ClosureRanges[0].Start != 0 || incremental.Closures[0].ClosureRanges[0].End != 20 {
			t.Fatalf("bad closure: %+v", incremental.Closures)
		}
		edited, err := ApplyEdits(fx.Project, edit)
		if err != nil {
			t.Fatal(err)
		}
		full, err := NewEngine(edited)
		if err != nil {
			t.Fatal(err)
		}
		fullReport, err := full.ComputeAll()
		if err != nil {
			t.Fatal(err)
		}
		if !ResultsEquivalent(incremental.Results, fullReport.Results) {
			t.Fatal("incremental gain result differs from full recomputation")
		}
		for _, seq := range incremental.ReusedSeqs {
			if incremental.Results[seq].Provenance.CacheState != "reused" {
				t.Fatalf("frame %d was not marked reused", seq)
			}
		}
	})

	t.Run("target_loudness_recomputes_one_segment", func(t *testing.T) {
		engine, _ := NewEngine(fx.Project)
		_, _ = engine.ComputeAll()
		edit := Edit{Kind: EditTarget, SegmentID: "seg-2", TargetLevelPermille: 140}
		incremental, err := engine.RecomputeWithEdits(edit)
		if err != nil {
			t.Fatal(err)
		}
		if len(incremental.RecomputedSeqs) != 20 || len(incremental.ReusedSeqs) != 60 {
			t.Fatalf("target edit counts r=%d u=%d", len(incremental.RecomputedSeqs), len(incremental.ReusedSeqs))
		}
		edited, err := ApplyEdits(fx.Project, edit)
		if err != nil {
			t.Fatal(err)
		}
		fullEngine, _ := NewEngine(edited)
		fullReport, err := fullEngine.ComputeAll()
		if err != nil {
			t.Fatal(err)
		}
		if !ResultsEquivalent(incremental.Results, fullReport.Results) {
			t.Fatal("target incremental result differs from full recomputation")
		}
	})

	t.Run("phoneme_closure_starts_inside_segment", func(t *testing.T) {
		engine, _ := NewEngine(fx.Project)
		_, _ = engine.ComputeAll()
		ph := *phonemeByID(fx.Project, "ph-02")
		ph.WeightPermille = 1120
		edit := Edit{Kind: EditPhoneme, PhonemeID: ph.ID, Phoneme: &ph}
		incremental, err := engine.RecomputeWithEdits(edit)
		if err != nil {
			t.Fatal(err)
		}
		if len(incremental.RecomputedSeqs) != 10 || len(incremental.ReusedSeqs) != 70 {
			t.Fatalf("expected 10 tail frames and 70 reused, got r=%d u=%d", len(incremental.RecomputedSeqs), len(incremental.ReusedSeqs))
		}
		edited, _ := ApplyEdits(fx.Project, edit)
		fullEngine, _ := NewEngine(edited)
		fullReport, err := fullEngine.ComputeAll()
		if err != nil {
			t.Fatal(err)
		}
		if !ResultsEquivalent(incremental.Results, fullReport.Results) {
			t.Fatal("phoneme incremental result differs from full recomputation")
		}
	})

	t.Run("phoneme_crosses_segment_boundary", func(t *testing.T) {
		engine, _ := NewEngine(fx.Project)
		_, _ = engine.ComputeAll()
		ph := *phonemeByID(fx.Project, "ph-03")
		if ph.EndSample <= 6400 || ph.StartSample >= 6400 {
			t.Fatalf("fixture phoneme does not cross boundary: %+v", ph)
		}
		ph.WeightPermille = 970
		incremental, err := engine.RecomputeWithEdits(Edit{Kind: EditPhoneme, PhonemeID: ph.ID, Phoneme: &ph})
		if err != nil {
			t.Fatal(err)
		}
		if len(incremental.Closures[0].ClosureRanges) != 2 {
			t.Fatalf("expected two segment closure ranges, got %+v", incremental.Closures[0].ClosureRanges)
		}
		if len(incremental.RecomputedSeqs) != 25 || len(incremental.ReusedSeqs) != 55 {
			t.Fatalf("cross-boundary counts recomputed=%d reused=%d", len(incremental.RecomputedSeqs), len(incremental.ReusedSeqs))
		}
	})

	t.Run("frame_rate_invalidates_all_coordinates", func(t *testing.T) {
		engine, _ := NewEngine(fx.Project)
		_, _ = engine.ComputeAll()
		incremental, err := engine.RecomputeWithEdits(Edit{Kind: EditFrameRate, FramesPerSecond: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(incremental.ReusedSeqs) != 0 || len(incremental.RecomputedSeqs) != 159 {
			t.Fatalf("frame rate edit should recompute all 159 overlapping frames, got %+v", incremental)
		}
	})
}

func TestParallelResultsAreBudgetAndOrderIndependent(t *testing.T) {
	fx := fixtureOrFail(t)
	engine, _ := NewEngine(fx.Project)
	full, err := engine.ComputeAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []Budget{{Workers: 1, FramesPerWorker: 1, MaxWorkers: 8}, {Workers: 2, FramesPerWorker: 4, MaxWorkers: 8}, {Workers: 4, FramesPerWorker: 8, MaxWorkers: 8}} {
		parallel, err := ParallelComputeAll(context.Background(), fx.Project, budget)
		if err != nil {
			t.Fatalf("budget %+v: %v", budget, err)
		}
		if !ResultsEquivalent(parallel.Results, full.Results) {
			t.Fatalf("parallel mismatch for budget %+v", budget)
		}
		if len(parallel.SegmentTimings) != len(fx.Project.Segments) {
			t.Fatalf("missing timings: %+v", parallel.SegmentTimings)
		}
	}
	_, err = FillGateForTest(Budget{Workers: 2, FramesPerWorker: 2, MaxWorkers: 8})
	if be, ok := AsBusinessError(err); !ok || be.Code != CodeBackpressure {
		t.Fatalf("expected E_BACKPRESSURE, got %v", err)
	}
}

func TestCheckpointRecoveryMatchesFull(t *testing.T) {
	fx := fixtureOrFail(t)
	fullEngine, _ := NewEngine(fx.Project)
	full, err := fullEngine.ComputeAll()
	if err != nil {
		t.Fatal(err)
	}
	version := ContentHash(fx.Project)

	t.Run("file_store_interrupt_and_recover", func(t *testing.T) {
		dir := t.TempDir()
		store, err := NewFileCheckpointStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		interrupted, err := RunCheckpointed(ctx, fx.Project, store, version, func(seq int) {
			if seq == 31 {
				cancel()
			}
		})
		if err != context.Canceled {
			t.Fatalf("expected cancellation, got %v", err)
		}
		if len(interrupted.Results) != 32 {
			t.Fatalf("expected 32 committed-complete results, got %d", len(interrupted.Results))
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewFileCheckpointStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		recovered, didRecover, err := RecoverAndComplete(context.Background(), fx.Project, reopened, version, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !didRecover || len(recovered.ReusedSeqs) != 32 {
			t.Fatalf("expected recovery of 32 frames, recovered=%v reused=%d", didRecover, len(recovered.ReusedSeqs))
		}
		if !ResultsEquivalent(recovered.Results, full.Results) {
			t.Fatal("recovered results differ from full computation")
		}
	})

	t.Run("sqlite_store_interrupt_and_recover", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "checkpoint.db")
		store, err := NewSQLiteCheckpointStore(path)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		_, err = RunCheckpointed(ctx, fx.Project, store, version, func(seq int) {
			if seq == 40 {
				cancel()
			}
		})
		if err != context.Canceled {
			t.Fatalf("expected cancellation, got %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewSQLiteCheckpointStore(path)
		if err != nil {
			t.Fatal(err)
		}
		recovered, didRecover, err := RecoverAndComplete(context.Background(), fx.Project, reopened, version, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !didRecover || len(recovered.ReusedSeqs) != 41 {
			t.Fatalf("expected recovery of 41 frames, recovered=%v reused=%d", didRecover, len(recovered.ReusedSeqs))
		}
		if !ResultsEquivalent(recovered.Results, full.Results) {
			t.Fatal("sqlite recovered results differ")
		}
	})
}

func TestInjectionCodesAndSourceImmutability(t *testing.T) {
	fx := fixtureOrFail(t)
	before := ContentHash(fx.Project)
	results := RunInjectionSuite(fx, t.TempDir())
	for _, result := range results {
		if !result.OK {
			t.Errorf("injection %s expected %s: %s", result.Name, result.ExpectedCode, result.Message)
		}
		if !result.SourceRemainedSame {
			t.Errorf("injection %s modified source project", result.Name)
		}
	}
	after := ContentHash(fx.Project)
	if before != after {
		t.Fatal("fixture changed by injection suite")
	}
	if seg, source, err := ResolveConflictWithWatermark(); err != nil || seg == nil || source == "" {
		t.Fatalf("watermark disambiguation failed: seg=%v source=%q err=%v", seg, source, err)
	}
}

func TestAnnotationGapOverlapAndFrameBoundsCodes(t *testing.T) {
	fx := fixtureOrFail(t)

	gap := CloneProject(fx.Project)
	ph := gap.Phonemes[1]
	ph.StartSample += 100
	gap.Phonemes[1] = ph
	err := NormalizeProject(gap)
	if be, ok := AsBusinessError(err); !ok || be.Code != CodePhonemeCoverageGap {
		t.Fatalf("expected E_PHONEME_GAP, got %v", err)
	}

	overlap := CloneProject(fx.Project)
	ph = overlap.Phonemes[1]
	ph.StartSample -= 100
	overlap.Phonemes[1] = ph
	err = NormalizeProject(overlap)
	if be, ok := AsBusinessError(err); !ok || be.Code != CodePhonemeOverlap {
		t.Fatalf("expected E_PHONEME_OVERLAP, got %v", err)
	}

	_, err = SafeFrameSamples(fx.Project, Frame{StartSample: -1, EndSample: 320, GlobalIndex: 999})
	if be, ok := AsBusinessError(err); !ok || be.Code != CodeFrameBounds {
		t.Fatalf("expected E_FRAME_BOUNDS, got %v", err)
	}
}

func TestSnapshotVersionReplacesCache(t *testing.T) {
	fx := fixtureOrFail(t)
	engine, err := NewEngine(fx.Project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ComputeAll(); err != nil {
		t.Fatal(err)
	}
	report, closure, err := engine.ReplaceSnapshot(fx.Project, "snapshot-version-changed")
	if err != nil {
		t.Fatal(err)
	}
	if closure.Code != CodeSnapshotVersion || len(report.Results) != 80 {
		t.Fatalf("bad snapshot replacement closure/report: %+v", closure)
	}
}

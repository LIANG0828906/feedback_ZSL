package verify_test

import (
	"context"
	"path/filepath"
	"testing"

	"screenverify/internal/dataset"
	"screenverify/internal/experiment"
	"screenverify/internal/review"
	"screenverify/internal/rverr"
	"screenverify/internal/sample"
	"screenverify/internal/store"
	"screenverify/internal/verify"
)

func generateDataset(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_, err := sample.GenerateDataset(sample.Options{OutputDir: dir, Seed: 0x51A7E, Frames: 16})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func openEngine(t *testing.T, clipDir string, p verify.Params, rt verify.RuntimeConfig) (*verify.Engine, *store.Store) {
	t.Helper()
	set, err := dataset.Load(clipDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := verify.NewEngine(set, p, rt, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.EnsureIdentity(context.Background(), "test-plan"); err != nil {
		t.Fatal(err)
	}
	return eng, st
}

func runAllBatches(t *testing.T, eng *verify.Engine) {
	t.Helper()
	for b := 0; b < eng.BatchCount(); b++ {
		if _, err := eng.RunBatch(context.Background(), b); err != nil {
			t.Fatalf("batch %d: %v", b, err)
		}
	}
}

func TestDetectionIntervalsAndConfidence(t *testing.T) {
	root := generateDataset(t)
	ctx := context.Background()
	for _, tc := range []struct {
		clip      string
		wantMatch bool
		wantCount int
	}{
		{"noise", true, 1},
		{"watermark", true, 1},
		{"clean", false, 0},
	} {
		t.Run(tc.clip, func(t *testing.T) {
			dir := filepath.Join(root, tc.clip)
			eng, st := openEngine(t, dir, verify.DefaultParams(), verify.RuntimeConfig{Parallelism: 3, BufferPixelsPerThread: 24576})
			defer st.Close()
			runAllBatches(t, eng)
			rep := eng.BuildReport(nil)
			if len(rep.Intervals) != tc.wantCount {
				t.Fatalf("interval count=%d want %d: %+v", len(rep.Intervals), tc.wantCount, rep.Intervals)
			}
			set, err := dataset.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range rep.Decisions {
				if d.Confidence < 0 || d.Confidence > 1 {
					t.Fatalf("frame %d confidence out of range: %v", d.Frame, d.Confidence)
				}
			}
			if tc.wantMatch {
				if !verify.IntervalMatchesTruth(rep.Intervals[0], set.Truth) {
					t.Fatalf("interval %+v does not match truth %+v tolerance %d", rep.Intervals[0], set.Truth.Replacement, set.Truth.FrameTolerance)
				}
			}
			if tc.clip == "clean" {
				for _, d := range rep.Decisions {
					if d.Tampered {
						t.Fatalf("clean clip has false positive frame %d", d.Frame)
					}
				}
			}
			_ = ctx
		})
	}
}

func TestThreeWayConsistencyAndResume(t *testing.T) {
	root := generateDataset(t)
	for _, clip := range []string{"noise", "watermark"} {
		t.Run(clip, func(t *testing.T) {
			dbDir := t.TempDir()
			rt := verify.RuntimeConfig{Parallelism: 4, BufferPixelsPerThread: 24576}
			tw, err := experiment.RunThreeWay(context.Background(), filepath.Join(root, clip), dbDir, verify.DefaultParams(), rt, 2)
			if err != nil {
				t.Fatalf("three-way: %v diff=%+v commit=%+v", err, tw.Diff, tw.CommitDiff)
			}
			if !tw.Consistent {
				t.Fatalf("not consistent: diff=%+v commit=%+v", tw.Diff, tw.CommitDiff)
			}
			rec := tw.Resume.Report.Recovery
			if rec == nil || !rec.Recovered || rec.CompletedCheckpoints != 2 {
				t.Fatalf("unexpected recovery: %+v", rec)
			}
			if len(tw.Realtime.Report.CommitSequence) != len(tw.Resume.Report.CommitSequence) {
				t.Fatalf("commit sequence length mismatch")
			}
		})
	}
}

func TestParameterInjectionPreciseInvalidation(t *testing.T) {
	root := generateDataset(t)
	ctx := context.Background()
	rt := verify.RuntimeConfig{Parallelism: 4, BufferPixelsPerThread: 24576}
	for _, c := range experiment.DefaultInjectionCases(verify.DefaultParams()) {
		t.Run(c.Name, func(t *testing.T) {
			res, err := experiment.RunInjection(ctx, filepath.Join(root, "noise"), t.TempDir(), rt, c)
			if err != nil {
				t.Fatalf("injection: %v diff=%+v", err, res.Diff)
			}
			if !res.Consistent {
				t.Fatalf("diff: %+v", res.Diff)
			}
			if res.Change.RecomputeCount <= 0 || res.Change.HitCount <= 0 {
				t.Fatalf("bad stats: %+v", res.Change)
			}
		})
	}

	block := experiment.InjectionCase{Name: "block", Field: "block_size", Initial: verify.DefaultParams(), Next: func() verify.Params {
		p := verify.DefaultParams()
		p.BlockSize = 8
		return p
	}()}
	res, err := experiment.RunInjection(ctx, filepath.Join(root, "noise"), t.TempDir(), rt, block)
	if err != nil {
		t.Fatal(err)
	}
	if res.Change.RecomputeCount != 32 || res.Change.HitCount != 64 {
		t.Fatalf("block invalidation counts=%d/%d want 32/64", res.Change.RecomputeCount, res.Change.HitCount)
	}
	counts := map[verify.EvidenceType]int{}
	for _, in := range res.Change.Invalidations {
		counts[in.EvidenceType]++
	}
	if counts[verify.EvNoise] != 16 || counts[verify.EvDecision] != 16 {
		t.Fatalf("unexpected invalidation types: %+v", counts)
	}
}

func TestFailureRetryMatchesCleanPathAndReviewer(t *testing.T) {
	root := generateDataset(t)
	clipDir := filepath.Join(root, "noise")
	ctx := context.Background()
	p := verify.DefaultParams()
	rt := verify.RuntimeConfig{Parallelism: 4, BufferPixelsPerThread: 24576, FailureArmed: true, FailureFrame: 8, FailureType: verify.EvNoise}
	st, err := store.Open(filepath.Join(t.TempDir(), "fault.db"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := dataset.Load(clipDir)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := verify.NewEngine(set, p, rt, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.EnsureIdentity(ctx, "fault-plan"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RunBatch(ctx, 0); err != nil {
		t.Fatal(err)
	}
	_, err = eng.RunBatch(ctx, 1)
	if rverr.CodeOf(err) != rverr.EFault {
		t.Fatalf("expected E_FAULT, got %v", err)
	}
	if err := eng.RetryFailedBatch(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for b := 2; b < eng.BatchCount(); b++ {
		if _, err := eng.RunBatch(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	faultReport := eng.BuildReport(nil)
	if faultReport.Stats.FailureCount != 1 || faultReport.Stats.RetryCount != 1 {
		t.Fatalf("failure stats wrong: %+v", faultReport.Stats)
	}
	dbPath := st.Path()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	cleanEng, cleanStore := openEngine(t, clipDir, p, verify.RuntimeConfig{Parallelism: 1, BufferPixelsPerThread: 24576})
	defer cleanStore.Close()
	runAllBatches(t, cleanEng)
	cleanReport := cleanEng.BuildReport(nil)
	if d := verify.CompareSnapshots(faultReport.Snapshot(), cleanReport.Snapshot()); d != nil {
		t.Fatalf("fault/retry differs: %+v", d)
	}
	if d := verify.CompareCommitSequences(faultReport.CommitSequence, cleanReport.CommitSequence); d != nil {
		t.Fatalf("commit sequence differs: %+v", d)
	}

	reviewResult, err := review.Run(dbPath, filepath.Join(clipDir, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reviewResult.OK || reviewResult.Violation != nil {
		t.Fatalf("review failed: %+v", reviewResult.Violation)
	}
}

func TestSnapshotVersionMismatchDiscardsAll(t *testing.T) {
	clipDir := filepath.Join(generateDataset(t), "noise")
	ctx := context.Background()
	eng, st := openEngine(t, clipDir, verify.DefaultParams(), verify.RuntimeConfig{Parallelism: 2, BufferPixelsPerThread: 24576})
	runAllBatches(t, eng)
	if err := st.SetRunMeta(ctx, "algo_version", "old-algorithm"); err != nil {
		t.Fatal(err)
	}
	info, err := eng.Recover(ctx, "test-plan")
	if err != nil {
		t.Fatal(err)
	}
	if !info.SnapshotDiscarded {
		t.Fatalf("expected discard, got %+v", info)
	}
	cps, err := st.Checkpoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 0 {
		t.Fatalf("expected all checkpoints discarded, got %d", len(cps))
	}
}

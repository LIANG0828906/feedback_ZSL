package audiofp_test

import (
	"context"
	"path/filepath"
	"testing"

	"audiofp"
	"audiofp/samplegen"
)

func TestSQLiteCheckpointResumeAndIdentityReset(t *testing.T) {
	ctx := context.Background()
	m := samplegen.GenerateDefault()
	params := audiofp.DefaultParams()
	full, err := audiofp.AnnotateFull(ctx, m, params, audiofp.ResourceLimits{MaxWorkers: 2})
	if err != nil {
		t.Fatal(err)
	}
	identity := audiofp.CheckpointIdentity{MaterialHash: m.ContentHash(), ParamVersion: params.Version(), AlgorithmVersion: audiofp.AlgorithmVersion}
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")

	store, snapshot, err := audiofp.OpenAndResumeCheckpoint(ctx, dbPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != 0 || snapshot.Reset {
		t.Fatalf("new checkpoint should be empty: %#v", snapshot)
	}
	for i := 0; i < 3; i++ {
		fp := full.Frames[i]
		err := store.SaveWindow(ctx, audiofp.CheckpointRecord{
			WindowIndex: i, StartSample: fp.StartSample, Fingerprint: fp,
			FingerprintDigest: fp.Digest(), ParamVersion: params.Version(),
			MaterialHash: m.ContentHash(), AlgorithmVersion: audiofp.AlgorithmVersion,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveWindow(ctx, audiofp.CheckpointRecord{WindowIndex: 99}); !audiofp.HasCode(err, audiofp.CodeState) {
		t.Fatalf("out-of-order checkpoint write must be E_STATE, got %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, snapshot2, err := audiofp.OpenAndResumeCheckpoint(ctx, dbPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	if snapshot2.Reset || len(snapshot2.Records) != 3 {
		t.Fatalf("expected three compatible records, got reset=%v count=%d", snapshot2.Reset, len(snapshot2.Records))
	}
	if snapshot2.Records[2].Fingerprint.Digest() != full.Frames[2].Digest() {
		t.Fatalf("restored fingerprint digest mismatch")
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}

	wrong := identity
	wrong.ParamVersion = "param-stale"
	store3, snapshot3, err := audiofp.OpenAndResumeCheckpoint(ctx, dbPath, wrong)
	if err != nil {
		t.Fatal(err)
	}
	defer store3.Close()
	if !snapshot3.Reset || len(snapshot3.Records) != 0 {
		t.Fatalf("parameter mismatch must reset whole snapshot: %#v", snapshot3)
	}
	if snapshot3.ResetReason == "" {
		t.Fatalf("reset reason should identify param_version")
	}
}

func TestResumeAfterInterruptMatchesFull(t *testing.T) {
	ctx := context.Background()
	m := samplegen.GenerateDefault()
	params := audiofp.DefaultParams()
	limits := audiofp.ResourceLimits{MaxWorkers: 2}
	full, err := audiofp.AnnotateFull(ctx, m, params, limits)
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "resume.db")
	identity := audiofp.CheckpointIdentity{MaterialHash: m.ContentHash(), ParamVersion: params.Version(), AlgorithmVersion: audiofp.AlgorithmVersion}
	store, _, err := audiofp.OpenAndResumeCheckpoint(ctx, dbPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	_, err = audiofp.CheckpointRealtime(ctx, m, params, limits, store, func(index int) error {
		if index == 57 {
			return audiofp.ErrStopAfterWindow
		}
		return nil
	})
	if err != audiofp.ErrStopAfterWindow {
		t.Fatalf("expected stop sentinel, got %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := audiofp.AnnotateResume(ctx, m, params, limits, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.RecoveredWindows != 58 {
		t.Fatalf("expected 58 durable windows, got %d", resumed.RecoveredWindows)
	}
	if _, err := audiofp.EnsureIdentical(full, resumed); err != nil {
		t.Fatalf("resume result differs: %v", err)
	}
}

func TestMismatchedCheckpointForcesFullRecompute(t *testing.T) {
	ctx := context.Background()
	m := samplegen.GenerateDefault()
	params := audiofp.DefaultParams()
	dbPath := filepath.Join(t.TempDir(), "stale.db")

	staleParams := params
	staleParams.BandCount = 16
	identity := audiofp.CheckpointIdentity{MaterialHash: m.ContentHash(), ParamVersion: staleParams.Version(), AlgorithmVersion: audiofp.AlgorithmVersion}
	store, _, err := audiofp.OpenAndResumeCheckpoint(ctx, dbPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := audiofp.ExtractFrameAt(m.PCM.Samples, 0, m.PCM.SampleRate, staleParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWindow(ctx, audiofp.CheckpointRecord{
		WindowIndex: 0, StartSample: 0, Fingerprint: fp, FingerprintDigest: fp.Digest(),
		ParamVersion: staleParams.Version(), MaterialHash: m.ContentHash(), AlgorithmVersion: audiofp.AlgorithmVersion,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := audiofp.AnnotateResume(ctx, m, params, audiofp.DefaultResourceLimits(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.SnapshotReset {
		t.Fatalf("expected stale snapshot reset")
	}
	if result.RecoveredWindows != 0 || len(result.Frames) == 0 {
		t.Fatalf("expected full recomputation, recovered=%d frames=%d", result.RecoveredWindows, len(result.Frames))
	}
}

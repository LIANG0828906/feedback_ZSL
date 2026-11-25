package fogkernel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func compareCheckpointToSolve(t *testing.T, got *SolveReport, scene *Scene) {
	t.Helper()
	want, err := SolveScene(scene, SolveOptions{Path: PathExact, Workers: 1, Buffer: 32})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rays) != len(want.Rays) {
		t.Fatalf("ray count %d != %d", len(got.Rays), len(want.Rays))
	}
	for ri := range want.Rays {
		a, b := got.Rays[ri], want.Rays[ri]
		if a.RayID == "" {
			t.Fatalf("ray %d missing after resume", ri)
		}
		if !near(a.Transmittance, b.Transmittance, 1e-12) || a.TerminalBlock != b.TerminalBlock || len(a.Blocks) != len(b.Blocks) {
			t.Fatalf("ray %d mismatch checkpoint T=%.12f term=%d blocks=%d vs solve T=%.12f term=%d blocks=%d",
				ri, a.Transmittance, a.TerminalBlock, len(a.Blocks), b.Transmittance, b.TerminalBlock, len(b.Blocks))
		}
		for bi := range b.Blocks {
			if bi >= len(a.Blocks) {
				t.Fatalf("ray %d missing block %d", ri, bi)
			}
			if !near(a.Blocks[bi].TOut, b.Blocks[bi].TOut, 1e-12) || !near(a.Blocks[bi].Energy, b.Blocks[bi].Energy, 1e-12) {
				t.Fatalf("ray %d block %d mismatch", ri, bi)
			}
		}
	}
}

func TestCheckpointCrashResume(t *testing.T) {
	bundle, err := GenerateDefaultBundle()
	if err != nil {
		t.Fatal(err)
	}
	scene := bundle.Canonical
	db := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := CreateFreshCheckpoint(db, scene.Version())
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunCheckpointed(context.Background(), scene, store, nil, CheckpointRunOptions{CrashAfterBlockCommits: 3})
	if !errors.Is(err, ErrCrashInjected) {
		t.Fatalf("expected crash injection, got %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	inspected, err := InspectCheckpoint(db, scene.Version())
	if err != nil {
		t.Fatal(err)
	}
	if inspected.CommittedRayCount != 0 {
		t.Fatalf("global ray sequence advanced before ray commit: %d", inspected.CommittedRayCount)
	}

	store2, recovery, err := OpenCheckpointStore(db, scene.Version())
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.HasPartialRay || len(recovery.PartialBlocks) != 3 {
		t.Fatalf("expected three durable partial blocks, got %+v", recovery)
	}
	resumed, err := RunCheckpointed(context.Background(), scene, store2, recovery, CheckpointRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	compareCheckpointToSolve(t, resumed.Report, scene)
}

func TestCheckpointSignalCommitPoint(t *testing.T) {
	bundle, _ := GenerateDefaultBundle()
	scene := bundle.Canonical
	db := filepath.Join(t.TempDir(), "signal.db")
	store, err := CreateFreshCheckpoint(db, scene.Version())
	if err != nil {
		t.Fatal(err)
	}
	out, err := RunCheckpointed(context.Background(), scene, store, nil, CheckpointRunOptions{StopAfterCompleteRays: 2})
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != StopSignal || out.CompleteRayCommits != 2 {
		t.Fatalf("unexpected stop: %+v", out)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2, recovery, err := OpenCheckpointStore(db, scene.Version())
	if err != nil {
		t.Fatal(err)
	}
	if recovery.CommittedRayCount != 2 {
		t.Fatalf("expected two committed rays, got %d", recovery.CommittedRayCount)
	}
	out2, err := RunCheckpointed(context.Background(), scene, store2, recovery, CheckpointRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	compareCheckpointToSolve(t, out2.Report, scene)
}

func TestCheckpointVersionMismatchAndTruncation(t *testing.T) {
	bundle, _ := GenerateDefaultBundle()
	scene := bundle.Canonical
	versionDB := filepath.Join(t.TempDir(), "version.db")
	old, err := CreateFreshCheckpoint(versionDB, "scene-old-version")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := InspectCheckpoint(versionDB, scene.Version())
	biz, ok := AsBizError(err)
	if !ok || biz.Code != CodeCheckpointVersion {
		t.Fatalf("expected E_CHECKPOINT_VERSION, got state=%+v err=%v", state, err)
	}

	truncDB := filepath.Join(t.TempDir(), "trunc.db")
	store, err := CreateFreshCheckpoint(truncDB, scene.Version())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginRay(0, "r-0-0"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(truncDB, 48); err != nil {
		t.Fatal(err)
	}
	_, err = InspectCheckpoint(truncDB, scene.Version())
	biz, ok = AsBizError(err)
	if !ok || biz.Code != CodeCheckpointTrunc {
		t.Fatalf("expected E_CHECKPOINT_TRUNCATED, got %v", err)
	}
}

package runner

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"iconseg/internal/checkpoint"
	"iconseg/internal/core"
	"iconseg/internal/sample"
	"iconseg/internal/verifier"
)

func TestCheckpointAndVerifier(t *testing.T) {
	icons := sample.Generate(20261125)
	icon := icons[1]
	params := core.DefaultParams()
	snap, err := core.NewSnapshot(icon.Manifest.Seed, icon.Manifest.ImageID, params)
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "state", "icons.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, err := Run(context.Background(), store, icon, snap, Options{})
	if err != nil {
		t.Fatalf("run: %+v", err)
	}
	t.Logf("snapshot=%s commits=%d status=%s seq=%v", record.SnapshotID, record.NewCommits,
		record.SnapshotStatus, record.CommitSequence)
	if record.SnapshotStatus != "complete" {
		t.Fatalf("snapshot status = %s", record.SnapshotStatus)
	}
	report, err := verifier.Verify(store, record.SnapshotID, icon.Manifest.ReferenceFingerprint)
	if err != nil {
		t.Fatalf("verify: %+v", err)
	}
	t.Logf("verified: %+v", report)
	if !report.Valid {
		t.Fatal("verifier did not validate")
	}
}

func TestPauseResumeMatchesOneShot(t *testing.T) {
	icon := sample.Generate(20261125)[2]
	params := core.DefaultParams()
	snap, err := core.NewSnapshot(icon.Manifest.Seed, icon.Manifest.ImageID, params)
	if err != nil {
		t.Fatal(err)
	}
	fullStore, err := checkpoint.Open(filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fullStore.Close()
	full, err := Run(context.Background(), fullStore, icon, snap, Options{})
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "resume", "resume.db")
	firstStore, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Run(context.Background(), firstStore, icon, snap, Options{PauseAfterCommits: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Paused || first.SnapshotStatus != "building" {
		t.Fatalf("expected paused building snapshot, got paused=%v status=%s", first.Paused, first.SnapshotStatus)
	}
	if first.Committed != 7 {
		t.Fatalf("committed=%d, want 7", first.Committed)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}

	secondStore, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	resumed, err := Run(context.Background(), secondStore, icon, snap, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.SkippedCommits != 7 || resumed.SnapshotStatus != "complete" {
		t.Fatalf("resume skipped=%d status=%s", resumed.SkippedCommits, resumed.SnapshotStatus)
	}
	if full.Exact.MaskHash != resumed.Exact.MaskHash || full.Exact.StatsHash != resumed.Exact.StatsHash {
		t.Fatal("full and resumed results differ")
	}
	if !reflect.DeepEqual(full.CommitSequence, resumed.CommitSequence) {
		t.Fatalf("commit sequence differs\nfull=%v\nresumed=%v", full.CommitSequence, resumed.CommitSequence)
	}
	if !reflect.DeepEqual(full.CommitHashes, resumed.CommitHashes) {
		t.Fatal("commit hashes or timestamps differ")
	}
	report, err := verifier.Verify(secondStore, resumed.SnapshotID, icon.Manifest.ReferenceFingerprint)
	if err != nil {
		t.Fatalf("verify resumed: %+v", err)
	}
	if !report.Valid {
		t.Fatal("resumed snapshot did not verify")
	}
}

func TestInvalidatedSnapshotForcesFullRecompute(t *testing.T) {
	icon := sample.Generate(20261125)[0]
	params := core.DefaultParams()
	snap, err := core.NewSnapshot(icon.Manifest.Seed, icon.Manifest.ImageID, params)
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "invalidate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := Run(context.Background(), store, icon, snap, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InvalidateSnapshot(first.SnapshotID, "test injection"); err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), store, icon, snap, Options{})
	if err != nil {
		t.Fatalf("recompute: %+v", err)
	}
	if !second.Invalidated {
		t.Fatal("invalidated snapshot was not reported")
	}
	if second.NewCommits != second.TotalLayers || second.SnapshotStatus != "complete" {
		t.Fatalf("new_commits=%d total=%d status=%s", second.NewCommits, second.TotalLayers, second.SnapshotStatus)
	}
	if second.Exact.MaskHash != first.Exact.MaskHash {
		t.Fatal("full recompute mask changed")
	}
	if _, err := verifier.Verify(store, second.SnapshotID, icon.Manifest.ReferenceFingerprint); err != nil {
		t.Fatal(err)
	}
}

package checkpoint

import (
	"path/filepath"
	"testing"

	"iconseg/internal/core"
	"iconseg/internal/layers"
	"iconseg/internal/sample"
)

func testSnapshot(t *testing.T) (*Store, SnapshotRecord, core.Snapshot, []layers.Block) {
	t.Helper()
	icon := sample.Generate(20261125)[0]
	snap, err := core.NewSnapshot(icon.Manifest.Seed, icon.Manifest.ImageID, core.DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := layers.Split(icon.Manifest.Width, icon.Manifest.Height, icon.Manifest.BlockSize)
	record, err := store.EnsureSnapshot(snap, icon.Manifest.ReferenceFingerprint, blocks)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store, record, snap, blocks
}

func TestIllegalTransitionPreservesState(t *testing.T) {
	store, record, _, _ := testSnapshot(t)
	defer store.Close()
	if err := store.Transition(record.ID, 0, string(layers.Gradient), Committed); err == nil {
		t.Fatal("pending -> committed should fail")
	} else if core.AsAppError(err).Code != core.CodeState {
		t.Fatalf("code=%s, want E_STATE", core.AsAppError(err).Code)
	}
	rows, err := store.LoadRows(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].State != Pending {
		t.Fatalf("state changed to %s after illegal transition", rows[0].State)
	}
	if err := store.Transition(record.ID, 0, string(layers.Gradient), Processing); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitLayer(record.ID, 0, string(layers.Gradient), "ok", []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(record.ID, 0, string(layers.Gradient), Processing); err == nil {
		t.Fatal("committed -> processing should fail")
	}
}

func TestHashMismatchInvalidatesWholeSnapshot(t *testing.T) {
	store, record, snap, _ := testSnapshot(t)
	defer store.Close()
	if err := store.Transition(record.ID, 0, string(layers.Gradient), Processing); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitLayer(record.ID, 0, string(layers.Gradient), "ok", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE blocks SET content=? WHERE snapshot_id=? AND block_index=0 AND layer=?`,
		[]byte("tampered"), record.ID, string(layers.Gradient)); err != nil {
		t.Fatal(err)
	}
	err := store.ValidateSnapshot(record.ID)
	if err == nil || core.AsAppError(err).Code != core.CodeVersion {
		t.Fatalf("ValidateSnapshot error = %v, want E_VERSION", err)
	}
	got, err := store.GetSnapshot(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "invalid" {
		t.Fatalf("status=%s, want invalid", got.Status)
	}
	if err := store.ResetSnapshot(record.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateSnapshot(record.ID); err != nil {
		t.Fatalf("reset should be resumable: %v", err)
	}
	_ = snap
}

func TestVersionMismatchInvalidates(t *testing.T) {
	store, record, _, _ := testSnapshot(t)
	defer store.Close()
	if _, err := store.db.Exec(`UPDATE blocks SET param_version='wrong' WHERE snapshot_id=?`, record.ID); err != nil {
		t.Fatal(err)
	}
	err := store.ValidateSnapshot(record.ID)
	if err == nil || core.AsAppError(err).Code != core.CodeVersion {
		t.Fatalf("ValidateSnapshot error = %v, want E_VERSION", err)
	}
}

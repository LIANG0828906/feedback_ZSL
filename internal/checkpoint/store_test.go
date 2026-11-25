package checkpoint_test

import (
	"context"
	"path/filepath"
	"testing"

	"semanticshading/internal/checkpoint"
	"semanticshading/internal/model"
)

func TestIdentityMismatchInvalidatesWholeSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	id := checkpoint.Identity{InputHash: "in-1", ManifestHash: "m-1", TruthFingerprint: "truth-1", ParamVersion: model.ParamVersion, AlgoVersion: model.AlgorithmVersion}
	store, reset, err := checkpoint.Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Reset {
		t.Fatal("new database should not report reset")
	}
	cp := model.Checkpoint{
		InputHash: id.InputHash, ManifestHash: id.ManifestHash, TruthFingerprint: id.TruthFingerprint,
		ParamVersion: id.ParamVersion, AlgoVersion: id.AlgoVersion, CommitOrder: 0,
		Block: model.BlockRecord{Block: model.BlockCoord{Index: 0, BX: 0, BY: 0, X0: 0, Y0: 0, X1: 16, Y1: 16}},
	}
	if err := store.Commit(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	changed := id
	changed.InputHash = "in-2"
	store2, reset2, err := checkpoint.Open(path, changed)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	if !reset2.Reset {
		t.Fatal("hash mismatch must reset snapshot")
	}
	count, err := store2.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count after identity mismatch = %d, want 0", count)
	}
}

func TestOpenExistingDoesNotReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.sqlite")
	id := checkpoint.Identity{InputHash: "in", ManifestHash: "m", TruthFingerprint: "truth", ParamVersion: "p", AlgoVersion: "a"}
	store, _, err := checkpoint.Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), model.Checkpoint{InputHash: "in", ManifestHash: "m", ParamVersion: "p", AlgoVersion: "a", CommitOrder: 0, Block: model.BlockRecord{Block: model.BlockCoord{Index: 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	readStore, err := checkpoint.OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readStore.Close()
	if readStore.Identity().InputHash != "in" {
		t.Fatalf("identity = %#v", readStore.Identity())
	}
	count, _ := readStore.Count()
	if count != 1 {
		t.Fatalf("OpenExisting changed count to %d", count)
	}
}

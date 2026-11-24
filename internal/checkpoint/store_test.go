package checkpoint_test

import (
	"errors"
	"path/filepath"
	"testing"

	"storyboardstudio/internal/checkpoint"
	"storyboardstudio/internal/model"
)

func TestSnapshotHashAndVersionMismatchDiscardAllRows(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cp := checkpoint.Checkpoint{
		SessionID: "session", ShotGlobalIndex: 4, NodeID: "n01_office", ShotLocalIndex: 1,
		ConsumedChoiceOffset: 1, CursorMS: 1250, Flags: map[string]bool{"a": true},
		Summary:  checkpoint.NodeSummary{EventOrder: []string{"n00_world", "n01_office"}},
		ShotHash: "shot-hash", ContentHash: "project-hash", StructureVersion: model.ProjectSchemaV1,
		AlgorithmVersion: model.AlgorithmVersion,
	}
	shot := model.PresentedShot{GlobalIndex: 4, NodeID: "n01_office", ShotID: "shot", ContentHash: "shot-hash"}
	if err := store.CommitBoundary(cp, shot); err != nil {
		t.Fatal(err)
	}

	_, err = store.LoadValidCheckpoint("session", "wrong-hash", model.ProjectSchemaV1, model.AlgorithmVersion)
	var coded *model.Error
	if !errors.As(err, &coded) || coded.Code != model.CodeHash {
		t.Fatalf("expected E_HASH, got %v", err)
	}
	latest, err := store.Latest("session")
	if err != nil {
		t.Fatal(err)
	}
	if latest != nil {
		t.Fatal("hash mismatch did not discard checkpoint snapshot")
	}

	if err := store.CommitBoundary(cp, shot); err != nil {
		t.Fatal(err)
	}
	_, err = store.LoadValidCheckpoint("session", "project-hash", model.ProjectSchemaV1, "old-algo")
	if !errors.As(err, &coded) || coded.Code != model.CodeVersion {
		t.Fatalf("expected E_VERSION, got %v", err)
	}
	latest, _ = store.Latest("session")
	if latest != nil {
		t.Fatal("version mismatch did not discard checkpoint snapshot")
	}
}

func TestCheckpointWritesAreSerializableAndListed(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "serial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 25; i++ {
		cp := checkpoint.Checkpoint{
			SessionID: "serial", ShotGlobalIndex: i, NodeID: "n", ShotLocalIndex: i,
			Flags: map[string]bool{}, Summary: checkpoint.NodeSummary{}, ShotHash: "h",
			ContentHash: "hash", StructureVersion: model.ProjectSchemaV1, AlgorithmVersion: model.AlgorithmVersion,
		}
		if err := store.CommitBoundary(cp, model.PresentedShot{GlobalIndex: i, ContentHash: "h"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.ListCheckpoints("serial")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 25 {
		t.Fatalf("expected 25 checkpoints, got %d", len(rows))
	}
	seq, err := store.WriteSequence("serial")
	if err != nil {
		t.Fatal(err)
	}
	if len(seq) != 25 || seq[0] != 1 || seq[24] != 25 {
		t.Fatalf("write sequence was not serial: %v", seq)
	}
}

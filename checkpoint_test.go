package mirror

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCheckpointRejectsTamperedWindowAndRollsBack(t *testing.T) {
	sample := mustGenerate(t, 42)
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.db")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), sample.Session, RunOptions{
		Mode: ModeRealtime, Checkpoint: store, StopAfter: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`UPDATE checkpoint_components SET payload_json = '{"tampered":true}' WHERE window_seq = 2 AND component = 'endpoints'`,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, rejected, err := store.RestoreLatest(context.Background(), sample.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("no previous checkpoint restored")
	}
	if state.WindowSeq != 1 {
		t.Fatalf("restored sequence = %d, want 1", state.WindowSeq)
	}
	if len(rejected) != 1 || rejected[0].WindowSeq != 2 {
		t.Fatalf("rejected = %+v", rejected)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM checkpoints`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("checkpoint rows = %d, want 2", count)
	}
}

func TestCheckpointSaveRequiresSequentialWindows(t *testing.T) {
	store, err := OpenCheckpoint(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state := CheckpointState{
		SessionID: "s", SourceVersion: SchemaV3, AlgorithmVersion: AlgorithmVersion,
		WindowSeq: 3, LastWindowID: "w3",
	}
	state.ContentHash = ComputeCheckpointHash(state)
	if err := store.SaveWindow(context.Background(), state); !IsCode(err, CodeState) {
		t.Fatalf("error = %v, want E_STATE", err)
	}
}

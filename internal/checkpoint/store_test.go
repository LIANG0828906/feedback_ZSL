package checkpoint_test

import (
	"path/filepath"
	"testing"

	"channelpipe"
	"channelpipe/internal/checkpoint"
)

func TestSnapshotMismatchResetsAll(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	identity := checkpoint.Identity{PlanFingerprint: "plan-a", ParamVersion: channelpipe.ParamVersion, AlgorithmVersion: channelpipe.AlgorithmVersion}
	store, info, err := checkpoint.Open(dbPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	if info.Reset {
		t.Fatal("new database should not report reset")
	}
	chunk := channelpipe.Chunk{Index: 0, Rect: channelpipe.Rect{X: 0, Y: 0, Width: 2, Height: 2}}
	if err := store.EnsureChunk("image", chunk, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ChunkState("image", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	changed := identity
	changed.PlanFingerprint = "plan-b"
	reopened, openInfo, err := checkpoint.Open(dbPath, changed)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !openInfo.Reset {
		t.Fatal("expected identity mismatch to reset snapshot")
	}
	if _, err := reopened.ChunkState("image", 0); !channelpipe.IsCode(err, channelpipe.CodeState) {
		t.Fatalf("expected old chunk to be absent after reset, got %v", err)
	}
}

func TestIllegalStoreTransitionPreservesState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	identity := checkpoint.Identity{PlanFingerprint: "plan", ParamVersion: "p1", AlgorithmVersion: "a1"}
	store, _, err := checkpoint.Open(dbPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	chunk := channelpipe.Chunk{Index: 3, Rect: channelpipe.Rect{Width: 1, Height: 1}}
	if err := store.EnsureChunk("img", chunk, 1); err != nil {
		t.Fatal(err)
	}
	err = store.TransitionChunk("img", 3, channelpipe.StateCommitted)
	if !channelpipe.IsCode(err, channelpipe.CodeState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	state, err := store.ChunkState("img", 3)
	if err != nil {
		t.Fatal(err)
	}
	if state != channelpipe.StatePending {
		t.Fatalf("state = %s, want pending", state)
	}
}

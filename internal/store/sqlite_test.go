package store

import (
	"errors"
	"path/filepath"
	"testing"

	"roomreverb/internal/model"
)

func TestVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "checkpoint.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	result := model.RoomResult{RoomID: "R1", ParamVersion: "p1", AlgoVersion: "a1", CommitOrder: 0}
	if err := st.SaveRoom(result, 1); err != nil {
		t.Fatal(err)
	}
	rows, err := st.LoadValid("p1", "a1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected valid row, rows=%d err=%v", len(rows), err)
	}

	if _, err := st.db.Exec(`UPDATE room_checkpoints SET param_version='p2'`); err != nil {
		t.Fatal(err)
	}
	_, err = st.LoadValid("p1", "a1")
	if !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("expected ErrSnapshotInvalid, got %v", err)
	}
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM room_checkpoints`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected mismatch to discard all rows, count=%d", count)
	}
}

func TestHashMismatchInvalidatesSnapshot(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	result := model.RoomResult{RoomID: "R2", ParamVersion: "p1", AlgoVersion: "a1", CommitOrder: 0}
	if err := st.SaveRoom(result, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE room_checkpoints SET content_hash='deadbeef'`); err != nil {
		t.Fatal(err)
	}
	_, err = st.LoadValid("p1", "a1")
	if !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("expected hash mismatch invalidation, got %v", err)
	}
}

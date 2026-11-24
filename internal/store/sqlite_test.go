package store_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"levelacoustic/internal/model"
	"levelacoustic/internal/store"
)

func testCheckpoint(roomID string, order int) (model.RoomCheckpoint, error) {
	cp := model.RoomCheckpoint{
		RoomID:           roomID,
		Revision:         "rev-1",
		Params:           model.Room{ID: roomID},
		ParameterVersion: model.ParameterVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		CommitOrder:      order,
	}
	return cp, cp.Seal()
}

func TestCheckpointWritesAreSerialized(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "serial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var wg sync.WaitGroup
	for i, roomID := range []string{"R1", "R2", "R3", "R4"} {
		i, roomID := i, roomID
		wg.Add(1)
		go func() {
			defer wg.Done()
			cp, err := testCheckpoint(roomID, i+1)
			if err != nil {
				t.Errorf("seal: %v", err)
				return
			}
			if err := st.CommitRoom(context.Background(), cp); err != nil {
				t.Errorf("commit %s: %v", roomID, err)
			}
		}()
	}
	wg.Wait()
	rows, err := st.LoadAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d", len(rows))
	}
	for i, row := range rows {
		if row.RoomID != []string{"R1", "R2", "R3", "R4"}[i] || row.CommitOrder != i+1 {
			t.Fatalf("unstable checkpoint order: %+v", rows)
		}
	}
	logs, err := st.CheckpointLog(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 4 {
		t.Fatalf("log rows = %d", len(logs))
	}
	if st.Stats().Writes != 4 {
		t.Fatalf("stats = %+v", st.Stats())
	}
}

func TestCheckpointWriteRejectsHashMismatch(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hash.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cp, err := testCheckpoint("R1", 1)
	if err != nil {
		t.Fatal(err)
	}
	cp.ContentHash = "not-the-hash"
	if err := st.CommitRoom(context.Background(), cp); err == nil {
		t.Fatal("expected hash mismatch rejection")
	}
	if st.Stats().Writes != 0 {
		t.Fatalf("rejected write counted: %+v", st.Stats())
	}
}

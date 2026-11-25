package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"example.com/multiscreen-cast/kernel"
	"example.com/multiscreen-cast/protocol"
	"example.com/multiscreen-cast/samplegen"
	"example.com/multiscreen-cast/store"
)

func prepare(t *testing.T) (*protocol.Session, *store.Store) {
	t.Helper()
	set, err := samplegen.Generate(samplegen.Options{Seed: 23, Screens: 3, Windows: 6, Sessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoints.sqlite")
	checkpointStore, openErr := store.Open(dbPath)
	if openErr != nil {
		t.Fatal(openErr)
	}
	t.Cleanup(func() { _ = checkpointStore.Close() })
	return &set.Sessions[0], checkpointStore
}

func commitWindows(t *testing.T, session *protocol.Session, checkpointStore *store.Store, count int) *kernel.Engine {
	t.Helper()
	engine, err := kernel.NewEngine(session, kernel.Config{
		Mapping:    kernel.DefaultMappingConfig(),
		Checkpoint: checkpointStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	for engine.Progress().Committed < count && !engine.Done() {
		if _, advanceErr := engine.Advance(context.Background()); advanceErr != nil {
			t.Fatal(advanceErr)
		}
	}
	if engine.Progress().Committed != count {
		t.Fatalf("committed %d windows, want %d", engine.Progress().Committed, count)
	}
	return engine
}

func TestCheckpointRoundTripAndRows(t *testing.T) {
	session, checkpointStore := prepare(t)
	engine := commitWindows(t, session, checkpointStore, 4)
	inMemory := engine.Trace()
	rows, err := checkpointStore.Rows(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows=%d want=4", len(rows))
	}
	for i, row := range rows {
		if row.WindowIndex != i+1 {
			t.Fatalf("unstable row order at %d: %+v", i, row)
		}
	}
	snapshot, status, loadErr := checkpointStore.LoadLatest(session, protocol.CurrentAlgorithmVersion)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if status.Invalidated || snapshot == nil || snapshot.CommittedWindows != 4 {
		t.Fatalf("unexpected load result: status=%+v snapshot=%+v", status, snapshot)
	}
	restored, restoreErr := kernel.RestoreEngine(session, kernel.Config{Mapping: kernel.DefaultMappingConfig()}, *snapshot)
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if diff := kernel.CompareTraces(inMemory, restored.Trace()); diff != nil {
		t.Fatalf("restored trace differs: %v", diff)
	}
}

func TestOneCorruptRowInvalidatesAllSnapshots(t *testing.T) {
	session, checkpointStore := prepare(t)
	_ = commitWindows(t, session, checkpointStore, 4)
	if err := checkpointStore.CorruptSnapshotForTest(session.ID, 2, []byte(`{"broken":`)); err != nil {
		t.Fatal(err)
	}
	snapshot, status, loadErr := checkpointStore.LoadLatest(session, protocol.CurrentAlgorithmVersion)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !status.Invalidated || snapshot != nil {
		t.Fatalf("expected whole-set invalidation, got status=%+v snapshot=%+v", status, snapshot)
	}
	rows, err := checkpointStore.Rows(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("invalidated snapshots remain: %+v", rows)
	}
}

func TestTopologyVersionMismatchInvalidatesAll(t *testing.T) {
	session, checkpointStore := prepare(t)
	_ = commitWindows(t, session, checkpointStore, 3)
	if err := checkpointStore.TamperTopologyVersionForTest(session.ID, 2, 999); err != nil {
		t.Fatal(err)
	}
	snapshot, status, loadErr := checkpointStore.LoadLatest(session, protocol.CurrentAlgorithmVersion)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !status.Invalidated || snapshot != nil {
		t.Fatalf("expected topology invalidation, got status=%+v snapshot=%+v", status, snapshot)
	}
}

func TestContentHashAndAlgorithmMismatchInvalidateAll(t *testing.T) {
	t.Run("content", func(t *testing.T) {
		session, checkpointStore := prepare(t)
		_ = commitWindows(t, session, checkpointStore, 3)
		session.Windows[0].Region.X++
		snapshot, status, loadErr := checkpointStore.LoadLatest(session, protocol.CurrentAlgorithmVersion)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if !status.Invalidated || snapshot != nil {
			t.Fatalf("expected content invalidation, got status=%+v", status)
		}
	})
	t.Run("algorithm", func(t *testing.T) {
		session, checkpointStore := prepare(t)
		_ = commitWindows(t, session, checkpointStore, 3)
		snapshot, status, loadErr := checkpointStore.LoadLatest(session, "different-algorithm")
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if !status.Invalidated || snapshot != nil {
			t.Fatalf("expected algorithm invalidation, got status=%+v", status)
		}
	})
}

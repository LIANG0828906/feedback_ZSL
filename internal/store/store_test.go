package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/store"

	_ "modernc.org/sqlite"
)

func TestPrepareLoadsAndInvalidatesOnVersionMismatch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key := testKey("param-v1")
	snapshotID, snapshot, invalidated, err := st.Prepare(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot != nil || invalidated {
		t.Fatalf("fresh prepare returned snapshot=%v invalidated=%v", snapshot != nil, invalidated)
	}
	for i := 0; i < 2; i++ {
		tile := core.TileResult{
			ImageID:      key.ImageID,
			Index:        i,
			State:        "committed",
			CommittedSeq: i,
			Core:         core.Rect{X: i * 10, Width: 10, Height: 10},
		}
		if err := st.SaveTile(ctx, snapshotID, key, tile); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkComplete(ctx, snapshotID); err != nil {
		t.Fatal(err)
	}
	loadedID, loaded, invalidated, err := st.Prepare(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated || loaded == nil || loadedID != snapshotID || !loaded.Complete || len(loaded.Tiles) != 2 {
		t.Fatalf("matching snapshot not loaded: id=%d invalidated=%v snapshot=%+v", loadedID, invalidated, loaded)
	}
	changed := key
	changed.ParameterVersion = "param-v2"
	newID, loaded, invalidated, err := st.Prepare(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	if !invalidated || loaded != nil || newID == snapshotID {
		t.Fatalf("version mismatch did not invalidate old snapshot: id=%d invalidated=%v", newID, invalidated)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var valid bool
	if err := db.QueryRow(`SELECT valid FROM snapshots WHERE id=?`, snapshotID).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("old snapshot remained valid")
	}
}

func TestUnitHashTamperInvalidatesWholeSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key := testKey("param-v1")
	id, _, _, err := st.Prepare(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		tile := core.TileResult{ImageID: key.ImageID, Index: i, State: "committed", CommittedSeq: i}
		if err := st.SaveTile(ctx, id, key, tile); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tile_units SET content_hash='tampered' WHERE unit_index=1`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newID, snapshot, invalidated, err := st.Prepare(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !invalidated || snapshot != nil || newID == id {
		t.Fatalf("hash tamper did not invalidate whole snapshot: new=%d old=%d invalidated=%v snapshot=%v", newID, id, invalidated, snapshot != nil)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var valid bool
	if err := db.QueryRow(`SELECT valid FROM snapshots WHERE id=?`, id).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("tampered snapshot remained valid")
	}
}

func TestConcurrentSavesAreSerializedAndLoadedInSequence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key := testKey("param-v1")
	id, _, _, err := st.Prepare(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	const count = 20
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			tile := core.TileResult{ImageID: key.ImageID, Index: index, State: "committed", CommittedSeq: index}
			errs <- st.SaveTile(ctx, id, key, tile)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := st.LoadSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tiles) != count {
		t.Fatalf("loaded %d tiles, want %d", len(snapshot.Tiles), count)
	}
	seen := map[string]bool{}
	for i, tile := range snapshot.Tiles {
		if tile.Index != i || tile.CommittedSeq != i {
			t.Fatalf("tile %d loaded out of sequence: index=%d seq=%d", i, tile.Index, tile.CommittedSeq)
		}
		if tile.ContentHash == "" || seen[tile.ContentHash] {
			t.Fatalf("tile %d has missing or duplicate content hash", i)
		}
		seen[tile.ContentHash] = true
	}
}

func testKey(parameterVersion string) store.Key {
	return store.Key{
		ImageID:          "image-1",
		ContentHash:      "image-content",
		TruthHash:        "truth-fingerprint",
		TemplateVersion:  "template-v1",
		ParameterVersion: parameterVersion,
		AlgorithmVersion: "algorithm-v1",
	}
}

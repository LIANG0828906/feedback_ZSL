package checkpoint

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"roomreverb/internal/generator"
	"roomreverb/internal/model"
	"roomreverb/internal/solver"
)

func fixture(t *testing.T) (model.Level, map[string]solver.RoomResult) {
	t.Helper()
	b, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	batch := solver.SolveRooms(context.Background(), b.Level, SortedRoomIDs(b.Level), solver.Config{
		MaxParallel: 2, PathBufferLimit: 64, GraphCacheBudget: 1,
	})
	if len(batch.Failures) > 0 {
		t.Fatalf("solve failures: %v", batch.Failures)
	}
	return b.Level, batch.Rooms
}

func TestCheckpointPersistsStableOrderAndHashes(t *testing.T) {
	level, rooms := fixture(t)
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	meta := RunMeta{RunID: "run", LevelHash: LevelHash(level), ParamsVersion: 3, AlgorithmVersion: model.AlgorithmVersion, Precision: model.Precision}
	prep, err := store.Prepare(context.Background(), meta)
	if err != nil || prep.Reset || len(prep.Snapshots) != 0 {
		t.Fatalf("initial prepare: %+v %v", prep, err)
	}
	for i, id := range SortedRoomIDs(level) {
		snap, err := store.SaveRoom(context.Background(), meta, rooms[id])
		if err != nil {
			t.Fatal(err)
		}
		if snap.Seq != i+1 || snap.ContentHash == "" || snap.ParamsVersion != 3 {
			t.Fatalf("bad snapshot header: %+v", snap)
		}
	}
	_ = store.Close()

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prep, err = store.Prepare(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	if prep.Reset || len(prep.Snapshots) != 6 {
		t.Fatalf("resume prepare: %+v", prep)
	}
	for i, snap := range prep.Snapshots {
		if snap.Seq != i+1 {
			t.Fatalf("snapshot order changed: %+v", snap)
		}
	}
}

func TestVersionMismatchDiscardsWholeSnapshot(t *testing.T) {
	level, rooms := fixture(t)
	path := filepath.Join(t.TempDir(), "version.sqlite")
	store, _ := Open(path)
	meta := RunMeta{RunID: "run", LevelHash: LevelHash(level), ParamsVersion: 1, AlgorithmVersion: model.AlgorithmVersion, Precision: model.Precision}
	_, _ = store.Prepare(context.Background(), meta)
	for _, id := range SortedRoomIDs(level)[:2] {
		if _, err := store.SaveRoom(context.Background(), meta, rooms[id]); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.Close()
	store, _ = Open(path)
	defer store.Close()
	meta.ParamsVersion = 2
	prep, err := store.Prepare(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	if !prep.Reset || len(prep.Snapshots) != 0 {
		t.Fatalf("version mismatch must reset all: %+v", prep)
	}
	n, _ := store.CountSnapshots(context.Background(), "run")
	if n != 0 {
		t.Fatalf("old snapshots survived reset: %d", n)
	}
}

func TestTamperedRoomHashDiscardsWholeSnapshot(t *testing.T) {
	level, rooms := fixture(t)
	path := filepath.Join(t.TempDir(), "tamper.sqlite")
	store, _ := Open(path)
	defer store.Close()
	meta := RunMeta{RunID: "run", LevelHash: LevelHash(level), ParamsVersion: 1, AlgorithmVersion: model.AlgorithmVersion, Precision: model.Precision}
	_, _ = store.Prepare(context.Background(), meta)
	for _, id := range SortedRoomIDs(level)[:3] {
		if _, err := store.SaveRoom(context.Background(), meta, rooms[id]); err != nil {
			t.Fatal(err)
		}
	}
	var payload string
	if err := store.db.QueryRow(`SELECT payload_json FROM snapshots WHERE run_id=? AND seq=2`, "run").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal([]byte(payload), &obj)
	obj["result"].(map[string]any)["pairs"] = []any{}
	tampered, _ := json.Marshal(obj)
	if _, err := store.db.Exec(`UPDATE snapshots SET payload_json=? WHERE run_id=? AND seq=2`, string(tampered), "run"); err != nil {
		t.Fatal(err)
	}
	prep, err := store.Prepare(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	if !prep.Reset || len(prep.Snapshots) != 0 {
		t.Fatalf("tampered hash must reset all: %+v", prep)
	}
	n, _ := store.CountSnapshots(context.Background(), "run")
	if n != 0 {
		t.Fatalf("mixed old/new snapshots after reset: %d", n)
	}
}

func TestRoomCacheRoundTrip(t *testing.T) {
	level, rooms := fixture(t)
	path := filepath.Join(t.TempDir(), "cache.sqlite")
	store, _ := Open(path)
	defer store.Close()
	id := "r0"
	hash := RoomInputHash(level, id)
	if err := store.SaveCache(context.Background(), rooms[id], hash, 2); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.LoadCache(context.Background(), id, hash, 2)
	if err != nil || !ok {
		t.Fatalf("cache load: %v %v", ok, err)
	}
	if string(got.CanonicalJSON()) != string(rooms[id].CanonicalJSON()) {
		t.Fatal("cached room result changed")
	}
	_, ok, _ = store.LoadCache(context.Background(), id, hash, 3)
	if ok {
		t.Fatal("cache must be version scoped")
	}
}

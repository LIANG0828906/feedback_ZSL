package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"occlusionlab/internal/generator"
	"occlusionlab/internal/model"
	"occlusionlab/internal/store"
	"occlusionlab/internal/streaming"
)

func testSetup(t *testing.T) (*model.Project, model.Params, string, store.Header) {
	t.Helper()
	project, err := generator.Generate(generator.DefaultConfig(1042))
	if err != nil {
		t.Fatal(err)
	}
	params := model.Params{
		Quantization: project.Meta.Precision, RasterResolution: 96,
		MaxWorkers: 2, RasterBufferCells: 128 * 128, MaxResident: 15,
		LoadCostBudget: 1000, TolerancePxQ: project.Meta.TolerancePxQ, Version: 1,
	}
	header := store.Header{
		SchemaVersion: model.SchemaVersion,
		StoreVersion:  store.StoreVersion,
		SceneHash:     project.ContentHash,
	}
	return project, params, filepath.Join(t.TempDir(), "checkpoints.sqlite"), header
}

func processWindows(t *testing.T, project *model.Project, params model.Params,
	checkpoint *store.CheckpointStore, count int) []*streaming.WindowFrame {
	t.Helper()
	engine, err := streaming.NewEngine(project, params, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		frame, processErr := engine.ProcessNext(context.Background())
		if processErr != nil {
			t.Fatal(processErr)
		}
		if frame == nil {
			t.Fatal("unexpected end of windows")
		}
	}
	return engine.CommittedFrames()
}

func TestCheckpointRoundTripAndResume(t *testing.T) {
	project, params, path, header := testSetup(t)
	checkpoint, err := store.Open(path, header)
	if err != nil {
		t.Fatal(err)
	}
	frames := processWindows(t, project, params, checkpoint, 2)
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(path, header)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Invalidated || loaded.Count != 2 {
		t.Fatalf("unexpected load result: %+v", loaded)
	}
	if loaded.Snapshots[0].Frame.ResultHash != frames[0].ResultHash ||
		loaded.Snapshots[1].Frame.ResultHash != frames[1].ResultHash {
		t.Fatal("restored result hashes differ from committed frames")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedCheckpointInvalidatesWholeSnapshot(t *testing.T) {
	project, params, path, header := testSetup(t)
	checkpoint, err := store.Open(path, header)
	if err != nil {
		t.Fatal(err)
	}
	processWindows(t, project, params, checkpoint, 2)
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}

	db, dbErr := sql.Open("sqlite", path)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if _, err := db.Exec(`UPDATE window_checkpoints SET result_json = '{"broken":true}' WHERE window_index = 0`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(path, header)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Invalidated || loaded.Count != 0 {
		t.Fatalf("expected whole snapshot invalidation, got %+v", loaded)
	}
	again, err := reopened.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if again.Invalidated || again.Count != 0 {
		t.Fatalf("invalidated snapshot was not fully cleared: %+v", again)
	}
}

func TestSceneHeaderMismatchInvalidatesWholeSnapshot(t *testing.T) {
	project, params, path, header := testSetup(t)
	checkpoint, err := store.Open(path, header)
	if err != nil {
		t.Fatal(err)
	}
	processWindows(t, project, params, checkpoint, 3)
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}
	header.SceneHash = "different-scene-hash"
	reopened, err := store.Open(path, header)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Invalidated || loaded.Count != 0 {
		t.Fatalf("expected header mismatch invalidation, got %+v", loaded)
	}
}

func TestSnapshotAppendRequiresStableWindowOrder(t *testing.T) {
	project, params, path, header := testSetup(t)
	checkpoint, err := store.Open(path, header)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Close()
	engine, err := streaming.NewEngine(project, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := engine.ProcessNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := streaming.Snapshot{
		Window: 1, Frame: frame, Params: params, SceneHash: project.ContentHash,
		ParamsHash: params.Hash(), ParamsVersion: params.Version,
	}
	if err := checkpoint.AppendSnapshot(snapshot); err == nil {
		t.Fatal("store accepted a non-contiguous first window")
	}
}

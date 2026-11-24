package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"virtualstudio/compose"
	"virtualstudio/model"
	"virtualstudio/store"
)

func testCheckpoint(t *testing.T, projectID string) (store.Checkpoint, store.Project) {
	t.Helper()
	scene := &model.Scene{
		SourceVersion: "v1", Width: 1, Height: 1, Channels: 4, FrameCount: 1, FrameDurationNS: 10,
		Layers: []model.Layer{{ID: "bg", Name: "bg", Visible: true, Opacity: 1,
			SourceAlphaMode: model.AlphaStraight, Pixels: []byte{10, 20, 30, 255}}},
	}
	commands := &model.CommandSet{Version: "test", WindowFrames: 1,
		Frames: []model.FrameCommand{{Frame: 0, Visible: map[string]bool{"bg": true}}}}
	frames, err := compose.RenderAll(scene, commands, compose.RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := model.BuildWindowSnapshot(scene, commands, 0, 0, 0)
	cp := store.Checkpoint{
		ProjectID: projectID, WindowIndex: 0, StartFrame: 0, EndFrame: 0, Sequence: 0,
		ParamVersion: snapshot.ParamVersion, ContentHash: snapshot.ContentHash,
		FrameDigest: compose.WindowDigest(frames), SourceVersion: scene.SourceVersion,
		AlgorithmVersion: model.AlgorithmVersion, Snapshot: snapshot, Frames: frames,
	}
	sceneJSON := []byte(`{"source_version":"v1"}`)
	commandsJSON := []byte(`{"version":"test"}`)
	project := store.Project{
		ID: projectID, AlgorithmVersion: model.AlgorithmVersion, SourceVersion: "v1",
		BaseHash: scene.BaseHash(), WindowSize: 1, FrameCount: 1, ProjectVersion: 1,
		TargetFormat: "v3", SceneJSON: sceneJSON, CommandsJSON: commandsJSON,
	}
	return cp, project
}

func TestCheckpointRoundTripAndPoisonPurge(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cp, project := testCheckpoint(t, "p")
	if err := st.SaveProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	checkpoints, err := st.LoadCheckpoints(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 1 || checkpoints[0].Snapshot.ParamVersion != cp.ParamVersion {
		t.Fatalf("unexpected checkpoints: %+v", checkpoints)
	}
	loaded, err := st.LoadProject(ctx, "p")
	if err != nil || loaded.ID != "p" {
		t.Fatalf("project=%+v err=%v", loaded, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE checkpoints SET frame_digest='poisoned' WHERE project_id='p'`); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, err = reopened.LoadCheckpoints(ctx, "p")
	var invalid *store.InvalidatedError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected InvalidatedError, got %v", err)
	}
	if _, err := reopened.LoadProject(ctx, "p"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project should have been purged, got %v", err)
	}
}

func TestVersionMismatchRejected(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cp, project := testCheckpoint(t, "p")
	if err := st.SaveProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	cp.AlgorithmVersion = "other"
	if err := st.PutCheckpoint(ctx, cp); !model.IsCode(err, model.CodeState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
}

func TestStatsTamperPurgesSnapshot(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "stats.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cp, project := testCheckpoint(t, "stats")
	if err := st.SaveProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := sqlDB.QueryRow(`SELECT frames_json FROM checkpoints WHERE project_id='stats'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var frames []model.Frame
	if err := json.Unmarshal(raw, &frames); err != nil {
		t.Fatal(err)
	}
	frames[0].Stats.MeanRGBA[0] += 1
	raw, err = json.Marshal(frames)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE checkpoints SET frames_json=? WHERE project_id='stats'`, raw); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, err = reopened.LoadCheckpoints(ctx, "stats")
	var invalid *store.InvalidatedError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected stats tamper invalidation, got %v", err)
	}
}

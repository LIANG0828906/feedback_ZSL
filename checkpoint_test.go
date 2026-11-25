package vpcompose_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"vpcompose"
)

func TestCheckpointRecoveryAndOverallInvalidation(t *testing.T) {
	project := loadProjectForTest(t, 30)
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := vpcompose.OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := vpcompose.NewEngine(project, store, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(context.Background(), vpcompose.ModeRealtime); err != nil {
		t.Fatal(err)
	}
	before, err := engine.Comparable("before")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = vpcompose.OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recoveredEngine, err := vpcompose.NewEngine(project, store, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := recoveredEngine.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Invalidated || !equalInts(recovery.Committed, []int{0, 1, 2}) {
		t.Fatalf("recovery = %+v", recovery)
	}
	after, err := recoveredEngine.Comparable("after")
	if err != nil {
		t.Fatal(err)
	}
	if err := vpcompose.CompareRuns(before, after); err != nil {
		t.Fatalf("recovered pixels differ: %v", err)
	}

	changed := project.Clone()
	changed.Width = 80
	mismatchEngine, err := vpcompose.NewEngine(changed, store, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	mismatch, err := mismatchEngine.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if !mismatch.Invalidated {
		t.Fatalf("version mismatch persisted: %+v", mismatch)
	}
	for _, state := range mismatchEngine.Windows() {
		if state.State != vpcompose.WindowPending {
			t.Fatalf("window %d not invalidated: %s", state.Index, state.State)
		}
	}
}

func TestForcedStopRecoversOnlyCommittedPrefix(t *testing.T) {
	project := loadProjectForTest(t, 31)
	path := filepath.Join(t.TempDir(), "forced-stop.db")
	store, err := vpcompose.OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := vpcompose.NewEngine(project, store, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ComposeWindow(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	// Close simulates process termination after a committed window. No engine
	// exit report is written or required.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = vpcompose.OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err = vpcompose.NewEngine(project, store, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := engine.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(recovery.Committed, []int{0}) {
		t.Fatalf("recovered = %v", recovery.Committed)
	}
	if first := vpcompose.FirstUncommittedWindow(recovery.Committed, project.WindowCount()); first != 1 {
		t.Fatalf("first uncommitted = %d", first)
	}
	run, err := engine.Run(context.Background(), vpcompose.ModeRecover)
	if err != nil {
		t.Fatal(err)
	}
	if run.Progress.Committed != 3 {
		t.Fatalf("recovery continuation = %+v", run.Progress)
	}
}

func TestCheckpointRowContentMismatchWipesWholeSnapshot(t *testing.T) {
	project := loadProjectForTest(t, 32)
	path := filepath.Join(t.TempDir(), "tampered.db")
	store, err := vpcompose.OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := vpcompose.NewEngine(project, store, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(context.Background(), vpcompose.ModeRealtime); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE windows SET content_hash='tampered' WHERE window_index=1`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = vpcompose.OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err = vpcompose.NewEngine(project, store, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := engine.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Invalidated || len(recovery.Committed) != 0 {
		t.Fatalf("tampered checkpoint recovery = %+v", recovery)
	}
	if first := vpcompose.FirstUncommittedWindow(recovery.Committed, project.WindowCount()); first != 0 {
		t.Fatalf("first rebuilt window = %d", first)
	}
}

func TestCommittedSnapshotsAllowLaterEditsAndIncrementalCommits(t *testing.T) {
	project := loadProjectForTest(t, 33)
	path := filepath.Join(t.TempDir(), "incremental.db")
	store, err := vpcompose.OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := vpcompose.NewEngine(project, store, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ComposeWindow(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	visible := false
	report, err := engine.EditLayer(vpcompose.LayerEdit{LayerID: "beauty-glow", Visible: &visible})
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(report.WindowIDs(), []int{1}) {
		t.Fatalf("invalidation = %v", report.WindowIDs())
	}
	run, err := engine.Run(context.Background(), vpcompose.ModeRealtime)
	if err != nil {
		t.Fatal(err)
	}
	if run.Progress.Committed != 3 {
		t.Fatalf("incremental commit progress = %+v", run.Progress)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = vpcompose.OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovered, err := vpcompose.NewEngine(project, store, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := recovered.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Invalidated || !equalInts(recovery.Committed, []int{0, 1, 2}) {
		t.Fatalf("recovery = %+v", recovery)
	}
	windows := recovered.Windows()
	if windows[0].Snapshot.Layers[2].Visible != true {
		t.Fatal("window 0 frozen snapshot was not preserved")
	}
	if windows[1].Snapshot.Layers[2].Visible != false {
		t.Fatal("window 1 did not capture the edited snapshot")
	}
}

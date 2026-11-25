package checkpoint_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"terrainbake/bake"
	"terrainbake/checkpoint"
	"terrainbake/generator"
	"terrainbake/model"
)

func storeBundle(t *testing.T) model.SampleBundle {
	t.Helper()
	bundle, err := generator.Generate(424242)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return bundle
}

func newStoredEngine(t *testing.T, path string, p *model.Project) (*checkpoint.Store, *bake.Engine) {
	t.Helper()
	store, err := checkpoint.Open(context.Background(), path, model.ProjectIdentity(*p), p.Params)
	if err != nil {
		t.Fatalf("open checkpoint: %v", err)
	}
	engine, err := bake.NewEngine(p, store)
	if err != nil {
		store.Close()
		t.Fatalf("new engine: %v", err)
	}
	return store, engine
}

func TestCheckpointRestartReusesEveryUnit(t *testing.T) {
	bundle := storeBundle(t)
	opts := bake.DefaultScheduleOptions(bundle.Project.Params)
	path := filepath.Join(t.TempDir(), "bake.sqlite")
	store, engine := newStoredEngine(t, path, &bundle.Project)
	if _, err := engine.LoadPersisted(context.Background()); err != nil {
		t.Fatalf("empty load: %v", err)
	}
	baseline, err := engine.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("bake full: %v", err)
	}
	count, err := store.Count(context.Background())
	if err != nil || count != len(baseline.CommitSequence) {
		t.Fatalf("count=%d err=%v want=%d", count, err, len(baseline.CommitSequence))
	}
	order, err := store.CommitOrder(context.Background())
	if err != nil {
		t.Fatalf("commit order: %v", err)
	}
	if len(order) != len(baseline.CommitSequence) {
		t.Fatalf("commit order length=%d", len(order))
	}
	for i := range order {
		if order[i] != baseline.CommitSequence[i] {
			t.Fatalf("commit order[%d]=%+v want=%+v", i, order[i], baseline.CommitSequence[i])
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store2, engine2 := newStoredEngine(t, path, &bundle.Project)
	defer store2.Close()
	loaded, err := engine2.LoadPersisted(context.Background())
	if err != nil {
		t.Fatalf("resume load: %v", err)
	}
	if loaded != len(baseline.CommitSequence) {
		t.Fatalf("loaded=%d", loaded)
	}
	resumed, err := engine2.BakeIncremental(context.Background(), opts)
	if err != nil {
		t.Fatalf("resume incremental: %v", err)
	}
	if len(resumed.Rebaked) != 0 {
		t.Fatalf("restart rebaked %d units", len(resumed.Rebaked))
	}
	if _, err := bake.CompareResults(baseline, resumed); err != nil {
		t.Fatalf("restart result mismatch: %v", err)
	}
}

func TestPartialCheckpointResumeMatchesFull(t *testing.T) {
	bundle := storeBundle(t)
	opts := bake.DefaultScheduleOptions(bundle.Project.Params)
	fullEngine, _ := bake.NewEngine(&bundle.Project, nil)
	full, err := fullEngine.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	path := filepath.Join(t.TempDir(), "partial.sqlite")
	store, err := checkpoint.Open(context.Background(), path, model.ProjectIdentity(bundle.Project), bundle.Project.Params)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const prefix = 12
	for i := 0; i < prefix; i++ {
		ref := full.CommitSequence[i]
		unit, _ := full.Unit(ref.ChunkID, ref.Time)
		if err := store.SaveUnit(context.Background(), unit); err != nil {
			t.Fatalf("save unit %d: %v", i, err)
		}
	}
	store.Close()

	store2, engine2 := newStoredEngine(t, path, &bundle.Project)
	defer store2.Close()
	loaded, err := engine2.LoadPersisted(context.Background())
	if err != nil {
		t.Fatalf("load prefix: %v", err)
	}
	if loaded != prefix {
		t.Fatalf("loaded=%d want=%d", loaded, prefix)
	}
	resumed, err := engine2.BakeIncremental(context.Background(), opts)
	if err != nil {
		t.Fatalf("resume bake: %v", err)
	}
	if len(resumed.Reused) != prefix {
		t.Fatalf("reused=%d want=%d", len(resumed.Reused), prefix)
	}
	if _, err := bake.CompareResults(full, resumed); err != nil {
		t.Fatalf("partial resume mismatch: %v", err)
	}
	order, err := store2.CommitOrder(context.Background())
	if err != nil {
		t.Fatalf("resumed commit order: %v", err)
	}
	if len(order) != len(full.CommitSequence) {
		t.Fatalf("resumed commit order length=%d want=%d", len(order), len(full.CommitSequence))
	}
	for i := range order {
		if order[i] != full.CommitSequence[i] {
			t.Fatalf("resumed commit order[%d]=%+v want=%+v", i, order[i], full.CommitSequence[i])
		}
	}
}

func TestContentMismatchInvalidatesWholeSnapshot(t *testing.T) {
	bundle := storeBundle(t)
	opts := bake.DefaultScheduleOptions(bundle.Project.Params)
	path := filepath.Join(t.TempDir(), "content.sqlite")
	store, engine := newStoredEngine(t, path, &bundle.Project)
	if _, err := engine.BakeFull(context.Background(), opts); err != nil {
		t.Fatalf("bake: %v", err)
	}
	store.Close()
	bundle.Project.Chunks[0].Heights[0] += 1
	store2, engine2 := newStoredEngine(t, path, &bundle.Project)
	defer store2.Close()
	_, err := engine2.LoadPersisted(context.Background())
	if model.CodeOf(err) != model.CodeSnap {
		t.Fatalf("want E_SNAP, got %v", err)
	}
	count, err := store2.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("snapshot retained %d partially spliced rows", count)
	}
}

func TestVersionMismatchClearsSnapshot(t *testing.T) {
	bundle := storeBundle(t)
	path := filepath.Join(t.TempDir(), "version.sqlite")
	store, engine := newStoredEngine(t, path, &bundle.Project)
	if _, err := engine.BakeFull(context.Background(), bake.DefaultScheduleOptions(bundle.Project.Params)); err != nil {
		t.Fatalf("bake: %v", err)
	}
	store.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`UPDATE snapshot SET algorithm_version='tampered' WHERE id=1`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	db.Close()
	_, err = checkpoint.Open(context.Background(), path, model.ProjectIdentity(bundle.Project), bundle.Project.Params)
	if model.CodeOf(err) != model.CodeSnap {
		t.Fatalf("want E_SNAP, got %v", err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw reopen: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM units`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("version mismatch left %d units", count)
	}
}

func TestEditKeepsUnaffectedPersistentUnitsAcrossRestart(t *testing.T) {
	bundle := storeBundle(t)
	opts := bake.DefaultScheduleOptions(bundle.Project.Params)
	path := filepath.Join(t.TempDir(), "edit-resume.sqlite")
	store, engine := newStoredEngine(t, path, &bundle.Project)
	if _, err := engine.BakeFull(context.Background(), opts); err != nil {
		t.Fatalf("bake: %v", err)
	}
	invalidations, err := engine.RaiseHeight(context.Background(), "chunk-00-00", model.VertexPoint{X: 1, Z: 1}, 4.0)
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if len(invalidations) == 0 {
		t.Fatal("edit had no invalidations")
	}
	incremental, err := engine.BakeIncremental(context.Background(), opts)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if len(incremental.Reused) == 0 {
		t.Fatal("edit did not reuse any units")
	}
	store.Close()

	store2, engine2 := newStoredEngine(t, path, engine.Project())
	defer store2.Close()
	loaded, err := engine2.LoadPersisted(context.Background())
	if err != nil {
		t.Fatalf("restart load: %v", err)
	}
	if loaded != len(incremental.CommitSequence) {
		t.Fatalf("loaded=%d want=%d", loaded, len(incremental.CommitSequence))
	}
	resumed, err := engine2.BakeIncremental(context.Background(), opts)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(resumed.Rebaked) != 0 {
		t.Fatalf("restart after edit rebaked %d persisted units", len(resumed.Rebaked))
	}
	if _, err := bake.CompareResults(incremental, resumed); err != nil {
		t.Fatalf("persisted edit resume mismatch: %v", err)
	}
}

package workbench_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"resource-workbench/internal/store"
	"resource-workbench/internal/workbench"
)

func TestSQLiteResumeAndSnapshotInvalidation(t *testing.T) {
	region, genErr := workbench.GenerateSample(42)
	if genErr != nil {
		t.Fatalf("generate sample: %v", genErr)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoints.sqlite")
	db, openErr := store.Open(dbPath)
	if openErr != nil {
		t.Fatalf("open sqlite: %v", openErr)
	}
	engine, err := workbench.NewEngine(region, workbench.RuntimeConfig{MaxParallel: 2}, db)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	first, runErr := engine.Run(context.Background())
	if runErr != nil {
		t.Fatalf("first run: %v", runErr)
	}
	if first.Snapshot.Committed != len(first.CommitOrder) {
		t.Fatalf("committed %d checkpoints, want %d", first.Snapshot.Committed, len(first.CommitOrder))
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close sqlite: %v", closeErr)
	}

	db2, openErr := store.Open(dbPath)
	if openErr != nil {
		t.Fatalf("reopen sqlite: %v", openErr)
	}
	defer db2.Close()
	resumed, err := workbench.NewEngine(region, workbench.RuntimeConfig{MaxParallel: 2}, db2)
	if err != nil {
		t.Fatalf("resume engine: %v", err)
	}
	resumedRun, runErr := resumed.Run(context.Background())
	if runErr != nil {
		t.Fatalf("resume run: %v", runErr)
	}
	if len(resumedRun.Recomputed) != 0 {
		t.Fatalf("resume recomputed %v, want no zones", resumedRun.Recomputed)
	}
	firstJSON, _ := json.Marshal(first.Points)
	resumedJSON, _ := json.Marshal(resumedRun.Points)
	if string(firstJSON) != string(resumedJSON) {
		t.Fatal("resumed points differ")
	}
}

func TestSQLiteMismatchInvalidatesWholeSnapshot(t *testing.T) {
	region, genErr := workbench.GenerateSample(42)
	if genErr != nil {
		t.Fatalf("generate sample: %v", genErr)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoints.sqlite")
	db, openErr := store.Open(dbPath)
	if openErr != nil {
		t.Fatalf("open sqlite: %v", openErr)
	}
	engine, err := workbench.NewEngine(region, workbench.RuntimeConfig{Serial: true}, db)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	first, runErr := engine.Run(context.Background())
	if runErr != nil {
		t.Fatalf("first run: %v", runErr)
	}
	_ = db.Close()

	raw, rawErr := sql.Open("sqlite", dbPath)
	if rawErr != nil {
		t.Fatalf("open raw sqlite: %v", rawErr)
	}
	if _, execErr := raw.Exec(`UPDATE zone_checkpoints SET param_version = 'tampered'
		WHERE id = (SELECT id FROM zone_checkpoints WHERE is_current = 1 ORDER BY id LIMIT 1)`); execErr != nil {
		_ = raw.Close()
		t.Fatalf("tamper checkpoint: %v", execErr)
	}
	_ = raw.Close()

	db2, openErr := store.Open(dbPath)
	if openErr != nil {
		t.Fatalf("reopen sqlite: %v", openErr)
	}
	defer db2.Close()
	recovered, err := workbench.NewEngine(region, workbench.RuntimeConfig{Serial: true}, db2)
	if err != nil {
		t.Fatalf("recovered engine: %v", err)
	}
	run, runErr := recovered.Run(context.Background())
	if runErr != nil {
		t.Fatalf("recovered run: %v", runErr)
	}
	if !run.Snapshot.Invalidated {
		t.Fatal("snapshot mismatch was not reported")
	}
	if len(run.Recomputed) != len(run.CommitOrder) {
		t.Fatalf("snapshot mismatch partially reused zones: %v", run.Recomputed)
	}
	firstJSON, _ := json.Marshal(first.Points)
	runJSON, _ := json.Marshal(run.Points)
	if string(firstJSON) != string(runJSON) {
		t.Fatal("full rebuild after invalidation differs from original")
	}
}

func TestSQLiteResumesMissingZoneOnly(t *testing.T) {
	region, genErr := workbench.GenerateSample(42)
	if genErr != nil {
		t.Fatalf("generate sample: %v", genErr)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoints.sqlite")
	db, openErr := store.Open(dbPath)
	if openErr != nil {
		t.Fatalf("open sqlite: %v", openErr)
	}
	engine, err := workbench.NewEngine(region, workbench.RuntimeConfig{Serial: true}, db)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	first, runErr := engine.Run(context.Background())
	if runErr != nil {
		t.Fatalf("first run: %v", runErr)
	}
	_ = db.Close()

	raw, rawErr := sql.Open("sqlite", dbPath)
	if rawErr != nil {
		t.Fatalf("open raw sqlite: %v", rawErr)
	}
	if _, execErr := raw.Exec(`DELETE FROM zone_checkpoints
		WHERE is_current = 1 AND zone_id = 'zone-2-northeast'`); execErr != nil {
		_ = raw.Close()
		t.Fatalf("delete checkpoint: %v", execErr)
	}
	_ = raw.Close()

	db2, openErr := store.Open(dbPath)
	if openErr != nil {
		t.Fatalf("reopen sqlite: %v", openErr)
	}
	defer db2.Close()
	resumed, err := workbench.NewEngine(region, workbench.RuntimeConfig{Serial: true}, db2)
	if err != nil {
		t.Fatalf("resumed engine: %v", err)
	}
	run, runErr := resumed.Run(context.Background())
	if runErr != nil {
		t.Fatalf("resumed run: %v", runErr)
	}
	if run.Snapshot.Invalidated {
		t.Fatal("missing checkpoint incorrectly invalidated the whole snapshot")
	}
	if len(run.Recomputed) != 1 || run.Recomputed[0] != "zone-2-northeast" {
		t.Fatalf("resumed recomputed %v, want only the missing zone", run.Recomputed)
	}
	firstJSON, _ := json.Marshal(first.Points)
	runJSON, _ := json.Marshal(run.Points)
	if string(firstJSON) != string(runJSON) {
		t.Fatal("partial resume changed the point set")
	}
}

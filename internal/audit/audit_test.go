package audit_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"hdrproof/internal/apperr"
	"hdrproof/internal/audit"
	"hdrproof/internal/engine"
	"hdrproof/internal/model"
	"hdrproof/internal/sample"
)

func fullDB(t *testing.T, dir string) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "audit.db")
	if _, err := engine.Execute(context.Background(), engine.Config{
		DatasetDir: dir, DBPath: db, Mode: engine.ModeFull,
		Params: model.DefaultParams(), MaxWorkers: 4, FrameBufferPerWorker: 2,
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestIndependentAuditPassesAndDetectsMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(sample.GenerateOptions{Dir: dir, Seed: 42}); err != nil {
		t.Fatal(err)
	}
	dbPath := fullDB(t, dir)
	result, err := audit.Audit(context.Background(), dir, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed || result.FramesAudited != 95 || result.RecordsAudited != 95 {
		t.Fatalf("unexpected audit result: %+v", result)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM frames WHERE route_id='arena-linear-bt2020' AND frame_index=0`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = audit.Audit(context.Background(), dir, dbPath)
	if err == nil || apperr.CodeOf(err) != apperr.CodeAudit {
		t.Fatalf("expected E_AUDIT, got %v", err)
	}
	info, _ := apperr.As(err)
	if info.Field != "records.missing" || info.RouteID == "" || info.Frame < 0 {
		t.Fatalf("wrong missing record context: %+v", info)
	}
}

func TestAuditDetectsDuplicateRecords(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(sample.GenerateOptions{Dir: dir, Seed: 42}); err != nil {
		t.Fatal(err)
	}
	dbPath := fullDB(t, dir)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`ALTER TABLE frames RENAME TO frames_old`,
		`CREATE TABLE frames (
			route_id TEXT NOT NULL, frame_index INTEGER NOT NULL, segment_index INTEGER NOT NULL,
			timestamp_ms INTEGER NOT NULL, width INTEGER NOT NULL, height INTEGER NOT NULL,
			channels INTEGER NOT NULL, pixels BLOB NOT NULL, pixel_hash TEXT NOT NULL,
			proof_json BLOB NOT NULL, record_hash TEXT NOT NULL, param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL, truth_fingerprint TEXT NOT NULL)`,
		`INSERT INTO frames SELECT * FROM frames_old`,
		`INSERT INTO frames SELECT * FROM frames_old WHERE route_id='arena-linear-bt2020' AND frame_index=0`,
		`DROP TABLE frames_old`,
	}
	for _, stmt := range statements {
		if _, err := tx.Exec(stmt); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = audit.Audit(context.Background(), dir, dbPath)
	if err == nil || apperr.CodeOf(err) != apperr.CodeAudit {
		t.Fatalf("expected E_AUDIT, got %v", err)
	}
	info, _ := apperr.As(err)
	if info.Field != "records.duplicate" {
		t.Fatalf("expected duplicate violation, got %+v", info)
	}
}

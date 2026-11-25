package acousticfield

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestIndependentVerifier(t *testing.T) {
	sample, err := Generate(505)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	dbPath := filepath.Join(t.TempDir(), "verify.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := NewEngine(sample.Level, &sample.Truth, opts, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err := engine.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifySnapshot(context.Background(), store, engine.SnapshotID(), sample.Truth, opts)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if !report.OK || !report.Conservation.OK {
		t.Fatalf("verification not OK: %+v", report)
	}
	if report.Chunks != len(sample.Level.Chunks) {
		t.Fatalf("verified %d chunks", report.Chunks)
	}
	if len(report.CommitSequence) != len(run.Results) {
		t.Fatalf("commit sequence length %d", len(report.CommitSequence))
	}
	if err := CompareRunReports(run, run); err != nil {
		t.Fatal(err)
	}

	// Mutating one persisted energy must produce E_DIFF with field context.
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE chunk_results SET result_json=json_set(result_json, '$.arrival_energy[0].energy', 1) WHERE chunk_id=3 AND snapshot_id=?`,
		engine.SnapshotID()); err != nil {
		t.Fatal(err)
	}
	_, err = VerifySnapshot(context.Background(), store, engine.SnapshotID(), sample.Truth, opts)
	if !IsCode(err, CodeDiff) {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Diff == nil || fe.Diff.Field == "" {
		t.Fatalf("diff context missing: %v", err)
	}
}

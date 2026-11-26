package vpf

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	_ "vpfstack/internal/sqlite"
)

func sqliteDriverAvailable() bool {
	for _, name := range sql.Drivers() {
		if name == "vpf_sqlite" {
			return true
		}
	}
	return false
}

func TestSQLiteCheckpointThreePaths(t *testing.T) {
	if !sqliteDriverAvailable() {
		t.Skip("vpf_sqlite bridge unavailable; SQLStore remains driver-agnostic")
	}
	ctx := context.Background()
	bundle := testBundle(t)
	dir := t.TempDir()
	var dbCounter int32
	report, err := VerifyThreePaths(ctx, bundle.Loaded[SchemaV3], 0, func() CheckpointStore {
		dsn := filepath.Join(dir, fmt.Sprintf("path-%d.sqlite", atomic.AddInt32(&dbCounter, 1)))
		db, openErr := sql.Open("vpf_sqlite", dsn)
		if openErr != nil {
			t.Fatalf("open sqlite: %v", openErr)
		}
		return NewSQLStore(db)
	})
	if err != nil {
		t.Fatalf("sqlite three paths: %v %+v", err, report)
	}
	if !report.PixelIdentical || !report.SequenceIdentical {
		t.Fatalf("sqlite report mismatch: %+v", report)
	}
}

func TestSQLiteIncrementalOpacity(t *testing.T) {
	if !sqliteDriverAvailable() {
		t.Skip("vpf_sqlite bridge unavailable")
	}
	ctx := context.Background()
	p := testBundle(t).Loaded[SchemaV3]
	db, err := sql.Open("vpf_sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	report, err := VerifyIncrementalEdit(ctx, p, 0, store, func(edited *Project) (Change, error) {
		return SetLayerOpacity(edited, "slate_badge", 3500)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Plan.InvalidChunks) != 1 || report.Plan.InvalidChunks[0] != 0 || !report.UnaffectedStable {
		t.Fatalf("sqlite incremental report invalid: %+v", report.Plan)
	}
}

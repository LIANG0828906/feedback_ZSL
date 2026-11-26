package sqlitefile_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"wardrobekernel/internal/atomicfile"
	"wardrobekernel/internal/sqlitefile"
)

func TestKVDBRoundTripAndMagic(t *testing.T) {
	rows := []sqlitefile.Row{
		{Key: "meta.algorithm_version", Value: "test-v1"},
		{Key: "batch:000001", Value: `{"batch_number":1,"part_ids":["a","b"]}`},
		{Key: "batch:000002", Value: `{"batch_number":2,"unicode":"材质槽"}`},
	}
	db, err := sqlitefile.BuildKVDB(rows)
	if err != nil {
		t.Fatal(err)
	}
	if string(db[:16]) != "SQLite format 3\x00" {
		t.Fatalf("bad SQLite magic: %q", db[:16])
	}
	path := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	if err := atomicfile.WriteBytes(path, db); err != nil {
		t.Fatal(err)
	}
	got, err := sqlitefile.ReadKVFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(rows) {
		t.Fatalf("rows=%d want %d", len(got), len(rows))
	}
	for i := range rows {
		if got[i] != rows[i] {
			t.Fatalf("row %d mismatch: got %#v want %#v", i, got[i], rows[i])
		}
	}
}

func TestKVDBMultiLeafRoundTrip(t *testing.T) {
	rows := make([]sqlitefile.Row, 0, 160)
	for i := 0; i < 160; i++ {
		rows = append(rows, sqlitefile.Row{
			Key:   fmt.Sprintf("batch:%06d", i+1),
			Value: fmt.Sprintf(`{"n":%d,"padding":%q}`, i+1, strings.Repeat("x", 700)),
		})
	}
	db, err := sqlitefile.BuildKVDB(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(db) <= 2*sqlitefile.PageSize {
		t.Fatalf("expected multipage database, size=%d", len(db))
	}
	path := filepath.Join(t.TempDir(), "multi.sqlite")
	if err := atomicfile.WriteBytes(path, db); err != nil {
		t.Fatal(err)
	}
	got, err := sqlitefile.ReadKVFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(rows) {
		t.Fatalf("rows=%d want %d", len(got), len(rows))
	}
	if got[7].Key != rows[7].Key || got[159].Value != rows[159].Value {
		t.Fatal("multipage row content/order mismatch")
	}
}

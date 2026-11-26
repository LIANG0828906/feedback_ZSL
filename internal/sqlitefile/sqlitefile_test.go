package sqlitefile_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"layerforge/internal/sqlitefile"
)

func TestRoundTripSQLiteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	db := sqlitefile.Database{Tables: []sqlitefile.Table{
		{Name: "meta", Columns: []string{"key", "value"}, Rows: [][]sqlitefile.Value{{"version", "v1"}, {"count", int64(2)}}},
		{Name: "items", Columns: []string{"id", "name"}, Rows: [][]sqlitefile.Value{{int64(1), "alpha"}, {int64(2), "beta"}}},
	}}
	if err := sqlitefile.WriteAtomic(path, db, nil); err != nil {
		t.Fatal(err)
	}
	got, err := sqlitefile.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tables) != 2 {
		t.Fatalf("tables = %d", len(got.Tables))
	}
	items := tableByName(got, "items")
	want := [][]sqlitefile.Value{{int64(1), "alpha"}, {int64(2), "beta"}}
	if !reflect.DeepEqual(items.Rows, want) {
		t.Fatalf("rows = %#v, want %#v", items.Rows, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 3*65536 {
		t.Fatalf("file size = %d", info.Size())
	}
}

func TestTruncatedFileIsDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.db")
	db := sqlitefile.Database{Tables: []sqlitefile.Table{
		{Name: "meta", Columns: []string{"key", "value"}, Rows: [][]sqlitefile.Value{{"version", "v1"}}},
	}}
	if err := sqlitefile.WriteAtomic(path, db, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlitefile.Read(path); err == nil {
		t.Fatal("expected truncation error")
	}
}

func tableByName(db *sqlitefile.Database, name string) sqlitefile.Table {
	for _, table := range db.Tables {
		if table.Name == name {
			return table
		}
	}
	return sqlitefile.Table{}
}

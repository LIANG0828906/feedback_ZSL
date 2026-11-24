package compare_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"hdrproof/internal/apperr"
	"hdrproof/internal/compare"
	"hdrproof/internal/engine"
	"hdrproof/internal/model"
	"hdrproof/internal/sample"
)

func makeDB(t *testing.T, dir, name string) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), name)
	if _, err := engine.Execute(context.Background(), engine.Config{
		DatasetDir: dir, DBPath: db, Mode: engine.ModeFull,
		Params: model.DefaultParams(), MaxWorkers: 2, FrameBufferPerWorker: 2,
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestEqualAndFirstDifference(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(sample.GenerateOptions{Dir: dir, Seed: 42}); err != nil {
		t.Fatal(err)
	}
	left := makeDB(t, dir, "left.db")
	right := makeDB(t, dir, "right.db")
	equal, err := compare.Snapshots(context.Background(), left, right)
	if err != nil || !equal.Equal {
		t.Fatalf("expected equal snapshots: %+v %v", equal, err)
	}

	db, err := sql.Open("sqlite", right)
	if err != nil {
		t.Fatal(err)
	}
	var pixels []byte
	if err := db.QueryRow(`SELECT pixels FROM frames WHERE route_id='arena-linear-bt2020' AND frame_index=0`).Scan(&pixels); err != nil {
		t.Fatal(err)
	}
	pixels[0] ^= 0x7f
	if _, err := db.Exec(`UPDATE frames SET pixels=? WHERE route_id='arena-linear-bt2020' AND frame_index=0`, pixels); err != nil {
		t.Fatal(err)
	}
	db.Close()
	result, err := compare.Snapshots(context.Background(), left, right)
	if err == nil || apperr.CodeOf(err) != apperr.CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	info, _ := apperr.As(err)
	if info.RouteID != "arena-linear-bt2020" || info.Frame != 0 || !strings.HasPrefix(info.Field, "pixels[") {
		t.Fatalf("wrong first difference: %+v result=%+v", info, result)
	}
}

func TestProofFieldDifferenceIsLocated(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(sample.GenerateOptions{Dir: dir, Seed: 42}); err != nil {
		t.Fatal(err)
	}
	left := makeDB(t, dir, "left.db")
	right := makeDB(t, dir, "right.db")
	db, err := sql.Open("sqlite", right)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE frames SET proof_json='{"record_version":"tampered"}' WHERE route_id='arena-linear-bt2020' AND frame_index=1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = compare.Snapshots(context.Background(), left, right)
	if err == nil || apperr.CodeOf(err) != apperr.CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	info, _ := apperr.As(err)
	if info.RouteID != "arena-linear-bt2020" || info.Frame != 1 || !strings.HasPrefix(info.Field, "proof.") {
		t.Fatalf("wrong proof difference: %+v", info)
	}
}

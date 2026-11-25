package verifier_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"iconcut"
	"iconcut/verifier"

	_ "modernc.org/sqlite"
)

func TestVerifiesGeneratedCheckpoint(t *testing.T) {
	dir := t.TempDir()
	_, sample, err := iconcut.GenerateBundle(dir, iconcut.DefaultSpecs()[1])
	if err != nil {
		t.Fatal(err)
	}
	b, err := iconcut.LoadBundle(filepath.Join(dir, sample.Manifest.ImageID))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")
	if _, err := iconcut.RunBundle(context.Background(), b, iconcut.RunOptions{DBPath: dbPath}); err != nil {
		t.Fatal(err)
	}
	rep, err := verifier.VerifyDB(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("verifier rejected checkpoint: %+v", rep.FirstViolation)
	}
	if rep.CommitsChecked != 48 || rep.TilesChecked != 12 || rep.RegionsComputed != 4 {
		t.Fatalf("unexpected counts: %+v", rep)
	}

	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sqlDB.Exec(`UPDATE commits SET content_hash='deadbeef' WHERE image_id=? AND tile_index=4 AND stage='mapping'`, b.Manifest.ImageID)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.Close()

	bad, err := verifier.VerifyDB(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bad.OK || bad.FirstViolation == nil {
		t.Fatal("verifier accepted corrupted hash")
	}
	v := bad.FirstViolation
	if v.TileIndex != 4 || v.Stage != iconcut.StageMapping || v.Field != "content_hash" {
		t.Fatalf("unexpected first violation: %+v", v)
	}
}

package checkpoint_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"terrain-occlusion/internal/checkpoint"
	"terrain-occlusion/internal/model"

	_ "modernc.org/sqlite"
)

func TestCheckpointHashTamperVoidsSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta("project_hash", "proj-1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, err := store.Commit(checkpoint.Record{
			SampleIndex: i, LogicalTimeMs: int64(i * 100), VisibleDigest: "d",
			UncertainJSON: "[]", ResidentJSON: "[]", ShadowVersion: "v1",
			CameraOffset: i + 1, ResultJSON: "{}", EngineJSON: "{}",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.LoadVerified("proj-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE checkpoints SET resident_json='[999]' WHERE sample_index=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	records, err := store2.LoadVerified("proj-1")
	assertHashError(t, err)
	if records != nil {
		t.Fatal("tampered checkpoint must not return any snapshot records")
	}
}

func TestCheckpointVersionMismatchVoidsSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta("project_hash", "proj-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(checkpoint.Record{SampleIndex: 0, VisibleDigest: "d", UncertainJSON: "[]", ResidentJSON: "[]", ShadowVersion: "v1", ResultJSON: "{}", EngineJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE meta SET value='checkpoint-v0' WHERE key='schema_version'`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	store2, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	_, err = store2.LoadVerified("proj-1")
	assertHashError(t, err)
}

func assertHashError(t *testing.T, err error) {
	t.Helper()
	var te *model.Error
	if !errors.As(err, &te) || te.Code != model.EHash {
		t.Fatalf("want E_HASH, got %v", err)
	}
}

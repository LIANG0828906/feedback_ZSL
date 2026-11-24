package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"audioforge/internal/engine"
	"audioforge/internal/errs"
	"audioforge/internal/generator"
	"audioforge/internal/model"
	"audioforge/internal/store"
)

func TestWindowCommitLoadAndHashInvalidation(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p-store", AlgorithmVersion: "a-store", CommitWindow: 3, Workers: 2, BufferPerWorker: 2}
	built, err := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Begin("job", params.ParamVersion, params.AlgorithmVersion, bundle.Capability.Version); err != nil {
		t.Fatal(err)
	}
	for _, commit := range built.Commits {
		if err := db.CommitWindow("job", commit.WindowIndex, commit.Packets); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Finalize("job"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := db.Load("job", params.ParamVersion, params.AlgorithmVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Complete || len(snapshot.Windows) != len(built.Commits) {
		t.Fatalf("unexpected snapshot: complete=%v windows=%d", snapshot.Complete, len(snapshot.Windows))
	}

	_, err = db.DB().Exec(`UPDATE packet_records SET samples = X'0000000000000001' WHERE job_id='job' AND channel=0 AND seq=0`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Load("job", params.ParamVersion, params.AlgorithmVersion)
	if !errs.IsCode(err, errs.ESnapshot) {
		t.Fatalf("expected E_SNAPSHOT, got %v", err)
	}
	if db.Exists("job") {
		t.Fatal("invalidated snapshot still exists")
	}
}

func TestVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p1", AlgorithmVersion: "a1", CommitWindow: 2, Workers: 1, BufferPerWorker: 1}
	built, _ := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	db, err := store.Open(filepath.Join(t.TempDir(), "version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Begin("job", params.ParamVersion, params.AlgorithmVersion, bundle.Capability.Version); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitWindow("job", 0, built.Commits[0].Packets); err != nil {
		t.Fatal(err)
	}
	_, err = db.Load("job", "p2", params.AlgorithmVersion)
	if !errs.IsCode(err, errs.ESnapshot) {
		t.Fatalf("expected E_SNAPSHOT, got %v", err)
	}
	if db.Exists("job") {
		t.Fatal("snapshot should have been invalidated")
	}
}

func TestIllegalWindowOrderIsAtomic(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 2, Workers: 1, BufferPerWorker: 1}
	built, _ := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	db, err := store.Open(filepath.Join(t.TempDir(), "order.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Begin("job", params.ParamVersion, params.AlgorithmVersion, bundle.Capability.Version); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitWindow("job", 0, built.Commits[0].Packets); err != nil {
		t.Fatal(err)
	}
	err = db.CommitWindow("job", 2, built.Commits[1].Packets)
	if !errs.IsCode(err, errs.EState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	var count int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE job_id='job'`).Scan(&count); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("window count = %d, want 1", count)
	}
}

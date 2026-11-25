package store

import (
	"path/filepath"
	"testing"

	"edgerefine/internal/model"
	"edgerefine/internal/state"
)

func testRecord(algo string) StageRecord {
	return StageRecord{
		SampleID: "sample", Tile: model.TileCoord{TX: 1, TY: 2},
		Stage: state.StageRefine, Status: state.StatusCommitted, Summary: "test",
		Convergence: model.TileConvergence{Converged: true},
		Output:      map[string]any{"mask": []byte{1, 2, 3}}, ParamVersion: 1,
		IterBudget: 5, AlgorithmVersion: algo, TruthFingerprint: "fp",
	}
}

func TestSnapshotHashAndVersionValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitStage(testRecord(model.AlgorithmVersion)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTruthFingerprint("sample", "fp"); err != nil {
		t.Fatal(err)
	}
	rows, valid, err := s.LoadValidStages("sample", "fp")
	if err != nil || !valid || len(rows) != 1 {
		t.Fatalf("expected valid snapshot, valid=%v rows=%d err=%v", valid, len(rows), err)
	}
	if _, err := s.db.Exec(`UPDATE stage_checkpoints SET content_hash='tampered' WHERE sample_id='sample'`); err != nil {
		t.Fatal(err)
	}
	_, valid, err = s.LoadValidStages("sample", "fp")
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("tampered hash must invalidate snapshot")
	}
	rows, _ = s.LoadStages("sample")
	if len(rows) != 0 {
		t.Fatal("invalid snapshot must be wholly discarded")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(filepath.Join(t.TempDir(), "version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.CommitStage(testRecord("old-version")); err != nil {
		t.Fatal(err)
	}
	_, valid, err = s2.LoadValidStages("sample", "fp")
	if err != nil || valid {
		t.Fatalf("algorithm version mismatch must invalidate, valid=%v err=%v", valid, err)
	}
}

func TestSerializedConcurrentCommits(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "serial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan error, 12)
	for i := 0; i < 12; i++ {
		go func(i int) {
			r := testRecord(model.AlgorithmVersion)
			r.Tile = model.TileCoord{TX: i, TY: 0}
			done <- s.CommitStage(r)
		}(i)
	}
	for i := 0; i < 12; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	seq, err := s.CommitSequence("sample")
	if err != nil || len(seq) != 12 {
		t.Fatalf("expected 12 ordered commits, seq=%d err=%v", len(seq), err)
	}
}

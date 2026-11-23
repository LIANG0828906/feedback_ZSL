package checkpoint_test

import (
	"os"
	"path/filepath"
	"testing"

	"captionx/internal/checkpoint"
	"captionx/internal/model"
)

func record(idx int) checkpoint.ChunkRecord {
	return checkpoint.ChunkRecord{
		ChunkIdx: idx,
		Captions: []model.CaptionResult{{
			CaptionID:  idx + 1,
			Window:     model.Window{Start: int64(idx * 100), End: int64(idx*100 + 50)},
			CharStarts: []int64{1, 2},
			CharEnds:   []int64{2, 3},
		}},
		ErrorTerms: []model.CaptionErrorTerms{{CaptionID: idx + 1, WindowDeviation: 3}},
	}
}

func TestCommitAndLoad(t *testing.T) {
	dir := t.TempDir()
	s, err := checkpoint.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash := "hash-A"
	var snap checkpoint.Snapshot
	for i := 0; i < 3; i++ {
		snap.ContentHash = hash
		if err := s.Commit(record(i), snap); err != nil {
			t.Fatal(err)
		}
		snap.Committed = append(snap.Committed, i)
	}
	got, recs, err := s.Load(hash)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Committed) != 3 || len(recs) != 3 {
		t.Fatalf("snapshot = %+v, recs = %d", got, len(recs))
	}
	if recs[2].Captions[0].Window.End != 250 {
		t.Fatalf("record content mismatch: %+v", recs[2].Captions[0].Window)
	}
	if got.TimeUnitVersion != checkpoint.TimeUnitVersion || got.AlgoVersion != checkpoint.AlgoVersion {
		t.Fatalf("versions not stamped: %+v", got)
	}
}

func TestHashMismatchDiscardsWholeSnapshot(t *testing.T) {
	dir := t.TempDir()
	s, _ := checkpoint.NewStore(dir)
	_ = s.Commit(record(0), checkpoint.Snapshot{ContentHash: "hash-A", Committed: []int{0}})
	snap, recs, err := s.Load("hash-B")
	if err != nil {
		t.Fatal(err)
	}
	if snap != nil || recs != nil {
		t.Fatalf("mismatched hash must discard snapshot, got %+v", snap)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("snapshot files must be removed, %d remain", len(entries))
	}
}

func TestCorruptSnapshotDiscarded(t *testing.T) {
	dir := t.TempDir()
	s, _ := checkpoint.NewStore(dir)
	_ = s.Commit(record(0), checkpoint.Snapshot{ContentHash: "h", Committed: []int{0}})
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, recs, err := s.Load("h")
	if err != nil || snap != nil || recs != nil {
		t.Fatalf("corrupt snapshot must be discarded: %v %+v", err, snap)
	}
}

func TestMissingChunkRecordDiscards(t *testing.T) {
	dir := t.TempDir()
	s, _ := checkpoint.NewStore(dir)
	_ = s.Commit(record(0), checkpoint.Snapshot{ContentHash: "h", Committed: []int{0}})
	_ = s.Commit(record(1), checkpoint.Snapshot{ContentHash: "h", Committed: []int{0, 1}})
	_ = os.Remove(filepath.Join(dir, "chunk-0001.json"))
	snap, _, err := s.Load("h")
	if err != nil || snap != nil {
		t.Fatalf("missing record must discard whole snapshot: %v %+v", err, snap)
	}
}

func TestNoSnapshot(t *testing.T) {
	s, _ := checkpoint.NewStore(t.TempDir())
	snap, recs, err := s.Load("anything")
	if err != nil || snap != nil || recs != nil {
		t.Fatalf("empty store must yield nil snapshot: %v", err)
	}
}

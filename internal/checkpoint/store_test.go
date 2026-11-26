package checkpoint

import (
	"testing"

	"acousticfield/internal/acoustic"
)

func TestCommitRecoverAndTornTail(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	result := acoustic.ChunkResult{ChunkID: "C00", AlgorithmVersion: acoustic.AlgorithmVersion, ParamVersion: "p1", CacheKey: "k1"}
	if _, err := store.CommitChunk("r1", 1, 1, result); err != nil {
		t.Fatal(err)
	}
	result.ChunkID = "C01"
	if _, err := store.CommitChunk("r1", 2, 2, result); err != nil {
		t.Fatal(err)
	}
	if err := store.CorruptTailForTest(); err != nil {
		t.Fatal(err)
	}
	records, err := store.ReadCommitted()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("expected two valid records after torn tail, got %d", len(records))
	}
	latest, _, err := store.LatestMatching(acoustic.AlgorithmVersion, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest = %d", len(latest))
	}
}

func TestVersionMismatchNotRecovered(t *testing.T) {
	dir := t.TempDir()
	store, _ := Open(dir)
	result := acoustic.ChunkResult{ChunkID: "C00", AlgorithmVersion: "old", ParamVersion: "old-param"}
	if _, err := store.CommitChunk("old", 1, 1, result); err != nil {
		t.Fatal(err)
	}
	latest, _, err := store.LatestMatching(acoustic.AlgorithmVersion, "current-param")
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 0 {
		t.Fatalf("version-mismatched chunk must not recover: %d", len(latest))
	}
}

func TestAtomicReportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, _ := Open(dir)
	want := &acoustic.Report{RoundID: "r", ChunkOrder: []string{"C00"}}
	if err := store.SaveReport(want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.RoundID != want.RoundID || len(got.ChunkOrder) != 1 {
		t.Fatalf("bad report: %+v", got)
	}
}

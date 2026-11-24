package store

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
)

func testManifest() Manifest {
	return Manifest{
		SourceHash:       "source",
		SceneHash:        "scene",
		ParamsVersion:    "params",
		AlgorithmVersion: "algo",
		MediumVersion:    "medium",
		GeometryVersion:  "geometry",
		LightVersions:    map[string]string{"light": "v1"},
		BatchSize:        4,
	}
}

func testRecord(i int) BlockRecord {
	return BlockRecord{
		CameraIndex:      0,
		CameraID:         "cam0",
		X:                i,
		Y:                0,
		Payload:          []byte("payload"),
		Digest:           "digest",
		ParamsVersion:    "params",
		AlgorithmVersion: "algo",
		MediumVersion:    "medium",
		GeometryVersion:  "geometry",
		LightVersions:    map[string]string{"light": "v1"},
	}
}

func TestConcurrentCommitsAreSerializedAndOrdered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	s, _, err := Open(path, testManifest())
	if err != nil {
		t.Fatal(err)
	}
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.CommitBatch([]BlockRecord{testRecord(i)}, testManifest(), false); err != nil {
				t.Errorf("commit %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT batch_no FROM batches ORDER BY batch_no`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var no int
		if err := rows.Scan(&no); err != nil {
			t.Fatal(err)
		}
		got = append(got, no)
	}
	if len(got) != n {
		t.Fatalf("batch count=%d want %d", len(got), n)
	}
	for i, no := range got {
		if no != i+1 {
			t.Fatalf("batch order not stable: %v", got)
		}
	}
}

func TestReplacingBlockArchivesOldValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	s, _, err := Open(path, testManifest())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CommitBatch([]BlockRecord{testRecord(0)}, testManifest(), false); err != nil {
		t.Fatal(err)
	}
	replacement := testRecord(0)
	replacement.Digest = "digest2"
	if _, err := s.CommitBatch([]BlockRecord{replacement}, testManifest(), false); err != nil {
		t.Fatal(err)
	}
	counts, err := s.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Blocks != 1 || counts.Batches != 2 || counts.History != 1 {
		t.Fatalf("unexpected counts: %+v", counts)
	}
}

func TestBlockVersionMismatchWipesWholeSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	m := testManifest()
	s, _, err := Open(path, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitBatch([]BlockRecord{testRecord(0)}, m, true); err != nil {
		t.Fatal(err)
	}
	newer := m
	newer.LightVersions = map[string]string{"light": "v2"}
	if err := s.SetComplete(newer, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, restore, err := Open(path, newer)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if !restore.Invalidated || len(restore.Blocks) != 0 {
		t.Fatalf("expected whole-snapshot invalidation, got %+v", restore)
	}
	counts, err := s2.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Blocks != 0 || counts.Batches != 0 {
		t.Fatalf("stale blocks survived invalidation: %+v", counts)
	}
}

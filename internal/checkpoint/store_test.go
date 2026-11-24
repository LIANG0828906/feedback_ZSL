package checkpoint_test

import (
	"path/filepath"
	"testing"

	"integritykernel/internal/checkpoint"
	"integritykernel/internal/core"
)

func TestBatchSnapshotVersionMismatchResets(t *testing.T) {
	store, err := checkpoint.Open(filepathJoinTemp(t, "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	payload := []byte(`{"ok":true}`)
	item := checkpoint.Item{
		Frame: 0, Type: core.EvidenceVerdict, CacheKey: "key-1",
		ContentHash: "content", ParamHash: "param", AlgorithmVersion: core.AlgorithmVersion,
		Digest: core.SHA256Hex(payload), PayloadJSON: payload,
	}
	if err := store.BeginBatch(1, "evidence", 0, 0, "param-v1", core.AlgorithmVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitBatch(1, "evidence", []checkpoint.Item{item}, "root", "param-v1", core.AlgorithmVersion, `{}`, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := store.SnapshotMatches("root", "param-v1", core.AlgorithmVersion); err != nil || !ok {
		t.Fatalf("expected matching snapshot, ok=%v err=%v", ok, err)
	}
	stored, err := store.LoadCurrent("root", "param-changed", core.AlgorithmVersion, map[int]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("version mismatch must invalidate all evidence, got %d", len(stored))
	}
}

func TestFirstUnfinishedBatch(t *testing.T) {
	store, err := checkpoint.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.BeginBatch(7, "evidence", 10, 12, "p", core.AlgorithmVersion); err != nil {
		t.Fatal(err)
	}
	seq, found, err := store.FirstUnfinishedBatch()
	if err != nil || !found || seq != 7 {
		t.Fatalf("expected batch 7, got seq=%d found=%v err=%v", seq, found, err)
	}
}

func TestLoadCurrentContentMismatchDoesNotMixEvidence(t *testing.T) {
	store, err := checkpoint.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	payload := []byte(`{"frame":0}`)
	item := checkpoint.Item{
		Frame: 0, Type: core.EvidenceNoise, CacheKey: "noise-key",
		ContentHash: "old-content", AlgorithmVersion: core.AlgorithmVersion,
		Digest: core.SHA256Hex(payload), PayloadJSON: payload,
	}
	if err := store.CommitBatch(1, "evidence", []checkpoint.Item{item}, "root", "p", core.AlgorithmVersion, `{}`, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := store.LoadCurrent("root", "p", core.AlgorithmVersion, map[int]string{0: "new-content"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("content mismatch must discard all rows, got %d", len(stored))
	}
}

func filepathJoinTemp(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

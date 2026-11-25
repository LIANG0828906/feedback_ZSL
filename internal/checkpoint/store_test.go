package checkpoint_test

import (
	"path/filepath"
	"testing"

	"presentkernel/internal/checkpoint"
	"presentkernel/internal/domain"
	"presentkernel/internal/quant"
)

func TestCheckpointMismatchInvalidatesRunSnapshot(t *testing.T) {
	db := filepath.Join(t.TempDir(), "cp.sqlite")
	store, err := checkpoint.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	r := &domain.PartitionResult{
		DialogueID: "D0", PartitionID: "D0-P0", PartitionIndex: 0,
		CacheKey: "old-key", ContentHash: "old-content", Digest: "digest",
		SourceSchema: domain.SchemaV3, QuantParamVersion: quant.ParamVersion,
		AlgorithmVersion: domain.AlgorithmVersion,
	}
	if err := store.CommitPartition("run", 1, r); err != nil {
		t.Fatal(err)
	}
	valid, reason, rows, err := store.ValidateRun("run", map[string]string{"D0-P0": "new-key"})
	if err != nil {
		t.Fatal(err)
	}
	if valid || reason == "" || len(rows) != 1 {
		t.Fatalf("expected mismatch report, valid=%v reason=%q rows=%d", valid, reason, len(rows))
	}
	_, rows, err = store.LoadRun("run")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("run snapshot should be invalidated, rows=%d", len(rows))
	}
	if _, ok, err := store.GetCached("old-key"); err != nil || !ok {
		t.Fatalf("content-addressed cache should remain available: ok=%v err=%v", ok, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

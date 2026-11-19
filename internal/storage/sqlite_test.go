package storage

import (
	"context"
	"path/filepath"
	"testing"

	"settlement/internal/tasks"
)

func TestApplyIsAtomicAndIdempotent(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "settlement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	batch := tasks.Generate(tasks.DefaultSeed, 20, 100)[0]
	result, err := tasks.BuildResult(batch)
	if err != nil {
		t.Fatal(err)
	}
	record := CommittedResult{
		BatchID:         batch.ID,
		HolderID:        "A",
		TaskCount:       int32(result.TaskCount),
		TotalAmount:     result.TotalAmount,
		ResultDigest:    result.Digest,
		ResultJSON:      result.PayloadJSON,
		CommittedAtUnix: 1234,
	}
	ctx := context.Background()
	if err := store.Apply(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(ctx, record); err != nil {
		t.Fatalf("idempotent apply failed: %v", err)
	}
	summaries, err := store.Summaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].BatchID != batch.ID {
		t.Fatalf("unexpected summaries: %+v", summaries)
	}
	count, err := store.TaskCount(ctx, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 100 {
		t.Fatalf("task rows = %d, want 100", count)
	}
}

func TestApplyRejectsDigestMismatchWithoutRows(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "settlement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	batch := tasks.Generate(tasks.DefaultSeed, 20, 100)[0]
	result, err := tasks.BuildResult(batch)
	if err != nil {
		t.Fatal(err)
	}
	record := CommittedResult{
		BatchID:         batch.ID,
		HolderID:        "A",
		TaskCount:       int32(result.TaskCount),
		TotalAmount:     result.TotalAmount,
		ResultDigest:    "wrong",
		ResultJSON:      result.PayloadJSON,
		CommittedAtUnix: 1234,
	}
	ctx := context.Background()
	if err := store.Apply(ctx, record); err == nil {
		t.Fatal("expected digest mismatch")
	}
	summaries, err := store.Summaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("partial result rows were written: %+v", summaries)
	}
}

package checkpoint_test

import (
	"context"
	"path/filepath"
	"testing"

	"skeletonretarget/internal/checkpoint"
	"skeletonretarget/internal/model"
)

func TestCommitResumeAndCorruptSuffixInvalidation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	meta := checkpoint.Meta{ActionHash: "action", AlgorithmVersion: "v1"}
	if err := store.Begin(ctx, meta); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, err := store.CommitInterval(ctx, record(i, "v1", "p1"))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CommitInterval(ctx, record(2, "v1", "p1")); !model.IsCode(err, model.CodeState) {
		t.Fatalf("expected stable order rejection, got %v", err)
	}
	if err := store.CorruptPayload(ctx, 1); err != nil {
		t.Fatal(err)
	}
	resume, err := store.Resume(ctx, meta, func(record checkpoint.IntervalRecord) bool {
		return record.ParamVersion == "p1"
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resume.Invalidated || resume.InvalidatedFrom != 1 {
		t.Fatalf("expected invalidation from interval 1, got %+v", resume)
	}
	if len(resume.Records) != 1 || resume.Records[0].IntervalID != 0 {
		t.Fatalf("expected consistent prefix of one interval, got %+v", resume.Records)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.Resume(ctx, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Records) != 1 || again.Records[0].IntervalID != 0 {
		t.Fatalf("expected only committed prefix after restart, got %+v", again.Records)
	}
}

func TestVersionMismatchInvalidatesFromFirstMismatch(t *testing.T) {
	ctx := context.Background()
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta := checkpoint.Meta{ActionHash: "action", AlgorithmVersion: "v1"}
	if err := store.Begin(ctx, meta); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.CommitInterval(ctx, record(i, "v1", "p1")); err != nil {
			t.Fatal(err)
		}
	}
	resume, err := store.Resume(ctx, meta, func(record checkpoint.IntervalRecord) bool {
		return record.IntervalID < 2
	})
	if err != nil {
		t.Fatal(err)
	}
	if resume.InvalidatedFrom != 2 || len(resume.Records) != 2 {
		t.Fatalf("unexpected version invalidation: %+v", resume)
	}
}

func record(id int, algorithm, param string) checkpoint.IntervalRecord {
	return checkpoint.IntervalRecord{
		IntervalID: id, FrameStart: id * 4, FrameEnd: id*4 + 3, State: "committed",
		Payload: []byte(`{"frame":` + string(rune('0'+id)) + `}`), Summary: []byte(`{}`),
		Dependencies: []string{"upstream:test"}, DependencyHash: "dep", CacheKey: "key-" + string(rune('0'+id)),
		TopologyVersion: "topo", ParamVersion: param, AlgorithmVersion: algorithm, StateProgress: id*4 + 3,
	}
}

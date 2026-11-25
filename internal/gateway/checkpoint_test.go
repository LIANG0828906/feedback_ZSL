package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteCheckpointResumeVersionAndTruncation(t *testing.T) {
	bundle := testBundle(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "checkpoint.sqlite")

	cp, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := cp.SaveWindowCommit(ctx, bundle.Canonical, bundle.Truth.Windows)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("first commit seq = %d, want 1", seq)
	}
	for _, batch := range bundle.Truth.Batches[:2] {
		if err := cp.SaveExportBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	state, err := cp.LoadState(ctx, bundle.Canonical.ProjectVersion)
	if err != nil {
		t.Fatal(err)
	}
	if state.GlobalCommitSeq != 1 || state.LastExportBatchNo != 2 {
		t.Fatalf("checkpoint seq = %d/%d", state.GlobalCommitSeq, state.LastExportBatchNo)
	}
	assertAggregatesMatch(t, bundle.Truth.Windows, state.Windows, 0)
	if len(state.Batches) != 2 {
		t.Fatalf("saved batches = %d, want 2", len(state.Batches))
	}
	_, err = cp.LoadState(ctx, "seed-wrong-version")
	ge := assertErrorCode(t, err, CodeCheckpointVersion)
	if ge.Context["found"] != nil {
		t.Fatalf("unexpected context key found: %+v", ge.Context)
	}
	if ge.Context["checkpoint_version"] != bundle.Canonical.ProjectVersion {
		t.Fatalf("version context missing: %+v", ge.Context)
	}
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}

	resume, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err = resume.LoadState(ctx, bundle.Canonical.ProjectVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, batch := range bundle.Truth.Batches {
		if batch.BatchNo > state.LastExportBatchNo {
			if err := resume.SaveExportBatch(ctx, batch); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := resume.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	finalState, err := reopened.LoadState(ctx, bundle.Canonical.ProjectVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalState.Batches) != len(bundle.Truth.Batches) {
		t.Fatalf("resumed batches = %d, want %d", len(finalState.Batches), len(bundle.Truth.Batches))
	}
	if !CompareBatches(bundle.Truth.Batches, finalState.Batches) {
		t.Fatal("resumed export sequence differs from one-shot export")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	truncatedPath := filepath.Join(t.TempDir(), "truncated.sqlite")
	if err := os.WriteFile(truncatedPath, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = OpenCheckpoint(truncatedPath)
	assertErrorCode(t, err, CodeCheckpointCorrupt)

	emptyPath := filepath.Join(t.TempDir(), "empty.sqlite")
	if err := os.WriteFile(emptyPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = OpenCheckpoint(emptyPath)
	assertErrorCode(t, err, CodeCheckpointCorrupt)
}

func TestStableValidationErrorMatrix(t *testing.T) {
	bundle := testBundle(t)
	tests := []struct {
		name   string
		code   string
		mutate func(p *Project)
	}{
		{"negative_bytes", CodeNegativeBytes, func(p *Project) { p.Records[0].RequestBytes = -1 }},
		{"zero_width", CodeWindowWidthInvalid, func(p *Project) { p.Window.WidthMS = 0 }},
		{"slide_too_large", CodeWindowSlideInvalid, func(p *Project) { p.Window.SlideMS = p.Window.WidthMS + 1 }},
		{"watermark_before_commit", CodeWatermarkBeforeCommit, func(p *Project) {
			old := *p.CommittedBoundaryMS - 1
			p.WatermarkMS = &old
		}},
		{"boundary_scale", CodeBucketScaleOutOfRange, func(p *Project) {
			p.HistogramBoundariesUS = []int64{1, 9_000_000_000_000_000_000}
		}},
		{"workers_budget", CodeBudgetWorkersInvalid, func(p *Project) { p.Budget.Workers = 0 }},
		{"buffer_budget", CodeBudgetBufferInvalid, func(p *Project) { p.Budget.ServiceBuffer = -1 }},
		{"workers_over_cap", CodeBudgetWorkersInvalid, func(p *Project) { p.Budget.Workers = p.Budget.MaxWorkers + 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := bundle.Canonical.Clone()
			tc.mutate(p)
			err := p.Validate()
			assertErrorCode(t, err, tc.code)
		})
	}

	_, err := LoadSchema([]byte(`{"schema":"v999"}`))
	assertErrorCode(t, err, CodeSchemaUnsupported)
}

func TestCheckpointWindowSetIsAtomicallyReplaced(t *testing.T) {
	bundle := testBundle(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "replace.sqlite")
	cp, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	stale := &Aggregate{
		Key:          WindowKey{Index: 9_999_999, GroupKey: GroupKey{Level: "service", ServiceID: "stale"}},
		BucketCounts: []int64{1}, RecordCount: 1, Committed: true,
	}
	if _, err := cp.SaveWindowCommit(ctx, bundle.Canonical, []*Aggregate{stale}); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.SaveWindowCommit(ctx, bundle.Canonical, bundle.Truth.Windows); err != nil {
		t.Fatal(err)
	}
	state, err := cp.LoadState(ctx, bundle.Canonical.ProjectVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Windows) != len(bundle.Truth.Windows) {
		t.Fatalf("stale window survived: got %d windows, want %d", len(state.Windows), len(bundle.Truth.Windows))
	}
	_ = cp.Close()
}

package enginelab_test

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	"enginelab.local/internal/gen"
	"enginelab.local/internal/loader"
	"enginelab.local/internal/model"
	"enginelab.local/internal/recovery"
)

func testBudget() model.Budget {
	return model.Budget{Workers: 4, QueueDepth: 16, MaxSegmentMemoryBytes: 16 << 20}
}

func TestParallelismScheduleAndBatchesDoNotChangeTruth(t *testing.T) {
	bundle := gen.NewBundle(gen.DefaultSeed)
	base, err := recovery.RecoverOneShot(bundle.Project, recovery.Options{Budget: testBudget(), BaselineItems: bundle.Truth.CheckpointItems})
	if err != nil {
		t.Fatal(err)
	}
	for _, workers := range []int{1, 2, 4, 8} {
		for _, batch := range []int{1, 2, 64} {
			got, err := recovery.RecoverOneShot(bundle.Project, recovery.Options{
				Budget:          model.Budget{Workers: workers, QueueDepth: 16, MaxSegmentMemoryBytes: 16 << 20},
				BaselineItems:   bundle.Truth.CheckpointItems,
				ReverseSchedule: workers%2 == 0,
				CommitBatchSize: batch,
			})
			if err != nil {
				t.Fatalf("workers=%d batch=%d: %v", workers, batch, err)
			}
			if cmp := recovery.CompareResults(base, got); !cmp.Equal {
				t.Fatalf("parallel mismatch workers=%d batch=%d: %+v", workers, batch, cmp.Differences)
			}
			for _, rep := range got.Segments {
				if rep.ElapsedNanoseconds < 0 {
					t.Fatalf("missing timing observation for %s", rep.SegmentID)
				}
			}
		}
	}
}

func TestBudgetBackpressureAndBounds(t *testing.T) {
	bundle := gen.NewBundle(gen.DefaultSeed)
	_, err := recovery.RecoverOneShot(bundle.Project, recovery.Options{
		Budget:        model.Budget{Workers: 1, QueueDepth: 0, MaxSegmentMemoryBytes: 16 << 20},
		BaselineItems: bundle.Truth.CheckpointItems,
	})
	var be *model.Error
	if !errors.As(err, &be) || be.Code != model.EBackpressure {
		t.Fatalf("expected E_BACKPRESSURE, got %v", err)
	}
	_, err = recovery.RecoverOneShot(bundle.Project, recovery.Options{Budget: model.Budget{Workers: 999}})
	if !errors.As(err, &be) || be.Code != model.EBudgetThreads {
		t.Fatalf("expected E_BUDGET_THREADS, got %v", err)
	}
	_, err = recovery.RecoverOneShot(bundle.Project, recovery.Options{Budget: model.Budget{Workers: 2, MaxSegmentMemoryBytes: -1}})
	if !errors.As(err, &be) || be.Code != model.EBudgetMemory {
		t.Fatalf("expected E_BUDGET_MEMORY bounds, got %v", err)
	}
	_, err = recovery.RecoverOneShot(bundle.Project, recovery.Options{Budget: model.Budget{Workers: 2, QueueDepth: 16, MaxSegmentMemoryBytes: 128}})
	if !errors.As(err, &be) || be.Code != model.EBudgetMemory {
		t.Fatalf("expected per-segment E_BUDGET_MEMORY, got %v", err)
	}
}

func TestCorruptSegmentDegradesWithoutChangingVisibility(t *testing.T) {
	bundle := gen.NewBundle(gen.DefaultSeed)
	raw := bundle.SegmentFiles["seg-003"]
	corrupt := loader.CorruptLastRecordByte(raw)
	parsed, err := loader.ParseSegmentFile("seg-003", corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Integrity != model.IntegrityDegraded || len(parsed.Diagnostics) == 0 || parsed.Diagnostics[0].Code != model.ERecordIncomplete {
		t.Fatalf("expected incomplete degradation, got %+v", parsed)
	}
	p, err := recovery.CloneProject(bundle.Project)
	if err != nil {
		t.Fatal(err)
	}
	seg := p.Segment("seg-003")
	seg.Records = parsed.Records
	seg.Integrity = parsed.Integrity
	seg.Diagnostics = parsed.Diagnostics
	seg.CompleteRecords = parsed.CompleteRecords
	seg.RawRecordCount = parsed.RawRecordCount

	result, err := recovery.RecoverOneShot(p, recovery.Options{Budget: testBudget()})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Items, bundle.Truth.Items) {
		t.Fatalf("corruption changed visible items\n got=%#v\nwant=%#v", result.Items, bundle.Truth.Items)
	}
	report := result.SegmentMap["seg-003"]
	if report.State != model.StateDegraded {
		t.Fatalf("seg-003 state=%s want degraded", report.State)
	}
	if report.LastRecovered.RecordID == "" || report.LastRecovered.LSN == 0 {
		t.Fatalf("missing last recoverable point: %+v", report)
	}

	truncatedRaw, err := loader.TruncateSegmentFileToRecords(bundle.SegmentFiles["seg-004"], 6)
	if err != nil {
		t.Fatal(err)
	}
	truncated, err := loader.ParseSegmentFile("seg-004", truncatedRaw)
	if err != nil {
		t.Fatal(err)
	}
	if truncated.Diagnostics[0].Code != model.ESegmentTruncated || truncated.CompleteRecords != 6 {
		t.Fatalf("unexpected truncation report: %+v", truncated)
	}
}

func TestThreeSchemaLoadAndTruthRecovery(t *testing.T) {
	bundle := gen.NewBundle(gen.DefaultSeed)
	loaded := map[model.SchemaVersion]*model.Project{}
	for version, doc := range bundle.Docs {
		p, err := loader.LoadDocument(doc)
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		loaded[version] = p
	}
	if err := recovery.CompareVersionProjects(loaded); err != nil {
		t.Fatalf("schema versions differ: %v", err)
	}

	result, err := recovery.RecoverOneShot(loaded[model.SchemaV1], recovery.Options{
		BaselineItems: bundle.Truth.CheckpointItems,
		Budget:        model.Budget{Workers: 4, QueueDepth: 8},
	})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !reflect.DeepEqual(result.Items, bundle.Truth.Items) {
		t.Fatalf("items mismatch\n got=%#v\nwant=%#v", result.Items, bundle.Truth.Items)
	}
	for id, want := range bundle.Truth.Ranges {
		got := result.Ranges[id]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("range %s mismatch\n got=%#v\nwant=%#v", id, got, want)
		}
		if !model.RangeWithinTolerance(got, want, bundle.Truth.Tolerance) {
			t.Fatalf("range %s outside tolerance %+v", id, bundle.Truth.Tolerance)
		}
	}
}

func TestTransactionVisibilitySets(t *testing.T) {
	bundle := gen.NewBundle(gen.DefaultSeed)
	p, err := recovery.CloneProject(bundle.Project)
	if err != nil {
		t.Fatal(err)
	}
	p.Checkpoints = nil
	p.LatestCheckpoint = nil
	result, err := recovery.RecoverOneShot(p, recovery.Options{Budget: testBudget(), BaselineItems: map[string]model.DataItem{}})
	if err != nil {
		t.Fatal(err)
	}
	visible := []string{}
	for _, seg := range result.Segments {
		visible = append(visible, seg.VisibleRecords...)
	}
	sort.Strings(visible)
	if !reflect.DeepEqual(visible, bundle.Truth.VisibleRecordIDs) {
		t.Fatalf("visible records mismatch\n got=%d %#v\nwant=%d %#v", len(visible), visible, len(bundle.Truth.VisibleRecordIDs), bundle.Truth.VisibleRecordIDs)
	}
	if !reflect.DeepEqual(result.CommittedTxIDs, bundle.Truth.CommittedTxIDs) {
		t.Fatalf("committed tx mismatch\n got=%v\nwant=%v", result.CommittedTxIDs, bundle.Truth.CommittedTxIDs)
	}
	if !reflect.DeepEqual(result.AbortedTxIDs, bundle.Truth.AbortedTxIDs) {
		t.Fatalf("aborted tx mismatch\n got=%v\nwant=%v", result.AbortedTxIDs, bundle.Truth.AbortedTxIDs)
	}
	committed := map[string]bool{}
	for _, tx := range result.CommittedTxIDs {
		committed[tx] = true
	}
	for _, tx := range bundle.Truth.UncommittedTxIDs {
		if committed[tx] {
			t.Fatalf("uncommitted transaction %s became visible", tx)
		}
	}
}

func TestIncrementalInvalidationMatchesFull(t *testing.T) {
	bundle := gen.NewBundle(gen.DefaultSeed)
	engine, err := recovery.NewEngine(bundle.Project, testBudget(), bundle.Truth.CheckpointItems)
	if err != nil {
		t.Fatal(err)
	}
	edits := []recovery.Edit{
		{ID: "m1-truncate", Type: recovery.EditTruncateSegment, SegmentID: "seg-004", ToRecordCount: 5, Description: "remove seg-004 tx-b commit and tail"},
		{ID: "m2-overwrite", Type: recovery.EditOverwriteRecord, SegmentID: "seg-003", RecordID: "rec-seg-003-a-put", NewValue: "overwritten-delta", NewVersion: 9001},
		{ID: "m3-checkpoint", Type: recovery.EditAddCheckpoint, SegmentID: "seg-004", CheckpointID: "cp-2"},
		{ID: "m4-dependency", Type: recovery.EditRewriteDependency, SegmentID: "seg-005", NewPredecessors: []string{"seg-003"}},
	}
	wantRecomputed := [][]string{
		{"seg-004", "seg-005"},
		{"seg-003"},
		{"seg-004", "seg-005"},
		{"seg-005"},
	}
	for editIndex, edit := range edits {
		inc, err := engine.Apply(edit)
		if err != nil {
			t.Fatalf("apply %s: %v", edit.ID, err)
		}
		full, err := recovery.RecoverOneShot(engine.Project, recovery.Options{Budget: testBudget(), CommitBatchSize: 2})
		if err != nil {
			t.Fatalf("full after %s: %v", edit.ID, err)
		}
		cmp := recovery.CompareResults(inc, full)
		if !cmp.Equal {
			t.Fatalf("moment %s mismatch: %+v", edit.ID, cmp.Differences)
		}
		if !reflect.DeepEqual(inc.CacheRecomputed, wantRecomputed[editIndex]) {
			t.Fatalf("moment %s closure=%v want=%v", edit.ID, inc.CacheRecomputed, wantRecomputed[editIndex])
		}
		t.Logf("%s recomputed=%v reused=%v", edit.ID, inc.CacheRecomputed, inc.CacheReused)
	}
}

package gateway

import (
	"math"
	"testing"
)

func TestLateRecomputeDropAndInferredWatermarkEquivalence(t *testing.T) {
	bundle := testBundle(t)
	report := bundle.Truth

	assertHasSourceRecord(t, report.RecomputeEvents, "rec-000")
	assertHasSourceRecord(t, report.RecomputeEvents, "rec-002")
	assertHasSourceRecord(t, report.RecomputeEvents, "rec-004")
	assertDroppedRecord(t, report.Dropped, "rec-001", 4000)
	assertDroppedRecord(t, report.Dropped, "rec-003", 3000)
	assertDroppedRecord(t, report.Dropped, "rec-005", 4000)

	explicit := bundle.Canonical.Clone()
	explicit.WatermarkMarks = nil
	boundary := *explicit.CommittedBoundaryMS
	explicit.WatermarkMS = &boundary
	explicitReport := mustRun(t, explicit)

	inferred := bundle.Canonical.Clone()
	inferred.WatermarkMS = nil
	inferred.WatermarkMarks = nil
	inferred.CommittedBoundaryMS = &boundary
	inferredReport := mustRun(t, inferred)
	if inferredReport.WatermarkSource != "presumed_committed_boundary" {
		t.Fatalf("watermark source = %q, want presumed", inferredReport.WatermarkSource)
	}
	if CanonicalJSON(inferredReport.RecomputedKeys) != CanonicalJSON(explicitReport.RecomputedKeys) {
		t.Fatalf("inferred recompute keys %v != explicit %v", inferredReport.RecomputedKeys, explicitReport.RecomputedKeys)
	}
	if CanonicalJSON(inferredReport.Dropped) != CanonicalJSON(explicitReport.Dropped) {
		t.Fatal("inferred and explicit watermark produced different drop sets")
	}

	v2, err := LoadSchema(bundle.Schemas["v2"])
	if err != nil {
		t.Fatal(err)
	}
	v2Report := mustRun(t, v2)
	if v2Report.WatermarkSource != "presumed_committed_boundary" {
		t.Fatalf("v2 source = %s", v2Report.WatermarkSource)
	}
}

func assertHasSourceRecord(t *testing.T, events []RecomputeEvent, recordID string) {
	t.Helper()
	for _, event := range events {
		if event.SourceID == recordID {
			return
		}
	}
	t.Fatalf("expected recompute event from %s", recordID)
}

func assertDroppedRecord(t *testing.T, drops []DroppedRecord, recordID string, lateness int64) {
	t.Helper()
	found := false
	for _, drop := range drops {
		if drop.RecordID == recordID {
			found = true
			if drop.LatenessMS != lateness {
				t.Fatalf("%s lateness = %d, want %d", recordID, drop.LatenessMS, lateness)
			}
			if drop.WindowIndex == 0 && drop.WindowKey.String() == "" {
				t.Fatalf("drop %s missing window location", recordID)
			}
		}
	}
	if !found {
		t.Fatalf("expected drop for %s", recordID)
	}
}

func TestHistogramCriticalBoundariesAndFixedOverflow(t *testing.T) {
	bundle := testBundle(t)
	hist, err := NewHistogram(bundle.Canonical.HistogramBoundariesUS, bundle.Canonical.DurationToleranceUS)
	if err != nil {
		t.Fatal(err)
	}
	traces := map[string]DurationTrace{}
	for _, trace := range bundle.Truth.DurationTraces {
		traces[trace.RecordID] = trace
		if !trace.WithinTolerance {
			t.Fatalf("trace %s exceeds tolerance: %+v", trace.RecordID, trace)
		}
	}

	criticalCount := 0
	for _, record := range bundle.Canonical.Records {
		if len(record.ID) < 5 || record.ID[:5] != "crit-" {
			continue
		}
		criticalCount++
		trace := traces[record.ID]
		wantBucket := ClassifyFixed(record.DurationFixed, hist.BoundariesFixed)
		if trace.BucketIndex != wantBucket {
			t.Fatalf("critical record %s bucket = %d, want %d", record.ID, trace.BucketIndex, wantBucket)
		}
		if !trace.Applied || trace.Increment != 1 {
			t.Fatalf("critical record %s was not applied: %+v", record.ID, trace)
		}
		for iteration := 0; iteration < 100; iteration++ {
			if got := ClassifyFixed(record.DurationFixed, hist.BoundariesFixed); got != wantBucket {
				t.Fatalf("bucket migrated on iteration %d for %s", iteration, record.ID)
			}
		}
	}
	if criticalCount != 9 {
		t.Fatalf("critical count = %d, want 9", criticalCount)
	}

	if _, err := USSmallToFixed(math.MaxInt64); err == nil {
		t.Fatal("expected fixed-point overflow error")
	} else {
		assertErrorCode(t, err, CodeDurationOverflow)
	}
	fixed, saturated := SaturatingFromUS(math.MaxInt64)
	if !saturated || fixed != FixedDuration(math.MaxInt64) {
		t.Fatalf("saturation = (%d,%v)", fixed, saturated)
	}
	if _, err := NewHistogram([]int64{10, 9}, 0); err == nil {
		t.Fatal("expected boundary order error")
	} else {
		assertErrorCode(t, err, CodeBucketBoundaryInvalid)
	}
	if _, err := NewHistogram([]int64{math.MaxInt64}, 0); err == nil {
		t.Fatal("expected boundary scale error")
	} else {
		assertErrorCode(t, err, CodeBucketScaleOutOfRange)
	}
}

func TestExportBatchInvariants(t *testing.T) {
	bundle := testBundle(t)
	batches := bundle.Truth.Batches
	if len(batches) < 2 {
		t.Fatalf("expected multiple batches, got %d", len(batches))
	}
	seenWindows := map[string]int{}
	for _, batch := range batches {
		if batch.WindowCount > bundle.Canonical.Export.MaxWindowsPerBatch {
			t.Fatalf("batch %d windows %d exceeds max", batch.BatchNo, batch.WindowCount)
		}
		if batch.RecordCount > int64(bundle.Canonical.Export.MaxRecordsPerBatch) {
			t.Fatalf("batch %d records %d exceeds max", batch.BatchNo, batch.RecordCount)
		}
		unique := map[string]bool{}
		for _, key := range batch.WindowKeys {
			if unique[key] {
				t.Fatalf("window %s duplicated in batch %d", key, batch.BatchNo)
			}
			unique[key] = true
			if prior, ok := seenWindows[key]; ok {
				t.Fatalf("window %s split across batches %d and %d", key, prior, batch.BatchNo)
			}
			seenWindows[key] = batch.BatchNo
			if batch.PriorityCodes[key] == 0 {
				t.Fatalf("window %s missing priority code", key)
			}
		}
		if batch.SnapshotDigest == "" || batch.SnapshotSummary == "" {
			t.Fatalf("batch %d missing snapshot summary", batch.BatchNo)
		}
	}

	rebuilt, err := BuildBatches(bundle.Canonical, bundle.Truth.Windows)
	if err != nil {
		t.Fatal(err)
	}
	if !CompareBatches(batches, rebuilt) {
		t.Fatal("one-shot export rebuild differs from generator truth")
	}
}

func TestParallelMatchesSequentialAndBackpressureCodes(t *testing.T) {
	bundle := testBundle(t)
	parallel, err := RunParallel(bundle.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	assertAggregatesMatch(t, bundle.Truth.Windows, parallel.Windows, 0)
	assertDropsMatch(t, bundle.Truth.Dropped, parallel.Dropped)
	if !CompareBatches(bundle.Truth.Batches, parallel.Batches) {
		t.Fatal("parallel batches differ")
	}
	if parallel.Diagnostics == nil || len(parallel.Diagnostics.Services) != len(bundle.Canonical.Services) {
		t.Fatalf("missing per-service diagnostics: %+v", parallel.Diagnostics)
	}

	reversed := bundle.Canonical.Clone()
	for i, j := 0, len(reversed.Services)-1; i < j; i, j = i+1, j-1 {
		reversed.Services[i], reversed.Services[j] = reversed.Services[j], reversed.Services[i]
	}
	reversed.Budget.Workers = 1
	oneWorker, err := RunParallel(reversed)
	if err != nil {
		t.Fatal(err)
	}
	assertAggregatesMatch(t, bundle.Truth.Windows, oneWorker.Windows, 0)

	queue, err := NewBoundedQueue[int](1)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.TryEnqueue(1); err != nil {
		t.Fatal(err)
	}
	err = queue.TryEnqueue(2)
	ge := assertErrorCode(t, err, CodeBackpressure)
	if ge.Context["capacity"] != 1 {
		t.Fatalf("backpressure context missing capacity: %+v", ge.Context)
	}

	pool, err := NewWorkerPool(bundle.Canonical.Budget)
	if err != nil {
		t.Fatal(err)
	}
	release, err := pool.TryAcquire()
	if err != nil {
		t.Fatal(err)
	}
	release()

	one := Budget{Workers: 1, ServiceBuffer: 1, MaxWorkers: 1, MaxServiceBuffer: 1}
	exhausted, err := NewWorkerPool(one)
	if err != nil {
		t.Fatal(err)
	}
	release, err = exhausted.TryAcquire()
	if err != nil {
		t.Fatal(err)
	}
	_, err = exhausted.TryAcquire()
	assertErrorCode(t, err, CodeBackpressure)
	release()
}

package textureconv

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestResumeProducesSameResultAsFull(t *testing.T) {
	b := testBundle(t, SamplePlan())
	rec := b.Manifest.Images[0]
	dir := t.TempDir()
	store, path := openTestStore(t, dir, "resume.db")
	e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p1",
		TileSize: 24, Workers: 4, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	partial, err := e.ProcessLimit(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Progress.CommittedTiles == 0 || partial.Progress.CommittedTiles == partial.Progress.TotalTiles {
		t.Fatalf("expected partial checkpoint, got %d/%d", partial.Progress.CommittedTiles, partial.Progress.TotalTiles)
	}
	store.Close()

	resumedStore, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p1",
		TileSize: 24, Workers: 4, Store: resumedStore,
	})
	if err != nil {
		resumedStore.Close()
		t.Fatal(err)
	}
	if inv, why := resumed.InvalidatedAtLoad(); inv {
		resumedStore.Close()
		t.Fatalf("matching snapshot was invalidated: %s", why)
	}
	if got := resumed.Progress().CommittedTiles; got != partial.Progress.CommittedTiles {
		resumedStore.Close()
		t.Fatalf("resume progress %d != checkpoint %d", got, partial.Progress.CommittedTiles)
	}
	processAll(t, resumed)
	resumeSnap, err := resumed.Snapshot()
	if err != nil {
		resumedStore.Close()
		t.Fatal(err)
	}
	if _, err := VerifyPersisted(b, rec.ID, resumedStore, VerifyOptions{
		ExpectedParamsVersion: "p1", ExpectedAlgorithmVersion: AlgorithmVersion,
	}); err != nil {
		resumedStore.Close()
		t.Fatalf("verify resume: %v", err)
	}
	resumedStore.Close()

	fullStore, _ := openTestStore(t, dir, "full.db")
	full, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p1",
		TileSize: 24, Workers: 1, Store: fullStore,
	})
	if err != nil {
		fullStore.Close()
		t.Fatal(err)
	}
	processAll(t, full)
	fullSnap, err := full.Snapshot()
	if err != nil {
		fullStore.Close()
		t.Fatal(err)
	}
	fullStore.Close()
	if err := CompareSnapshots(resumeSnap, fullSnap); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledRunRetainsOnlyCommittedCheckpoint(t *testing.T) {
	b := testBundle(t, SamplePlan())
	rec := b.Manifest.Images[0]
	store, path := openTestStore(t, t.TempDir(), "cancel.db")
	e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "pc",
		TileSize: 24, Workers: 2, Store: store,
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	partial, err := e.ProcessLimit(context.Background(), 2)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Process(ctx); err == nil {
		store.Close()
		t.Fatal("canceled Process returned nil error")
	}
	if got := e.Progress().CommittedTiles; got != partial.Progress.CommittedTiles {
		store.Close()
		t.Fatalf("cancel changed committed count %d -> %d", partial.Progress.CommittedTiles, got)
	}
	store.Close()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "pc",
		TileSize: 24, Workers: 2, Store: reopened,
	})
	if err != nil {
		reopened.Close()
		t.Fatal(err)
	}
	if got := resumed.Progress().CommittedTiles; got != partial.Progress.CommittedTiles {
		reopened.Close()
		t.Fatalf("reopened checkpoint has %d, want %d", got, partial.Progress.CommittedTiles)
	}
	processAll(t, resumed)
	if _, err := VerifyPersisted(b, rec.ID, reopened, VerifyOptions{
		ExpectedParamsVersion: "pc", ExpectedAlgorithmVersion: AlgorithmVersion,
	}); err != nil {
		reopened.Close()
		t.Fatal(err)
	}
	reopened.Close()
}

func TestAppendEntryReusesCommittedUpstreamCache(t *testing.T) {
	planA := SamplePlan()
	bA := testBundle(t, planA)
	planB := clonePlan(planA)
	planB.Entries = append(planB.Entries, Entry{
		Seq: 6, TargetDepth: Depth16, TargetChannels: 4,
		Permutation: []int{0, 1, 2, 3}, Fill: []FillSpec{}, Drop: []int{},
		Quantization: QuantRound, Alpha: AlphaPreserve,
	})
	bB := testBundle(t, planB)
	if bA.Hashes["rgba8"] != bB.Hashes["rgba8"] {
		t.Fatal("deterministic source changed between plans")
	}
	rec := bA.Manifest.Images[0]
	dir := t.TempDir()
	store, _ := openTestStore(t, dir, "incremental.db")
	e, err := NewEngine(bA.Images[rec.ID], planA, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p2",
		TileSize: 24, Workers: 4, Store: store,
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := e.ProcessLimit(context.Background(), 2); err != nil {
		store.Close()
		t.Fatal(err)
	}
	manifest, err := e.ApplyEdit(planB, EditOptions{})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if len(manifest.Affected) != 1 || manifest.Affected[0].Entry != 6 {
		store.Close()
		t.Fatalf("append invalidation should only target the new entry: %+v", manifest)
	}
	run, err := e.Process(context.Background())
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if run.CacheHits == 0 {
		store.Close()
		t.Fatal("expected committed upstream cache hits after append")
	}
	incSnap, err := e.Snapshot()
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := VerifyPersisted(bB, rec.ID, store, VerifyOptions{
		ExpectedParamsVersion: "p2", ExpectedAlgorithmVersion: AlgorithmVersion,
	}); err != nil {
		store.Close()
		t.Fatalf("verify append: %v", err)
	}
	store.Close()

	fullStore, _ := openTestStore(t, dir, "full-append.db")
	full, err := NewEngine(bB.Images[rec.ID], planB, EngineOptions{
		ImageID: rec.ID, SourceHash: bB.Hashes[rec.ID], ParamsVersion: "p2",
		TileSize: 24, Workers: 1, Store: fullStore,
	})
	if err != nil {
		fullStore.Close()
		t.Fatal(err)
	}
	processAll(t, full)
	fullSnap, err := full.Snapshot()
	if err != nil {
		fullStore.Close()
		t.Fatal(err)
	}
	fullStore.Close()
	if err := CompareSnapshots(incSnap, fullSnap); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedChunksRemainFrozenWithoutExplicitUnfreeze(t *testing.T) {
	b := testBundle(t, SamplePlan())
	rec := b.Manifest.Images[0]
	store, _ := openTestStore(t, t.TempDir(), "frozen.db")
	defer store.Close()
	e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p3",
		TileSize: 24, Workers: 1, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ProcessLimit(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	before := e.Progress()
	edited := clonePlan(b.Plan)
	edited.Entries[2].Alpha = AlphaPremultiply
	_, err = e.ApplyEdit(edited, EditOptions{})
	mustCode(t, err, CodeState)
	after := e.Progress()
	if before.CommittedTiles != after.CommittedTiles {
		t.Fatalf("frozen edit changed progress %d -> %d", before.CommittedTiles, after.CommittedTiles)
	}
	if _, err := e.ApplyEdit(edited, EditOptions{AllowUnfreeze: true}); err != nil {
		t.Fatalf("explicit unfreeze failed: %v", err)
	}
}

func TestFailureIsIsolatedAndRetryMatchesCleanRun(t *testing.T) {
	b := testBundle(t, SamplePlan())
	rec := b.Manifest.Images[0]
	dir := t.TempDir()
	var once atomic.Bool
	store, _ := openTestStore(t, dir, "fault.db")
	e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p4",
		TileSize: 24, Workers: 4, Store: store,
		FaultInjector: func(entry, tile int) error {
			if entry == 2 && tile == 1 && once.CompareAndSwap(false, true) {
				return errors.New("injected once")
			}
			return nil
		},
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	firstReport, err := e.Process(context.Background())
	mustCode(t, err, CodeProcess)
	if len(firstReport.Failures) == 0 {
		store.Close()
		t.Fatal("expected a failed tile report")
	}
	if got := e.State(2, 1); got != StateFailed {
		store.Close()
		t.Fatalf("failing tile state = %s", got)
	}
	if _, err := e.Snapshot(); err == nil {
		store.Close()
		t.Fatal("snapshot unexpectedly succeeded with a failed tile")
	}
	processAll(t, e)
	if got := e.State(2, 1); got != StateCommitted {
		store.Close()
		t.Fatalf("retried tile state = %s", got)
	}
	retrySnap, err := e.Snapshot()
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := VerifyPersisted(b, rec.ID, store, VerifyOptions{
		ExpectedParamsVersion: "p4", ExpectedAlgorithmVersion: AlgorithmVersion,
	}); err != nil {
		store.Close()
		t.Fatalf("verify retry: %v", err)
	}
	store.Close()
	cleanStore, _ := openTestStore(t, dir, "clean.db")
	clean, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p4",
		TileSize: 24, Workers: 4, Store: cleanStore,
	})
	if err != nil {
		cleanStore.Close()
		t.Fatal(err)
	}
	processAll(t, clean)
	cleanSnap, err := clean.Snapshot()
	if err != nil {
		cleanStore.Close()
		t.Fatal(err)
	}
	cleanStore.Close()
	if err := CompareSnapshots(retrySnap, cleanSnap); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotCorruptionInvalidatesWholeSnapshot(t *testing.T) {
	b := testBundle(t, SamplePlan())
	rec := b.Manifest.Images[0]
	dir := t.TempDir()
	store, path := openTestStore(t, dir, "corrupt.db")
	e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p5",
		TileSize: 24, Workers: 2, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	processAll(t, e)
	store.Close()
	raw, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.db.Exec(`UPDATE stage_results SET content_hash='corrupted'
		WHERE rowid=(SELECT MIN(rowid) FROM stage_results)`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p5",
		TileSize: 24, Workers: 2, Store: reopened,
	})
	if err != nil {
		reopened.Close()
		t.Fatal(err)
	}
	invalidated, why := e2.InvalidatedAtLoad()
	if !invalidated || why == "" {
		reopened.Close()
		t.Fatal("corrupt snapshot was not invalidated as a whole")
	}
	if p := e2.Progress(); p.CommittedTiles != 0 {
		reopened.Close()
		t.Fatalf("partial old rows survived invalidation: %+v", p)
	}
	processAll(t, e2)
	if _, err := VerifyPersisted(b, rec.ID, reopened, VerifyOptions{
		ExpectedParamsVersion: "p5", ExpectedAlgorithmVersion: AlgorithmVersion,
	}); err != nil {
		reopened.Close()
		t.Fatal(err)
	}
	reopened.Close()
}

func TestPlanFingerprintMismatchWipesSnapshot(t *testing.T) {
	planA := SamplePlan()
	bA := testBundle(t, planA)
	planB := clonePlan(planA)
	planB.Entries[4].Fill[0].Value = 12345
	bB := testBundle(t, planB)
	rec := bA.Manifest.Images[0]
	dir := t.TempDir()
	store, path := openTestStore(t, dir, "plan-mismatch.db")
	e, err := NewEngine(bA.Images[rec.ID], planA, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p6",
		TileSize: 24, Workers: 2, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	processAll(t, e)
	store.Close()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := NewEngine(bB.Images[rec.ID], planB, EngineOptions{
		ImageID: rec.ID, SourceHash: bB.Hashes[rec.ID], ParamsVersion: "p6",
		TileSize: 24, Workers: 2, Store: reopened,
	})
	if err != nil {
		reopened.Close()
		t.Fatal(err)
	}
	inv, why := e2.InvalidatedAtLoad()
	if !inv || why == "" {
		reopened.Close()
		t.Fatal("plan mismatch did not invalidate snapshot")
	}
	if p := e2.Progress(); p.CommittedTiles != 0 {
		reopened.Close()
		t.Fatalf("mixed old progress survived: %+v", p)
	}
	reopened.Close()
}

func TestCommitOrderIsStableAndDense(t *testing.T) {
	b := testBundle(t, SamplePlan())
	rec := b.Manifest.Images[0]
	store, _ := openTestStore(t, t.TempDir(), "order.db")
	e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "p7",
		TileSize: 24, Workers: 4, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	processAll(t, e)
	rows, err := store.db.Query(`SELECT submit_order FROM chunk_commits ORDER BY submit_order`)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	var orders []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			store.Close()
			t.Fatal(err)
		}
		orders = append(orders, v)
	}
	rows.Close()
	store.Close()
	if len(orders) == 0 {
		t.Fatal("no chunk commits")
	}
	for i, v := range orders {
		if v != i+1 {
			t.Fatalf("submit orders are not dense/stable at %d: %v", i, orders)
		}
	}
}

func TestStorePathDirectoryIsCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "db", "state.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

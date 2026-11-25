package maskqa

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestIncrementalCoordinateEditReusesPersistentTiles(t *testing.T) {
	set, _ := GenerateSet(DefaultSeed)
	fx := set.Fixtures[0]
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "incremental.db")

	store, err := OpenCheckpoint(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(fx.Project, fx.Truth, DefaultSchedulerConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenCheckpoint(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	edited := *fx.Project
	edited.Strokes = append([]Stroke(nil), fx.Project.Strokes...)
	edited.Strokes[0].Start.X += 3
	plan, err := PlanInvalidation(fx.Project, &edited, 1, eng.Tiles)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.AffectedTiles, []int{0}) {
		t.Fatalf("coordinate edit affected tiles=%v candidate=%v", plan.AffectedTiles, plan.CandidateTiles)
	}
	if len(plan.Items) != 1 || plan.Items[0].StrokeIndex != 1 || plan.Items[0].TileID != 0 {
		t.Fatalf("invalidation items=%+v", plan.Items)
	}

	incEng, err := NewEngine(&edited, fx.Truth, DefaultSchedulerConfig(), reopened)
	if err != nil {
		t.Fatal(err)
	}
	inc, incErr := incEng.Run(ctx, RunOptions{Invalidation: plan})
	if incErr != nil {
		if qe, ok := AsError(incErr); !ok || (qe.Code != CodeTOP && qe.Code != CodeDIR) {
			t.Fatalf("incremental run unexpected error: %v", incErr)
		}
	}
	if !reflect.DeepEqual(inc.RecomputedTiles, []int{0}) {
		t.Fatalf("recomputed=%v", inc.RecomputedTiles)
	}
	if len(inc.ReusedTiles) != len(eng.Tiles)-1 {
		t.Fatalf("reused tiles=%v all=%d", inc.ReusedTiles, len(eng.Tiles))
	}

	fullEng, err := NewEngine(&edited, fx.Truth, DefaultSchedulerConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	full, fullErr := fullEng.Run(ctx, RunOptions{})
	if fullErr != nil {
		if qe, ok := AsError(fullErr); !ok || (qe.Code != CodeTOP && qe.Code != CodeDIR) {
			t.Fatalf("full edited run unexpected error: %v", fullErr)
		}
	}
	if !inc.Mask.Equal(full.Mask) {
		tile, p, msg := FirstMaskDifference(inc.Mask, full.Mask, eng.Tiles)
		t.Fatalf("incremental != full tile=%d p=%v %s", tile, p, msg)
	}
}

func TestIncrementalWeightEditKeepsTruthAndReusesTiles(t *testing.T) {
	set, _ := GenerateSet(DefaultSeed)
	fx := set.Fixtures[0]
	ctx := context.Background()
	store, err := OpenCheckpoint(filepath.Join(t.TempDir(), "weight.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	eng, _ := NewEngine(fx.Project, fx.Truth, DefaultSchedulerConfig(), store)
	if _, err := eng.Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}

	edited := *fx.Project
	edited.Strokes = append([]Stroke(nil), fx.Project.Strokes...)
	edited.Strokes[0].WeightBPS = 9500
	plan, err := PlanInvalidation(fx.Project, &edited, 1, eng.Tiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.AffectedTiles) != 2 {
		t.Fatalf("weight affected tiles=%v", plan.AffectedTiles)
	}
	incEng, _ := NewEngine(&edited, fx.Truth, DefaultSchedulerConfig(), store)
	inc, err := incEng.Run(ctx, RunOptions{Invalidation: plan})
	if err != nil {
		t.Fatalf("weight edit should preserve binary truth: %v", err)
	}
	if !inc.Report.OK || !inc.Mask.Equal(fx.Truth.Mask) {
		t.Fatal("weight edit changed resolved binary mask or report")
	}
	if !reflect.DeepEqual(inc.RecomputedTiles, plan.AffectedTiles) {
		t.Fatalf("recomputed=%v planned=%v", inc.RecomputedTiles, plan.AffectedTiles)
	}
	if len(inc.ReusedTiles) == 0 {
		t.Fatal("expected unaffected persistent tile reuse")
	}
}

func TestThreePathsPixelReportAndSequenceEqual(t *testing.T) {
	set, err := GenerateSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, fx := range set.Fixtures {
		dir := filepath.Join(t.TempDir(), fx.Name)
		report, err := RunThreePaths(ctx, fx.Project, fx.Truth, SchedulerConfig{MaxParallelism: 2, MaxPixelsPerWorker: 4096, QueueBudgetTiles: 2}, dir)
		if err != nil {
			if qe, ok := AsError(err); ok {
				t.Fatalf("%s three paths: %v diffs=%v firstTile=%d", fx.Name, err, report.Differences, qe.FirstTile)
			}
			t.Fatalf("%s three paths: %v", fx.Name, err)
		}
		if !report.OK || report.FirstDiffTile != -1 {
			t.Fatalf("%s bad report %+v", fx.Name, report)
		}
		if report.Realtime.CommitCount == 0 || report.Realtime.CommitCount != report.Full.CommitCount || report.Recovery.CommitCount != report.Full.CommitCount {
			t.Fatalf("commit counts realtime=%d recovery=%d full=%d", report.Realtime.CommitCount, report.Recovery.CommitCount, report.Full.CommitCount)
		}
		if report.Recovery.Recovery == nil || report.Recovery.Recovery.CommittedStrokeBoundary == 0 || report.Recovery.Recovery.Invalidated {
			t.Fatalf("recovery summary=%+v", report.Recovery.Recovery)
		}
	}
}

func TestSQLiteHashAndVersionInvalidation(t *testing.T) {
	set, _ := GenerateSet(DefaultSeed)
	fx := set.Fixtures[0]
	ctx := context.Background()
	store, err := OpenCheckpoint(filepath.Join(t.TempDir(), "corrupt.db"))
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := NewEngine(fx.Project, fx.Truth, DefaultSchedulerConfig(), store)
	if _, err := eng.Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CorruptCheckpointForTest(ctx, fx.Project.ProjectID, 1, 0); err != nil {
		t.Fatal(err)
	}
	rec, cps, err := store.Recover(ctx, fx.Project.ProjectID, fx.Project.ResidualDigest, eng.Order.OrderVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Invalidated || rec.Reason == "" {
		t.Fatalf("recovery=%+v", rec)
	}
	if len(cps) != 0 {
		t.Fatalf("corrupt snapshot was not fully invalidated: %d checkpoints", len(cps))
	}

	if _, err := eng.Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CorruptMetaVersionForTest(ctx, fx.Project.ProjectID); err != nil {
		t.Fatal(err)
	}
	rec, _, err = store.Recover(ctx, fx.Project.ProjectID, fx.Project.ResidualDigest, eng.Order.OrderVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Invalidated {
		t.Fatalf("version corruption recovery=%+v", rec)
	}
	_ = store.Close()
}

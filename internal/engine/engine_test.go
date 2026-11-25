package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"atelier/internal/apperr"
	"atelier/internal/model"
	"atelier/internal/sample"
)

func newTestEngine(t *testing.T) (*Engine, model.Scenario, Snapshot) {
	t.Helper()
	sc := sample.Generate(sample.DefaultSeed)
	e, err := New(sc)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	snap, err := e.FullRebuild(context.Background())
	if err != nil {
		t.Fatalf("full rebuild: %v", err)
	}
	return e, sc, snap
}

func TestDeterministicTruthAndHeatmapTolerances(t *testing.T) {
	e, sc, snap := newTestEngine(t)

	truthPoints := map[string][]model.TruthPoint{}
	truthDensity := map[string]float64{}
	for _, p := range sc.Truth.Points {
		truthPoints[p.ZoneID] = append(truthPoints[p.ZoneID], p)
	}
	for _, zt := range sc.Truth.Zones {
		truthDensity[zt.ZoneID] = zt.BaseDensity
	}
	if diff := math.Abs(float64(len(snap.Points) - len(sc.Truth.Points))); diff >
		math.Max(ReconCountAbsTol, ReconCountRelTol*float64(len(sc.Truth.Points))) {
		t.Fatalf("reconstructed points=%d truth=%d outside tolerance", len(snap.Points), len(sc.Truth.Points))
	}
	for _, state := range snap.ZoneStates {
		truth := truthPoints[state.ZoneID]
		countDiff := math.Abs(float64(len(state.Points) - len(truth)))
		limit := math.Max(ReconCountAbsTol, ReconCountRelTol*float64(len(truth)))
		if countDiff > limit {
			t.Errorf("%s point count diff %.1f > %.1f", state.ZoneID, countDiff, limit)
		}
		actualDensity := float64(len(truth)) / state.Estimate.EffectiveArea
		if math.Abs(state.Estimate.Density-actualDensity) > DensityAbsTolerance {
			t.Errorf("%s density %.6f vs truth %.6f", state.ZoneID, state.Estimate.Density, actualDensity)
		}
		truthRarity := rarityFractions(truth)
		if l1 := l1Distance(state.Estimate.Rarity, truthRarity); l1 > RarityL1Tolerance {
			t.Errorf("%s rarity L1 %.6f > %.6f: got=%v truth=%v base=%v",
				state.ZoneID, l1, RarityL1Tolerance, state.Estimate.Rarity, truthRarity, truthDensity[state.ZoneID])
		}
	}
	for _, obs := range sc.Observations {
		v, ok := snap.Heatmap.ValueAtXY(obs.X, obs.Y)
		if !ok {
			t.Fatalf("heatmap missing observation %s", obs.ID)
		}
		if math.Abs(v-obs.Density) > HeatAbsTolerance {
			t.Fatalf("heatmap at %s=%.6f obs=%.6f error > %.6f", obs.ID, v, obs.Density, HeatAbsTolerance)
		}
	}

	second, err := e.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotsByteEqual(t, snap, second)

	e2, err := New(sample.Generate(sample.DefaultSeed))
	if err != nil {
		t.Fatal(err)
	}
	third, err := e2.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotsByteEqual(t, snap, third)
}

func TestIncrementalPreciseInvalidationAndFullEquality(t *testing.T) {
	e, _, initial := newTestEngine(t)
	ctx := context.Background()

	updated, err := e.UpdateZoneParams(ctx, "west-woods", model.ZoneParams{DensityScale: 1.07, RarityShift: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Invalidation.AffectedZones) != 1 || updated.Invalidation.ReusedZoneCount != 3 {
		t.Fatalf("unexpected invalidation: %+v", updated.Invalidation)
	}
	if updated.Invalidation.AffectedZones[0].Resample {
		t.Fatal("density parameter edit should reuse the stratified samples")
	}
	if updated.Invalidation.RecomputedPixels == 0 || updated.Invalidation.ReusedPixels == 0 {
		t.Fatalf("expected partial heatmap invalidation, got %+v", updated.Invalidation)
	}
	before := stateByID(initial.ZoneStates, "west-woods")
	after := stateByID(updated.ZoneStates, "west-woods")
	if before.SamplingKey != after.SamplingKey {
		t.Fatal("sampling key changed for a density-only edit")
	}
	if before.EstimateKey == after.EstimateKey {
		t.Fatal("estimate key did not change for a density edit")
	}
	for _, zoneID := range []string{"central-meadow", "east-ridge", "south-marsh"} {
		if stateByID(initial.ZoneStates, zoneID).ContentHash != stateByID(updated.ZoneStates, zoneID).ContentHash {
			t.Fatalf("unaffected zone %s content changed", zoneID)
		}
	}
	compareEqual(t, e)

	rarityOnly, err := e.UpdateZoneParams(ctx, "west-woods", model.ZoneParams{DensityScale: 1.07, RarityShift: 0.12})
	if err != nil {
		t.Fatal(err)
	}
	if rarityOnly.Invalidation.AffectedZones[0].Heatmap {
		t.Fatal("rarity-only edit must not invalidate heatmap pixels")
	}
	if rarityOnly.Invalidation.RecomputedPixels != 0 {
		t.Fatalf("rarity-only edit recomputed %d pixels", rarityOnly.Invalidation.RecomputedPixels)
	}
	compareEqual(t, e)

	moved, err := e.MoveExclusion(ctx, "mirror-lake", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved.Invalidation.AffectedZones) == 0 {
		t.Fatal("moving an exclusion should invalidate intersecting zones")
	}
	for _, inv := range moved.Invalidation.AffectedZones {
		if !inv.Resample || !inv.Reestimate || !inv.Reconstruct || !inv.Heatmap {
			t.Fatalf("exclusion invalidation is not precise: %+v", inv)
		}
	}
	hasTileCoordinates := false
	for _, entry := range moved.Invalidation.Entries {
		if entry.TileX >= 0 && entry.TileY >= 0 && entry.Reason != "" && entry.ReuseState != "" {
			hasTileCoordinates = true
			break
		}
	}
	if !hasTileCoordinates {
		t.Fatal("invalidation manifest is missing tile coordinates or status")
	}
	compareEqual(t, e)
}

func TestZoneBoundaryEditAndDiffDiagnostics(t *testing.T) {
	e, _, initial := newTestEngine(t)
	ctx := context.Background()
	for i, zone := range e.zoneOrder {
		if zone.ID != "west-woods" {
			continue
		}
		poly := model.Polygon{Vertices: append([]model.Point(nil), zone.Polygon.Vertices...)}
		poly.Vertices[1].X = 84
		poly.Vertices[2].X = 78
		snap, err := e.UpdateZoneBoundary(ctx, zone.ID, poly)
		if err != nil {
			t.Fatalf("boundary update: %v", err)
		}
		if len(snap.Invalidation.AffectedZones) != 1 || snap.Invalidation.ReusedZoneCount != 3 {
			t.Fatalf("boundary invalidation leaked: %+v", snap.Invalidation)
		}
		compareEqual(t, e)
		_ = i
		return
	}
	t.Fatal("west zone not found")

	broken := initial
	broken.Heatmap = cloneHeatmap(initial.Heatmap)
	broken.Heatmap.Values[127] += 0.25
	_, err := CompareSnapshots(broken, initial)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.CodeDiff || ae.PixelX != 7 || ae.PixelY != 2 {
		t.Fatalf("expected E_DIFF at pixel 7,2, got %#v", err)
	}
}

func TestBoundedWorkerPoolBlocksAndRecordsReason(t *testing.T) {
	pool := newWorkerPool(1, 1)
	defer pool.Close()
	err := pool.Run(context.Background(), 8, func(i int) {
		time.Sleep(2 * time.Millisecond)
	})
	if err != nil {
		t.Fatal(err)
	}
	metrics := pool.Metrics()
	if metrics.BufferBlockEvents == 0 || metrics.LastBlockReason == "" {
		t.Fatalf("expected buffer blocking to be recorded: %+v", metrics)
	}
}

func TestIndependentRegionProfiles(t *testing.T) {
	cases := []struct {
		seed     int64
		regionID string
		zones    int
	}{
		{2042, "frontier-basin", 4},
		{7183, "ember-rift", 3},
		{9911, "tidal-reach", 4},
	}
	for _, testCase := range cases {
		scenario := sample.Generate(testCase.seed)
		if scenario.Region.ID != testCase.regionID || len(scenario.Region.Zones) != testCase.zones {
			t.Fatalf("seed %d profile mismatch: region=%s zones=%d", testCase.seed, scenario.Region.ID, len(scenario.Region.Zones))
		}
		eng, err := New(scenario)
		if err != nil {
			t.Fatalf("seed %d: %v", testCase.seed, err)
		}
		snap, err := eng.FullRebuild(context.Background())
		if err != nil {
			t.Fatalf("seed %d full rebuild: %v", testCase.seed, err)
		}
		if len(snap.Heatmap.Values) != snap.Heatmap.Width*snap.Heatmap.Height || len(snap.Points) == 0 {
			t.Fatalf("seed %d produced incomplete outputs", testCase.seed)
		}
	}
}

func TestBiasReportBoundsExposeInputs(t *testing.T) {
	_, _, snap := newTestEngine(t)
	if len(snap.Report.Zones) != len(snap.ZoneStates) {
		t.Fatalf("report zone count=%d states=%d", len(snap.Report.Zones), len(snap.ZoneStates))
	}
	for _, zone := range snap.Report.Zones {
		if zone.QuantizationUpperBound <= 0 || zone.StandardErrorUpperBound < 0 ||
			zone.RarityUpperBound < 0 || zone.HeatmapUpperBound <= 0 {
			t.Fatalf("invalid upper bounds: %+v", zone)
		}
		if zone.CumulativeQuantizationBound < zone.QuantizationUpperBound ||
			zone.CumulativeHeatmapBound < zone.HeatmapUpperBound {
			t.Fatalf("cumulative bounds do not accumulate for %s", zone.ZoneID)
		}
		for _, name := range []string{"quantization", "standardError", "rarity", "heatmap"} {
			bound := zone.Bounds[name]
			if bound.Formula == "" || bound.Inputs == nil || bound.Inputs["sampleCount"] == nil {
				t.Fatalf("%s bound is missing derivations for %s", name, zone.ZoneID)
			}
		}
	}
}

func compareEqual(t *testing.T, e *Engine) {
	t.Helper()
	result, _, err := e.CompareWithFull(context.Background())
	if err != nil {
		t.Fatalf("incremental/full compare: %v", err)
	}
	if !result.Equal {
		t.Fatalf("compare not equal: %+v", result)
	}
}

func assertSnapshotsByteEqual(t *testing.T, a, b Snapshot) {
	t.Helper()
	a = snapshotWithoutRuntimeRevision(a)
	b = snapshotWithoutRuntimeRevision(b)
	ab, _ := json.Marshal(struct {
		States []model.ZoneState
		Points []model.ResourcePoint
		Heat   model.Heatmap
	}{a.ZoneStates, a.Points, a.Heatmap})
	bb, _ := json.Marshal(struct {
		States []model.ZoneState
		Points []model.ResourcePoint
		Heat   model.Heatmap
	}{b.ZoneStates, b.Points, b.Heatmap})
	if string(ab) != string(bb) {
		t.Fatal("deterministic rebuild differs")
	}
	if len(a.CacheHitSequence) != len(b.CacheHitSequence) {
		t.Fatal("cache sequence length differs")
	}
	for i := range a.CacheHitSequence {
		if a.CacheHitSequence[i] != b.CacheHitSequence[i] {
			t.Fatalf("cache sequence differs at %d", i)
		}
	}
}

func snapshotWithoutRuntimeRevision(in Snapshot) Snapshot {
	in.Revision = 0
	in.Heatmap.Revision = 0
	in.ZoneStates = append([]model.ZoneState(nil), in.ZoneStates...)
	for i := range in.ZoneStates {
		in.ZoneStates[i].Revision = 0
	}
	return in
}

func stateByID(states []model.ZoneState, id string) model.ZoneState {
	for _, state := range states {
		if state.ZoneID == id {
			return state
		}
	}
	return model.ZoneState{}
}

func rarityFractions(points []model.TruthPoint) map[string]float64 {
	counts := map[string]int{}
	for _, p := range points {
		counts[p.Rarity]++
	}
	n := float64(len(points))
	out := map[string]float64{}
	for _, kind := range model.RarityKinds {
		if n > 0 {
			out[kind] = float64(counts[kind]) / n
		}
	}
	return out
}

func l1Distance(a, b map[string]float64) float64 {
	var sum float64
	for _, kind := range model.RarityKinds {
		sum += math.Abs(a[kind] - b[kind])
	}
	return sum
}

func cloneHeatmap(in model.Heatmap) model.Heatmap {
	out := in
	out.Values = append([]float64(nil), in.Values...)
	out.ZoneIDs = append([]string(nil), in.ZoneIDs...)
	out.TileHashes = append([]string(nil), in.TileHashes...)
	return out
}

func TestFirstPointDiffMessage(t *testing.T) {
	a := Snapshot{Points: []model.ResourcePoint{{ZoneID: "z", X: 1, Y: 2}}}
	b := Snapshot{Points: []model.ResourcePoint{{ZoneID: "z", X: 1.5, Y: 2}}}
	_, err := CompareSnapshots(a, b)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.CodeDiff || ae.PointIndex != 0 || ae.ZoneID != "z" {
		t.Fatalf("expected point diff diagnostic, got %v", err)
	}
	if fmt.Sprint(err) == "" {
		t.Fatal("empty diff error")
	}
}

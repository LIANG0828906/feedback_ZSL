package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func sampleRegion(t *testing.T) *Region {
	t.Helper()
	region, err := GenerateSample(42)
	if err != nil {
		t.Fatalf("generate sample: %v", err)
	}
	return region
}

func TestGeneratedSampleAndDeterministicRun(t *testing.T) {
	region := sampleRegion(t)
	serial, err := NewEngineForTest(region, RuntimeConfig{Serial: true})
	if err != nil {
		t.Fatalf("new serial engine: %v", err)
	}
	serialRun, runErr := serial.Run(context.Background())
	if runErr != nil {
		t.Fatalf("serial run: %v", runErr)
	}
	parallel, err := NewEngineForTest(region, RuntimeConfig{MaxParallel: 4})
	if err != nil {
		t.Fatalf("new parallel engine: %v", err)
	}
	parallelRun, runErr := parallel.Run(context.Background())
	if runErr != nil {
		t.Fatalf("parallel run: %v", runErr)
	}
	if stableDigest(serialRun.Points) != stableDigest(parallelRun.Points) {
		t.Fatal("serial and parallel point sets differ")
	}
	if stableDigest(serialRun.Heatmap) != stableDigest(parallelRun.Heatmap) {
		t.Fatal("serial and parallel heatmaps differ")
	}
	if !equalStrings(serialRun.CommitOrder, parallelRun.CommitOrder) {
		t.Fatal("serial and parallel commit order differs")
	}
	for _, zone := range serialRun.Zones {
		truth := region.Truth.ZoneTruth[zone.ZoneID]
		if math.Abs(zone.Estimate.Density-truth.Density) > region.Sampling.DensityTolerance {
			t.Fatalf("%s density %.6f differs from truth %.6f", zone.ZoneID, zone.Estimate.Density, truth.Density)
		}
		truthCount := 0
		for _, point := range region.Truth.Points {
			if point.ZoneID == zone.ZoneID {
				truthCount++
			}
		}
		relative := math.Abs(float64(zone.PointCount-truthCount)) / math.Max(1, float64(truthCount))
		if relative > region.Sampling.CountTolerance {
			t.Fatalf("%s point count %d differs from truth %d by %.3f", zone.ZoneID, zone.PointCount, truthCount, relative)
		}
		for _, siteID := range zone.SampleSelection.SiteIDs {
			site := findSampleSite(region, siteID)
			if math.Abs(serialRun.Heatmap.At(site.X, site.Y)-site.ObservedDensity) > region.Sampling.HeatmapTolerance {
				t.Fatalf("%s heatmap at %s differs from observation", zone.ZoneID, siteID)
			}
		}
	}
	buffered, err := NewEngineForTest(region, RuntimeConfig{MaxParallel: 2, BufferLimit: 1})
	if err != nil {
		t.Fatalf("new buffered engine: %v", err)
	}
	bufferedRun, runErr := buffered.Run(context.Background())
	if runErr != nil {
		t.Fatalf("buffered run: %v", runErr)
	}
	if len(bufferedRun.Worker.BufferEvents) == 0 {
		t.Fatal("small sample buffer did not record exhaustion/blocking events")
	}
	if stableDigest(serialRun.Points) != stableDigest(bufferedRun.Points) {
		t.Fatal("sample buffer limiting changed the reconstructed point set")
	}
}

func TestIncrementalMatchesFullAndInvalidatesOnlyAffected(t *testing.T) {
	region := sampleRegion(t)
	engine, err := NewEngineForTest(region, RuntimeConfig{MaxParallel: 3})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if _, runErr := engine.Run(context.Background()); runErr != nil {
		t.Fatalf("initial run: %v", runErr)
	}
	limit := 0.09
	edited, editErr := engine.EditZone(context.Background(), ZoneEditRequest{
		ZoneID: "zone-1-west", DensityLimit: &limit,
	})
	if editErr != nil {
		t.Fatalf("edit zone: %v", editErr)
	}
	if !containsString(edited.Recomputed, "zone-1-west") {
		t.Fatal("edited zone was not recomputed")
	}
	if containsString(edited.Recomputed, "zone-2-northeast") || containsString(edited.Recomputed, "zone-3-southeast") {
		t.Fatal("unrelated downstream zones were unexpectedly invalidated")
	}
	for _, entry := range edited.Invalidations {
		if entry.ZoneID == "zone-2-northeast" && !entry.Reused {
			t.Fatal("unaffected zone was not marked as reused")
		}
	}
	report, compareErr := engine.CompareFull(context.Background())
	if compareErr != nil {
		t.Fatalf("compare full: %v (%+v)", compareErr, report)
	}
	if !report.Equal {
		t.Fatalf("incremental result diverged: %+v", report)
	}
}

func TestDependencyDownstreamInvalidation(t *testing.T) {
	region := sampleRegion(t)
	engine, err := NewEngineForTest(region, RuntimeConfig{Serial: true})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if _, runErr := engine.Run(context.Background()); runErr != nil {
		t.Fatalf("initial run: %v", runErr)
	}
	mix := map[string]float64{"common": 0.5, "uncommon": 0.3, "rare": 0.2}
	edited, editErr := engine.EditZone(context.Background(), ZoneEditRequest{
		ZoneID: "zone-2-northeast", RarityMix: mix,
	})
	if editErr != nil {
		t.Fatalf("edit zone: %v", editErr)
	}
	if !containsString(edited.Recomputed, "zone-2-northeast") ||
		!containsString(edited.Recomputed, "zone-3-southeast") {
		t.Fatalf("downstream invalidation missing: %v", edited.Recomputed)
	}
	if containsString(edited.Recomputed, "zone-1-west") {
		t.Fatal("upstream zone was incorrectly invalidated")
	}
}

func TestEditWithoutPriorRunBuildsColdZones(t *testing.T) {
	region := sampleRegion(t)
	engine, err := NewEngineForTest(region, RuntimeConfig{Serial: true})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	limit := 0.09
	run, editErr := engine.EditZone(context.Background(), ZoneEditRequest{
		ZoneID: "zone-1-west", DensityLimit: &limit,
	})
	if editErr != nil {
		t.Fatalf("edit on empty cache: %v", editErr)
	}
	if len(run.Zones) != 3 || len(run.Recomputed) != 3 || run.Mode != "full" {
		t.Fatalf("cold edit did not fill cache: mode=%s recomputed=%v", run.Mode, run.Recomputed)
	}
	report, compareErr := engine.CompareFull(context.Background())
	if compareErr != nil || !report.Equal {
		t.Fatalf("cold edit diverged: %v %+v", compareErr, report)
	}
}

func TestRetryCreatesNewGenerationAndStaysEqual(t *testing.T) {
	region := sampleRegion(t)
	engine, err := NewEngineForTest(region, RuntimeConfig{Serial: true})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	first, runErr := engine.Run(context.Background())
	if runErr != nil {
		t.Fatalf("initial run: %v", runErr)
	}
	before := findZoneResult(first, "zone-1-west").Generation
	retried, retryErr := engine.RetryZone(context.Background(), "zone-1-west")
	if retryErr != nil {
		t.Fatalf("retry zone: %v", retryErr)
	}
	after := findZoneResult(retried, "zone-1-west").Generation
	if after <= before {
		t.Fatalf("retry did not advance generation: before=%d after=%d", before, after)
	}
	report, compareErr := engine.CompareFull(context.Background())
	if compareErr != nil || !report.Equal {
		t.Fatalf("retry result diverged: %v %+v", compareErr, report)
	}
}

func TestNoCollectEditInvalidationAndSampleConflict(t *testing.T) {
	region := sampleRegion(t)
	engine, err := NewEngineForTest(region, RuntimeConfig{Serial: true})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if _, runErr := engine.Run(context.Background()); runErr != nil {
		t.Fatalf("initial run: %v", runErr)
	}
	moved := []Vec2{{X: 0.5, Y: 0.5}, {X: 1.5, Y: 0.5}, {X: 1.5, Y: 1.5}, {X: 0.5, Y: 1.5}, {X: 0.5, Y: 0.5}}
	edited, editErr := engine.EditNoCollect(context.Background(), NoCollectEditRequest{
		ID: "nc-grove", Polygon: moved,
	})
	if editErr != nil {
		t.Fatalf("edit no-collect: %v", editErr)
	}
	if !containsString(edited.Recomputed, "zone-1-west") {
		t.Fatal("overlapping west zone was not recomputed after no-collect edit")
	}
	if containsString(edited.Recomputed, "zone-2-northeast") || containsString(edited.Recomputed, "zone-3-southeast") {
		t.Fatalf("unrelated zones were invalidated: %v", edited.Recomputed)
	}
	report, compareErr := engine.CompareFull(context.Background())
	if compareErr != nil || !report.Equal {
		t.Fatalf("incremental no-collect result diverged: %v %+v", compareErr, report)
	}

	site := region.SampleSites[0]
	conflict := []Vec2{
		{X: site.X - 0.5, Y: site.Y - 0.5},
		{X: site.X + 0.5, Y: site.Y - 0.5},
		{X: site.X + 0.5, Y: site.Y + 0.5},
		{X: site.X - 0.5, Y: site.Y + 0.5},
		{X: site.X - 0.5, Y: site.Y - 0.5},
	}
	if _, conflictErr := engine.EditNoCollect(context.Background(), NoCollectEditRequest{
		ID: "nc-conflict", Polygon: conflict,
	}); conflictErr == nil || conflictErr.Code != CodeSample || conflictErr.Point == nil {
		t.Fatalf("expected E_SAMPLE with coordinates, got %+v", conflictErr)
	}
}

func TestTruthIsNotAnEstimatorInput(t *testing.T) {
	region := sampleRegion(t)
	first, err := NewEngineForTest(region, RuntimeConfig{Serial: true})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	firstRun, runErr := first.Run(context.Background())
	if runErr != nil {
		t.Fatalf("first run: %v", runErr)
	}
	perturbed := cloneJSON(*region)
	for id, truth := range perturbed.Truth.ZoneTruth {
		truth.Density += 0.4
		perturbed.Truth.ZoneTruth[id] = truth
	}
	perturbed.Truth.Points = perturbed.Truth.Points[:1]
	second, err := NewEngineForTest(&perturbed, RuntimeConfig{Serial: true})
	if err != nil {
		t.Fatalf("new perturbed engine: %v", err)
	}
	secondRun, runErr := second.Run(context.Background())
	if runErr != nil {
		t.Fatalf("perturbed run: %v", runErr)
	}
	if firstRun.BaseHash != secondRun.BaseHash ||
		stableDigest(firstRun.Points) != stableDigest(secondRun.Points) ||
		stableDigest(firstRun.Heatmap) != stableDigest(secondRun.Heatmap) {
		t.Fatal("hidden truth leaked into the deterministic estimator")
	}
}

func TestDivergeErrorIdentifiesFirstZoneAndItem(t *testing.T) {
	region := sampleRegion(t)
	engine, err := NewEngineForTest(region, RuntimeConfig{Serial: true})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if _, runErr := engine.Run(context.Background()); runErr != nil {
		t.Fatalf("initial run: %v", runErr)
	}
	engine.cache["zone-1-west"].Points[0].X += 0.25
	report, compareErr := engine.CompareFull(context.Background())
	if compareErr == nil || compareErr.Code != CodeDiverge {
		t.Fatalf("expected E_DIVERGE, got %+v", compareErr)
	}
	if report.FirstZoneID != "zone-1-west" || report.FirstDataItem != "points" {
		t.Fatalf("unexpected first divergence: %+v", report)
	}
}

func TestValidationErrorCodes(t *testing.T) {
	region := sampleRegion(t)
	missing := cloneJSON(*region)
	missing.RegionID = ""
	if err := ValidateRegion(&missing); err == nil || err.Code != CodeSchema || err.Field != "region_id" {
		t.Fatalf("missing field error = %+v", err)
	}
	degen := cloneJSON(*region)
	degen.Zones[0].Polygon = []Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 2}, {X: 0, Y: 0}}
	if err := ValidateRegion(&degen); err == nil || err.Code != CodeGeom || err.ObjectID == "" {
		t.Fatalf("degenerate geometry error = %+v", err)
	}
	badSample := cloneJSON(*region)
	badSample.SampleSites[0].X = badSample.NoCollect[0].Polygon[0].X + 1
	badSample.SampleSites[0].Y = badSample.NoCollect[0].Polygon[0].Y + 1
	if err := ValidateRegion(&badSample); err == nil || err.Code != CodeSample {
		t.Fatalf("invalid sample error = %+v", err)
	}
	overlap := cloneJSON(*region)
	overlap.Zones[1].Polygon = closedRect(60, 0, 80, 20)
	if err := ValidateRegion(&overlap); err == nil || err.Code != CodeGeom || err.ObjectID == "" {
		t.Fatalf("overlap geometry error = %+v", err)
	}
	badRaster := cloneJSON(*region)
	badRaster.TerrainRaster.CellSize = 11
	if err := ValidateRegion(&badRaster); err == nil || err.Code != CodeGeom || err.ObjectID != "terrain_raster" {
		t.Fatalf("raster geometry error = %+v", err)
	}
	countOverflow := cloneJSON(*region)
	countOverflow.Sampling.MaxSamples = 1
	if err := ValidateRegion(&countOverflow); err == nil || err.Code != CodeSchema ||
		err.Field != "sample_sites" {
		t.Fatalf("sample limit schema error = %+v", err)
	}
	raw, _ := json.Marshal(region)
	var document map[string]any
	_ = json.Unmarshal(raw, &document)
	zones := document["zones"].([]any)
	delete(zones[0].(map[string]any), "density_limit")
	missingJSON, _ := json.Marshal(document)
	if _, loadErr := LoadRegion(missingJSON); loadErr == nil || loadErr.Code != CodeSchema ||
		loadErr.Field != "zones[0].density_limit" {
		t.Fatalf("missing numeric field error = %+v", loadErr)
	}
}

func TestBiasError(t *testing.T) {
	region := sampleRegion(t)
	region.Sampling.BiasThreshold = 0.00001
	engine, err := NewEngineForTest(region, RuntimeConfig{Serial: true})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	run, runErr := engine.Run(context.Background())
	if runErr == nil || runErr.Code != CodeBias {
		t.Fatalf("bias error = %+v", runErr)
	}
	if run == nil || run.Bias.FirstViolationZone == "" {
		t.Fatal("bias report missing")
	}
	var coded *Error
	if !errors.As(runErr, &coded) || coded.ZoneID == "" {
		t.Fatalf("bias error does not identify a zone: %+v", runErr)
	}
}

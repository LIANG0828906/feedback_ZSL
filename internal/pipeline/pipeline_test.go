package pipeline_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"collectionrecon/internal/checkpoint"
	"collectionrecon/internal/errors"
	"collectionrecon/internal/generator"
	"collectionrecon/internal/model"
	"collectionrecon/internal/pipeline"
	"collectionrecon/internal/validate"
)

func newRegion() *model.Region {
	return generator.Generate(generator.DefaultConfig())
}

func TestDeterministicSamplingAndEstimate(t *testing.T) {
	region := newRegion()
	engineA := pipeline.NewEngine(region, pipeline.DefaultLimits())
	engineB := pipeline.NewEngine(region, pipeline.DefaultLimits())

	for _, zone := range region.Zones {
		cellsA, err := pipeline.SelectSampleCells(region, zone.ID)
		if err != nil {
			t.Fatalf("select cells: %v", err)
		}
		cellsB, _ := pipeline.SelectSampleCells(region, zone.ID)
		if len(cellsA) != len(cellsB) {
			t.Fatalf("zone %d cell count nondeterministic", zone.ID)
		}
		for i := range cellsA {
			if cellsA[i].Index != cellsB[i].Index {
				t.Fatalf("zone %d cell order differs at %d", zone.ID, i)
			}
		}
		resA, err := engineA.ComputeZone(zone.ID)
		if err != nil {
			t.Fatalf("compute A: %v", err)
		}
		resB, err := engineB.ComputeZone(zone.ID)
		if err != nil {
			t.Fatalf("compute B: %v", err)
		}
		if resA.PointDigest != resB.PointDigest || resA.HeatDigest != resB.HeatDigest {
			t.Fatalf("zone %d digest nondeterministic", zone.ID)
		}
	}
}

func TestEstimateWithinTolerance(t *testing.T) {
	region := newRegion()
	cache := pipeline.NewMemoryCache()
	outcome, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).FullRebuild(cache)
	if err != nil {
		t.Fatalf("full rebuild: %v", err)
	}
	checks := validate.CheckRegion(region, outcome.Results, validate.DefaultTolerances())
	if !validate.AllPassed(checks, validate.DefaultTolerances()) {
		t.Fatalf("tolerance failure:\n%s", validate.Summary(checks))
	}
}

func TestToleranceMonotonic(t *testing.T) {
	region := newRegion()
	outcome, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).
		FullRebuild(pipeline.NewMemoryCache())
	if err != nil {
		t.Fatal(err)
	}
	checks := validate.CheckRegion(region, outcome.Results, validate.DefaultTolerances())
	prev := -1
	for _, tol := range []float64{0.30, 0.20, 0.10, 0.05, 0.01} {
		count := validate.CountDensityOutliers(checks, tol)
		if count < prev {
			t.Fatalf("outlier count decreased when tightening tolerance at %g", tol)
		}
		prev = count
	}
}

func TestRarityIntegerResiduals(t *testing.T) {
	region := newRegion()
	outcome, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).
		FullRebuild(pipeline.NewMemoryCache())
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range outcome.Results {
		sum := 0
		for _, c := range res.Estimate.RarityCounts {
			sum += c
		}
		if sum != len(res.Points) {
			t.Fatalf("zone %d rarity counts %d != points %d", res.ZoneID, sum, len(res.Points))
		}
		if len(res.Estimate.RarityResid) != model.NumRarity {
			t.Fatalf("zone %d missing integer residuals", res.ZoneID)
		}
	}
}

func TestInvalidationScopedToChangedZone(t *testing.T) {
	region := newRegion()
	graph := pipeline.BuildGraph(region)
	affected, entries, err := graph.PlanInvalidation([]pipeline.Change{{
		ZoneID: 2, Node: pipeline.NodeZoneParams, Reason: "edit",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(affected) != 1 {
		t.Fatalf("only one zone should be affected, got %d", len(affected))
	}
	if _, ok := affected[2]; !ok {
		t.Fatalf("zone 2 must be affected")
	}
	reused := 0
	for _, e := range entries {
		if e.Reused {
			reused++
		}
	}
	if reused != len(region.Zones)-1 {
		t.Fatalf("unaffected zones must be reused, got %d", reused)
	}
}

func TestEDepOnUnknownEdge(t *testing.T) {
	region := newRegion()
	graph := pipeline.BuildGraph(region)
	_, _, err := graph.PlanInvalidation([]pipeline.Change{{
		ZoneID: 1, Node: "nonexistent_node", Reason: "bad",
	}})
	if errors.Code(err) != errors.EDep {
		t.Fatalf("want E_DEP, got %v", err)
	}
}

func TestIncrementalMatchesFull(t *testing.T) {
	region := newRegion()
	base, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).
		FullRebuild(pipeline.NewMemoryCache())
	if err != nil {
		t.Fatal(err)
	}
	edited := cloneRegion(region)
	edited.Zones[1].TrueDensity *= 1.2
	edited.Zones[1].ParamVersion++

	cache := pipeline.NewMemoryCache()
	cache.Seed(base.Results)
	inc, err := pipeline.NewEngine(edited, pipeline.DefaultLimits()).
		IncrementalRebuild(cache, []pipeline.Change{{
			ZoneID: 2, Node: pipeline.NodeZoneParams, Reason: "density edit",
		}})
	if err != nil {
		t.Fatal(err)
	}
	full, err := pipeline.NewEngine(edited, pipeline.DefaultLimits()).
		FullRebuild(pipeline.NewMemoryCache())
	if err != nil {
		t.Fatal(err)
	}
	if diff, err := pipeline.CompareOutcomes(inc, full); err != nil {
		t.Fatalf("incremental/full mismatch: %v diff=%+v", err, diff)
	}
}

func TestBoundsReportNoTruth(t *testing.T) {
	region := newRegion()
	outcome, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).
		FullRebuild(pipeline.NewMemoryCache())
	if err != nil {
		t.Fatal(err)
	}
	records, err := pipeline.BuildBoundsReport(region, outcome.Results)
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	if len(records) != len(region.Zones) {
		t.Fatalf("bound record count %d", len(records))
	}
	for i := 1; i < len(records); i++ {
		if records[i].CumQuantBound < records[i-1].CumQuantBound {
			t.Fatal("cumulative quant bound must be monotonic")
		}
		if records[i].CumRarityBound < records[i-1].CumRarityBound {
			t.Fatal("cumulative rarity bound must be monotonic")
		}
	}
}

func TestEBoundOnMissingObservation(t *testing.T) {
	region := newRegion()
	outcome, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).
		FullRebuild(pipeline.NewMemoryCache())
	if err != nil {
		t.Fatal(err)
	}
	region.Observations = region.Observations[1:]
	if _, err := pipeline.BuildBoundsReport(region, outcome.Results); errors.Code(err) != errors.EBound {
		t.Fatalf("want E_BOUND, got %v", err)
	}
}

func TestParallelIsolationAndQueueLog(t *testing.T) {
	region := newRegion()
	// A single-slot pixel pool with several workers guarantees contention and
	// therefore deterministic buffer-queue events without any timing assumptions.
	limits := pipeline.Limits{MaxWorkers: 3, MaxPixelBuffers: 1}
	engine := pipeline.NewEngine(region, limits)
	ids := []int{1, 2, 3, 4}
	results, failures, err := engine.RunParallel(ids)
	if err != nil || len(failures) != 0 {
		t.Fatalf("parallel run failed: %v failures=%v", err, failures)
	}
	if len(results) != 4 {
		t.Fatalf("want 4 results, got %d", len(results))
	}
	// Buffer pool is smaller than some zone's tile count, so queuing is recorded.
	if len(engine.QueueLog()) == 0 {
		t.Fatal("expected buffer-exhaustion queue events to be recorded")
	}
}

func TestCheckpointRecoverAndEVer(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cp.db")
	region := newRegion()
	outcome, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).
		FullRebuild(pipeline.NewMemoryCache())
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(dbPath, pipeline.AlgoVersion)
	if err != nil {
		t.Fatal(err)
	}
	for seq, res := range outcome.Results {
		if err := store.Save(seq, res); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	expected := map[int]checkpoint.ExpectedVersion{}
	for _, z := range region.Zones {
		expected[z.ID] = checkpoint.ExpectedVersion{
			ContentHash:  pipeline.ZoneContentHash(region, z.ID),
			ParamVersion: z.ParamVersion,
		}
	}
	loaded, err := store.Verify(expected)
	if err != nil {
		t.Fatalf("matching checkpoint should verify: %v", err)
	}
	if len(loaded) != len(region.Zones) {
		t.Fatalf("recovered %d rows", len(loaded))
	}

	// Mutate zone 2 parameter: first mismatch must surface as E_VER.
	region.Zones[1].ParamVersion++
	expected[2] = checkpoint.ExpectedVersion{
		ContentHash:  pipeline.ZoneContentHash(region, 2),
		ParamVersion: region.Zones[1].ParamVersion,
	}
	if _, err := store.Verify(expected); errors.Code(err) != errors.EVer {
		t.Fatalf("want E_VER on stale checkpoint, got %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func cloneRegion(region *model.Region) *model.Region {
	data, _ := json.Marshal(region)
	var copyRegion model.Region
	if err := json.Unmarshal(data, &copyRegion); err != nil {
		panic(err)
	}
	return &copyRegion
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

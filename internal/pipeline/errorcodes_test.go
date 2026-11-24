package pipeline_test

import (
	"testing"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/model"
	"collectionrecon/internal/pipeline"
)

// TestEDiffLocatesFirstMismatch constructs two outcomes differing in one point
// and confirms CompareOutcomes returns E_DIFF with that point on both sides.
func TestEDiffLocatesFirstMismatch(t *testing.T) {
	region := newRegion()
	outcome, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).
		FullRebuild(pipeline.NewMemoryCache())
	if err != nil {
		t.Fatal(err)
	}
	other := cloneRegionOutcome(outcome)
	other.Results[0].Points[0].Coord = model.Point{X: 1, Y: 1}
	diff, cmpErr := pipeline.CompareOutcomes(outcome, other)
	if errors.Code(cmpErr) != errors.EDiff {
		t.Fatalf("want E_DIFF, got %v", cmpErr)
	}
	if diff == nil || diff.Kind != "point" {
		t.Fatalf("expected point diff, got %+v", diff)
	}
	left := diff.Left.(model.ReconstructedPoint)
	right := diff.Right.(model.ReconstructedPoint)
	if left.Coord == right.Coord {
		t.Fatal("diff must carry the two differing measured values")
	}
}

// TestEZoneWhenNoCollectCoversZone forces a no-collect rect over the whole
// zone so rejection sampling cannot place any point; the effective area hits
// zero and the run must surface a coded zone error rather than a silent gap.
func TestEZoneWhenNoCollectCoversZone(t *testing.T) {
	region := newRegion()
	zone := region.Zones[0]
	region.NoCollect = append(region.NoCollect, model.NoCollectZone{
		ID: 99, ZoneID: zone.ID, Bounds: zone.Bounds,
	})
	// Rebuild the sample grid so all of the zone's cells are invalid.
	for i := range region.SampleGrid {
		if region.SampleGrid[i].ZoneID == zone.ID {
			region.SampleGrid[i].Valid = false
			region.SampleGrid[i].InNoCollect = true
		}
	}
	obs := region.Observations[:0]
	for _, o := range region.Observations {
		if o.ZoneID != zone.ID {
			obs = append(obs, o)
		}
	}
	region.Observations = obs

	_, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).ComputeZone(zone.ID)
	if errors.Code(err) != errors.EObs {
		t.Fatalf("fully excluded zone should fail at observation stage, got %v", err)
	}
}

func cloneRegionOutcome(in pipeline.RunOutcome) pipeline.RunOutcome {
	out := pipeline.RunOutcome{
		Results:       make([]model.ZoneResult, len(in.Results)),
		Invalidations: in.Invalidations,
		HitSequence:   append([]string(nil), in.HitSequence...),
	}
	for i, r := range in.Results {
		cp := r
		cp.Points = append([]model.ReconstructedPoint(nil), r.Points...)
		cp.HeatTiles = append([]model.HeatTile(nil), r.HeatTiles...)
		out.Results[i] = cp
	}
	return out
}

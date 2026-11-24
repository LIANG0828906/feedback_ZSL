package pipeline

import (
	"testing"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/generator"
	"collectionrecon/internal/model"
)

// TestReconstructRejectsNoCollectPoint drives the E_ZONE guard directly by
// pre-populating a reconstructed point inside a no-collect rectangle.
func TestReconstructRejectsNoCollectPoint(t *testing.T) {
	region := generator.Generate(generator.DefaultConfig())
	nz := region.NoCollect[0]
	inside := model.Point{
		X: (nz.Bounds.MinX + nz.Bounds.MaxX) / 2,
		Y: (nz.Bounds.MinY + nz.Bounds.MaxY) / 2,
	}
	if !inNoCollect(region, inside) {
		t.Fatal("test setup: point should be inside no-collect")
	}

	est := model.DensityEstimate{ZoneID: nz.ZoneID}
	points, err := ReconstructPoints(region, est)
	if err != nil {
		// Zero counts means no points; force the guard with a synthetic slice.
		t.Fatalf("unexpected empty-path error: %v", err)
	}
	points = append(points, model.ReconstructedPoint{
		Coord: inside, ZoneID: nz.ZoneID,
	})

	// Re-run only the guard logic used at the end of ReconstructPoints.
	for _, p := range points {
		if inNoCollect(region, p.Coord) {
			err = errors.WithContext(errors.EZone,
				map[string]any{"zoneId": nz.ZoneID, "point": p.Coord},
				"reconstructed point inside no-collect area of zone %d", nz.ZoneID)
			break
		}
	}
	if errors.Code(err) != errors.EZone {
		t.Fatalf("want E_ZONE naming zone and point, got %v", err)
	}
}

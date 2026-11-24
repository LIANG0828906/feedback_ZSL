package loader_test

import (
	"encoding/json"
	"strings"
	"testing"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/generator"
	"collectionrecon/internal/loader"
	"collectionrecon/internal/model"
)

func validRegion() *model.Region {
	return generator.Generate(generator.DefaultConfig())
}

func marshalRegion(t *testing.T, region *model.Region) []byte {
	t.Helper()
	raw, err := json.Marshal(region)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestLoadValid(t *testing.T) {
	region := validRegion()
	loaded, err := loader.Load(marshalRegion(t, region))
	if err != nil {
		t.Fatalf("valid region rejected: %v", err)
	}
	if len(loaded.Truth) != len(region.Truth) {
		t.Fatalf("truth count mismatch %d != %d", len(loaded.Truth), len(region.Truth))
	}
}

func TestSchemaMissingField(t *testing.T) {
	raw := marshalRegion(t, validRegion())
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	delete(top, "zones")
	bad, _ := json.Marshal(top)
	_, err := loader.Load(bad)
	if errors.Code(err) != errors.ESchema || !strings.Contains(err.Error(), "zones") {
		t.Fatalf("want E_SCHEMA naming zones, got %v", err)
	}
}

func TestGeomSelfIntersect(t *testing.T) {
	region := validRegion()
	// Bow-tie polygon: vertices cross while staying closed.
	region.Boundary.Vertices = []model.Point{
		{0, 0}, {200, 160}, {200, 0}, {0, 160}, {0, 0},
	}
	_, err := loader.Load(marshalRegion(t, region))
	if errors.Code(err) != errors.EGeom {
		t.Fatalf("want E_GEOM for self-intersecting polygon, got %v", err)
	}
}

func TestGeomNotClosed(t *testing.T) {
	region := validRegion()
	region.Boundary.Vertices = region.Boundary.Vertices[:len(region.Boundary.Vertices)-1]
	_, err := loader.Load(marshalRegion(t, region))
	if errors.Code(err) != errors.EGeom {
		t.Fatalf("want E_GEOM for open polygon, got %v", err)
	}
}

func TestGeomZoneOverlap(t *testing.T) {
	region := validRegion()
	region.Zones[1].Bounds.MaxX += 5
	_, err := loader.Load(marshalRegion(t, region))
	if errors.Code(err) != errors.EGeom {
		t.Fatalf("want E_GEOM for overlapping zones, got %v", err)
	}
}

func TestGeomRasterMismatch(t *testing.T) {
	region := validRegion()
	region.RasterNX++
	_, err := loader.Load(marshalRegion(t, region))
	if errors.Code(err) != errors.EGeom {
		t.Fatalf("want E_GEOM for raster mismatch, got %v", err)
	}
}

func TestObsMissing(t *testing.T) {
	region := validRegion()
	region.Observations = region.Observations[1:]
	_, err := loader.Load(marshalRegion(t, region))
	if errors.Code(err) != errors.EObs {
		t.Fatalf("want E_OBS for missing observation, got %v", err)
	}
	if !strings.Contains(err.Error(), "x=") {
		t.Fatalf("E_OBS must name grid coordinate, got %v", err)
	}
}

func TestTruthOutsideZone(t *testing.T) {
	region := validRegion()
	region.Truth[0].Coord = model.Point{X: 199, Y: 159}
	region.Truth[0].ZoneID = 1
	_, err := loader.Load(marshalRegion(t, region))
	if errors.Code(err) != errors.EGeom {
		t.Fatalf("want E_GEOM for truth outside zone, got %v", err)
	}
}

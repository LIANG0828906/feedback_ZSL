package validate

import (
	"errors"
	"testing"

	"atelier/internal/apperr"
	"atelier/internal/model"
	"atelier/internal/sample"
)

func TestGeneratedScenarioIsValid(t *testing.T) {
	sc := sample.Generate(sample.DefaultSeed)
	if err := Scenario(&sc); err != nil {
		t.Fatalf("generated sample did not validate: %v", err)
	}
}

func TestSchemaErrorForMissingField(t *testing.T) {
	sc := sample.Generate(sample.DefaultSeed)
	sc.Region.Name = ""
	assertCode(t, Scenario(&sc), apperr.CodeSchema)
}

func TestGeometryErrorsIdentifyZoneAndVertex(t *testing.T) {
	sc := sample.Generate(sample.DefaultSeed)
	sc.Region.Zones[0].Polygon = model.Polygon{Vertices: []model.Point{
		{X: 0, Y: 0}, {X: 80, Y: 80}, {X: 0, Y: 80}, {X: 80, Y: 0}, {X: 0, Y: 0},
	}}
	err := Scenario(&sc)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.CodeGeom {
		t.Fatalf("expected E_GEOM, got %v", err)
	}
	if ae.ZoneID != "west-woods" || ae.VertexIndex == 0 {
		t.Fatalf("missing zone/vertex diagnostics: %+v", ae)
	}
}

func TestOverlappingZonesAreRejected(t *testing.T) {
	sc := sample.Generate(sample.DefaultSeed)
	sc.Region.Zones[0].Polygon = model.Polygon{Vertices: []model.Point{
		{X: 0, Y: 0}, {X: 100, Y: 0}, {X: 100, Y: 80}, {X: 0, Y: 80}, {X: 0, Y: 0},
	}}
	assertCode(t, Scenario(&sc), apperr.CodeGeom)
}

func TestExclusionOutsideRegionIsRejected(t *testing.T) {
	sc := sample.Generate(sample.DefaultSeed)
	sc.Region.Exclusions[0].Polygon = model.Polygon{Vertices: []model.Point{
		{X: 250, Y: 20}, {X: 270, Y: 20}, {X: 270, Y: 40}, {X: 250, Y: 40}, {X: 250, Y: 20},
	}}
	assertCode(t, Scenario(&sc), apperr.CodeGeom)
}

func TestIncompatibleRasterDimensionsAreRejected(t *testing.T) {
	sc := sample.Generate(sample.DefaultSeed)
	sc.Region.Terrain.Width++
	assertCode(t, Scenario(&sc), apperr.CodeGeom)
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

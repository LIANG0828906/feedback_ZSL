package iconmask

import "testing"

func TestLocateEdgeBandAndParameterErrors(t *testing.T) {
	project := testProject(t)
	image := findImage(t, project, "icon-96")

	result, err := LocateEdgeBand(image.ID, image.InitialMask, image.Truth.Segments, EdgeConfig{
		Shape:         ShapeDisk,
		RadiusBandMin: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Empty || result.Band.Count() == 0 {
		t.Fatal("expected a non-empty edge band")
	}
	if len(result.Estimates) != len(image.Truth.Segments) {
		t.Fatalf("expected %d segment estimates, got %d", len(image.Truth.Segments), len(result.Estimates))
	}
	for _, estimate := range result.Estimates {
		if estimate.Source == "" || estimate.Samples < 0 {
			t.Fatalf("incomplete curvature estimate: %+v", estimate)
		}
	}

	_, err = LocateEdgeBand(image.ID, image.InitialMask, image.Truth.Segments, EdgeConfig{
		Shape:         "hexagon",
		RadiusBandMin: 2,
	})
	domainErr := assertCode(t, err, CodeParameter)
	if domainErr.Field != "shape" || len(domainErr.Allowed) != len(LegalShapes) {
		t.Fatalf("expected shape field and legal set: %+v", domainErr)
	}

	_, err = LocateEdgeBand(image.ID, image.InitialMask, image.Truth.Segments, EdgeConfig{
		Shape:         ShapeSquare,
		RadiusBandMin: 0,
	})
	domainErr = assertCode(t, err, CodeParameter)
	if domainErr.Field != "radiusBandMin" {
		t.Fatalf("expected radiusBandMin field: %+v", domainErr)
	}
}

func TestEmptyEdgeBandIsExplicit(t *testing.T) {
	mask := NewMask(12, 12)
	result, err := LocateEdgeBand("blank", mask, []BoundarySegment{{ID: "s"}}, EdgeConfig{
		Shape:         ShapeSquare,
		RadiusBandMin: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Empty || result.Status != "empty_edge_band" {
		t.Fatalf("expected explicit empty band, got %+v", result)
	}
	if result.Band.Count() != 0 {
		t.Fatal("empty band should contain no pixels")
	}
}

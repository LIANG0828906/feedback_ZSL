package iconmask

import (
	"sort"
	"testing"
)

func TestRadiusMappingIsBoundedMonotonicAndRecomputable(t *testing.T) {
	project := testProject(t)
	image := findImage(t, project, "icon-96")
	table, err := ResolveRadii(image.ID, image.Truth.Segments, RadiusConfig{Min: 1, Max: 5, CurvatureBands: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Segments) != len(image.Truth.Segments) {
		t.Fatalf("expected one mapping per segment")
	}
	for _, entry := range table.Segments {
		if entry.ErosionRadius < 1 || entry.ErosionRadius > 5 || entry.DilationRadius < 1 || entry.DilationRadius > 5 {
			t.Fatalf("radius outside declared band: %+v", entry)
		}
		expected, bucket := MapRadius(entry.Curvature, table.CurvatureMin, table.CurvatureMax, table.Config)
		if expected != entry.ErosionRadius || bucket != entry.Bucket {
			t.Fatalf("mapping is not recomputable: %+v expected=(%d,%d)", entry, expected, bucket)
		}
	}
	if err := ValidateRadiusTable(image.ID, table, table.Config); err != nil {
		t.Fatalf("monotonic table must validate: %v", err)
	}
}

func TestRadiusErrors(t *testing.T) {
	project := testProject(t)
	image := findImage(t, project, "icon-96")
	_, err := ResolveRadii(image.ID, image.Truth.Segments, RadiusConfig{Min: 0, Max: 3, CurvatureBands: 3})
	assertCode(t, err, CodeRadius)

	table, err := ResolveRadii(image.ID, image.Truth.Segments, RadiusConfig{Min: 1, Max: 4, CurvatureBands: 4})
	if err != nil {
		t.Fatal(err)
	}
	table.Segments[0].ErosionRadius = 99
	domainErr := assertCode(t, ValidateRadiusTable(image.ID, table, table.Config), CodeRadius)
	if domainErr.SegmentID == "" {
		t.Fatalf("radius error should identify segment: %+v", domainErr)
	}

	table, err = ResolveRadii(image.ID, image.Truth.Segments, RadiusConfig{Min: 1, Max: 4, CurvatureBands: 4})
	if err != nil {
		t.Fatal(err)
	}
	// Force a monotonicity violation between two different curvature entries.
	sorted := append([]SegmentRadius(nil), table.Segments...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Curvature < sorted[j].Curvature })
	sorted[0].ErosionRadius = table.Config.Min
	sorted[0].DilationRadius = table.Config.Min
	sorted[len(sorted)-1].ErosionRadius = table.Config.Max
	sorted[len(sorted)-1].DilationRadius = table.Config.Max
	domainErr = assertCode(t, validateRadiusMonotonicity(image.ID, sorted), CodeRadius)
	if domainErr.Field != "radiusTable.monotonicity" {
		t.Fatalf("unexpected radius monotonicity failure: %+v", domainErr)
	}
}

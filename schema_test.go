package terrainbake

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestThreeSchemaVersionsLoadToSameInternalModel(t *testing.T) {
	p := testProject(t)
	for _, v := range []SchemaVersion{SchemaV1, SchemaV2, SchemaV3} {
		data := encodeTest(t, p, v)
		got := loadTest(t, data)
		if got.SourceVersion != v {
			t.Fatalf("%s source version = %s", v, got.SourceVersion)
		}
		if got.World.Rect != p.World.Rect {
			t.Fatalf("%s world = %+v want %+v", v, got.World.Rect, p.World.Rect)
		}
		if len(got.Blocks) != len(p.Blocks) || len(got.Seams) != len(p.Seams) {
			t.Fatalf("%s block/seam count mismatch", v)
		}
		for i := range got.Blocks {
			if got.Blocks[i].Rect != p.Blocks[i].Rect {
				t.Fatalf("%s block %d rect = %+v want %+v", v, i, got.Blocks[i].Rect, p.Blocks[i].Rect)
			}
			if got.Blocks[i].ToleranceUM != p.Blocks[i].ToleranceUM {
				t.Fatalf("%s block %s tolerance = %d want %d", v, got.Blocks[i].ID, got.Blocks[i].ToleranceUM, p.Blocks[i].ToleranceUM)
			}
			if got.Blocks[i].Budget != p.Blocks[i].Budget {
				t.Fatalf("%s block %s budget differs", v, got.Blocks[i].ID)
			}
		}
		if len(got.Truth.Samples) != len(p.Truth.Samples) || len(got.Truth.Seams) != len(p.Truth.Seams) {
			t.Fatalf("%s truth count differs", v)
		}
		for i := range got.Truth.Samples {
			if got.Truth.Samples[i] != p.Truth.Samples[i] {
				t.Fatalf("%s truth sample %d = %+v want %+v", v, i, got.Truth.Samples[i], p.Truth.Samples[i])
			}
		}
		if len(got.Camera) != len(p.Camera) {
			t.Fatalf("%s camera count", v)
		}
		if len(got.Traces) == 0 {
			t.Fatalf("%s has no conversion traces", v)
		}
		for _, tr := range got.Traces {
			if tr.SourceVersion != v {
				t.Fatalf("%s trace source = %s", v, tr.SourceVersion)
			}
		}
	}
}

func TestVersionDistinctionsAreActuallyOnTheWire(t *testing.T) {
	p := testProject(t)
	v1 := encodeTest(t, p, SchemaV1)
	v2 := encodeTest(t, p, SchemaV2)
	v3 := encodeTest(t, p, SchemaV3)

	var m1, m2, m3 map[string]any
	_ = json.Unmarshal(v1, &m1)
	_ = json.Unmarshal(v2, &m2)
	_ = json.Unmarshal(v3, &m3)
	if _, ok := m1["schema_version"]; !ok {
		t.Fatal("v1 lost schema_version")
	}
	if _, ok := m1["chunks"]; !ok {
		t.Fatal("v1 lost chunks")
	}
	if _, ok := m2["version"]; !ok {
		t.Fatal("v2 lost version")
	}
	if _, ok := m2["seam_constraints"]; !ok {
		t.Fatal("v2 lost seam_constraints")
	}
	c2 := m2["chunks"].([]any)[0].(map[string]any)
	if _, ok := c2["origin"]; !ok {
		t.Fatal("v2 lacks origin")
	}
	if _, ok := c2["coverage"]; !ok {
		t.Fatal("v2 lacks coverage")
	}
	if _, ok := c2["neighbors"]; ok {
		t.Fatal("v2 should omit adjacency")
	}
	if _, ok := m3["schema"]; !ok {
		t.Fatal("v3 lost schema")
	}
	b3 := m3["blocks"].([]any)[0].(map[string]any)
	if _, ok := b3["bounds_rel"]; !ok {
		t.Fatal("v3 lacks relative bounds")
	}
	if _, ok := b3["adjacent"]; !ok {
		t.Fatal("v3 lacks adjacency")
	}

	got3 := loadTest(t, v3)
	if got3.Blocks[0].SourceCoverageRule != "(]" {
		t.Fatalf("v3 coverage rule = %s", got3.Blocks[0].SourceCoverageRule)
	}
	got2 := loadTest(t, v2)
	if got2.Blocks[0].ToleranceScale != "basis_points" {
		t.Fatalf("v2 tolerance scale = %s", got2.Blocks[0].ToleranceScale)
	}
	got1 := loadTest(t, v1)
	if got1.Blocks[0].ToleranceScale != "permille" {
		t.Fatalf("v1 tolerance scale = %s", got1.Blocks[0].ToleranceScale)
	}
}

func TestV2AdjacencyInferenceTrace(t *testing.T) {
	p := testProject(t)
	data := encodeTest(t, p, SchemaV2)
	data = mutateJSON(t, data, func(doc map[string]any) {
		delete(doc, "seam_constraints")
	})
	got := loadTest(t, data)
	if len(got.Seams) == 0 {
		t.Fatal("no seams inferred")
	}
	found := false
	for _, tr := range got.Traces {
		if tr.Operation == "adjacency_inference" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing adjacency inference trace")
	}
	for _, b := range got.Blocks {
		if len(b.Neighbors) == 0 {
			t.Fatalf("block %s has no inferred neighbours", b.ID)
		}
		if b.DeclaredNeighbors {
			t.Fatalf("block %s should not claim a declared adjacency table", b.ID)
		}
	}
}

func TestSchemaFailures(t *testing.T) {
	p := testProject(t)
	v2 := encodeTest(t, p, SchemaV2)

	missing := mutateJSON(t, v2, func(doc map[string]any) { delete(doc, "seed") })
	e := mustCode(t, func() error { _, err := LoadProject(missing); return err }(), CodeSchema)
	if e.Path != "seed" {
		t.Fatalf("missing seed path = %s", e.Path)
	}

	unknown := mutateJSON(t, v2, func(doc map[string]any) { doc["version"] = "terrain/v9" })
	e = mustCode(t, func() error { _, err := LoadProject(unknown); return err }(), CodeSchema)
	if e.Path != "version" {
		t.Fatalf("unknown version path = %s", e.Path)
	}

	badJSON := append([]byte("{\"version\":\"terrain/v2\",\"chunks\":"), []byte("[}")...)
	mustCode(t, func() error { _, err := LoadProject(badJSON); return err }(), CodeDecode)

	overlap := mutateJSON(t, encodeTest(t, p, SchemaV1), func(doc map[string]any) {
		chunks := doc["chunks"].([]any)
		c0 := chunks[0].(map[string]any)["bounds"].(map[string]any)
		c1 := chunks[1].(map[string]any)["bounds"].(map[string]any)
		c1["min_x"] = c0["min_x"]
	})
	e = mustCode(t, func() error { _, err := LoadProject(overlap); return err }(), CodeOver)
	if e.Path == "" {
		t.Fatal("E_OVER missing field path")
	}

	adj := mutateJSON(t, encodeTest(t, p, SchemaV3), func(doc map[string]any) {
		blocks := doc["blocks"].([]any)
		b0 := blocks[0].(map[string]any)
		b0["adjacent"] = append(b0["adjacent"].([]any), "B99")
	})
	e = mustCode(t, func() error { _, err := LoadProject(adj); return err }(), CodeAdj)
	if e.Path == "" {
		t.Fatal("E_ADJ missing field path")
	}
}

func TestSchemaEncodingDeterministic(t *testing.T) {
	p := testProject(t)
	for _, v := range []SchemaVersion{SchemaV1, SchemaV2, SchemaV3} {
		a := encodeTest(t, p, v)
		b := encodeTest(t, p, v)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s encoding is not deterministic", v)
		}
	}
}

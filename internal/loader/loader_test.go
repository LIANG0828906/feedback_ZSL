package loader

import (
	"encoding/json"
	"testing"

	"github.com/gameaudio/workbench/internal/errors"
	"math"

	"github.com/gameaudio/workbench/internal/generator"
	"github.com/gameaudio/workbench/internal/model"
)

func TestValidateGoldenProject(t *testing.T) {
	p := generator.Generate()
	if err := Validate(p, model.GeoConfig{ScaleBits: generator.DefaultScaleBits}); err != nil {
		t.Fatalf("golden project must validate: %v", err)
	}
}

func TestSchemaMissingField(t *testing.T) {
	p := generator.Generate()
	p.SchemaVersion = ""
	err := Validate(p, model.GeoConfig{ScaleBits: 4})
	if errors.CodeOf(err) != errors.ESchema {
		t.Fatalf("want E_SCHEMA, got %v", err)
	}
}

func TestPolygonNotClosed(t *testing.T) {
	p := generator.Generate()
	p.Regions[0].Vertices = p.Regions[0].Vertices[:2]
	err := Validate(p, model.GeoConfig{ScaleBits: 4})
	if errors.CodeOf(err) != errors.ESchema {
		t.Fatalf("want E_SCHEMA for open polygon, got %v", err)
	}
}

func TestPolygonSelfIntersect(t *testing.T) {
	p := generator.Generate()
	// 蝴蝶结自交四边形。
	p.Regions[0].Vertices = []model.Point{
		{X: 0, Y: 0}, {X: 10, Y: 10}, {X: 10, Y: 0}, {X: 0, Y: 10},
	}
	err := Validate(p, model.GeoConfig{ScaleBits: 4})
	if errors.CodeOf(err) != errors.ESchema {
		t.Fatalf("want E_SCHEMA for self-intersect, got %v", err)
	}
}

func TestReferenceMissingRegion(t *testing.T) {
	p := generator.Generate()
	p.Sources[0].RegionID = "NOPE"
	err := Validate(p, model.GeoConfig{ScaleBits: 4})
	if errors.CodeOf(err) != errors.ERef {
		t.Fatalf("want E_REF, got %v", err)
	}
	if we, ok := err.(*errors.WorkError); ok && we.Field != "source:"+p.Sources[0].ID {
		t.Fatalf("error must point at source id, got field=%s", we.Field)
	}
}

func TestNonFiniteWall(t *testing.T) {
	p := generator.Generate()
	p.Walls[0].B.X = math.Inf(1)
	err := Validate(p, model.GeoConfig{ScaleBits: 4})
	if errors.CodeOf(err) != errors.ESchema {
		t.Fatalf("want E_SCHEMA for non-finite wall, got %v", err)
	}
}

func TestPointOutsideLevel(t *testing.T) {
	p := generator.Generate()
	p.Listeners[0].Pos.X = p.Level.X1 + 5
	err := Validate(p, model.GeoConfig{ScaleBits: 4})
	if errors.CodeOf(err) != errors.ESchema {
		t.Fatalf("want E_SCHEMA, got %v", err)
	}
}

func TestMissingTruth(t *testing.T) {
	p := generator.Generate()
	p.Truths = p.Truths[1:]
	err := Validate(p, model.GeoConfig{ScaleBits: 4})
	if errors.CodeOf(err) != errors.ESchema {
		t.Fatalf("want E_SCHEMA for missing truth, got %v", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	p := generator.Generate()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(data, model.GeoConfig{ScaleBits: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Truths) != len(p.Truths) {
		t.Fatalf("truth count mismatch")
	}
}

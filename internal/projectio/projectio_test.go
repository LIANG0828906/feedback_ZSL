package projectio_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"terrain-occlusion/internal/model"
	"terrain-occlusion/internal/projectio"
	"terrain-occlusion/internal/samplegen"
)

func errorCode(t *testing.T, err error) model.ErrorCode {
	t.Helper()
	var te *model.Error
	if !errors.As(err, &te) {
		t.Fatalf("expected *model.Error, got %T: %v", err, err)
	}
	return te.Code
}

func TestGeneratedProjectValidatesAndRoundTrips(t *testing.T) {
	p, err := samplegen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := projectio.Validate(p); err != nil {
		t.Fatalf("generated project must validate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "project.json")
	if err := projectio.SaveFile(path, p); err != nil {
		t.Fatal(err)
	}
	loaded, err := projectio.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Chunks) != len(p.Chunks) || loaded.Seed != p.Seed {
		t.Fatal("round trip changed core project data")
	}
}

func TestMissingFieldReturnsESchema(t *testing.T) {
	p, _ := samplegen.Generate()
	data, _ := json.Marshal(p)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "seed")
	missing, _ := json.Marshal(raw)
	path := filepath.Join(t.TempDir(), "missing.json")
	if err := os.WriteFile(path, missing, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := projectio.LoadFile(path)
	if code := errorCode(t, err); code != model.ESchema {
		t.Fatalf("want E_SCHEMA, got %s (%v)", code, err)
	}
}

func TestDimensionAndReferenceErrors(t *testing.T) {
	p, _ := samplegen.Generate()
	p.Terrain.Heights = p.Terrain.Heights[:10]
	if code := errorCode(t, projectio.Validate(p)); code != model.EDim {
		t.Fatalf("height length want E_DIM, got %s", code)
	}

	p, _ = samplegen.Generate()
	p.Chunks[1].Rect = p.Chunks[0].Rect
	if code := errorCode(t, projectio.Validate(p)); code != model.EDim {
		t.Fatalf("overlap want E_DIM, got %s (%v)", code, projectio.Validate(p))
	}

	p, _ = samplegen.Generate()
	p.Chunks[0].OccluderRefs = []int{999}
	err := projectio.Validate(p)
	if code := errorCode(t, err); code != model.ERef {
		t.Fatalf("bad ref want E_REF, got %s", code)
	}
	var te *model.Error
	errors.As(err, &te)
	if te.Field == "" || te.Details["reference"] != 999 {
		t.Fatalf("E_REF must identify field/reference: %+v", te)
	}
}

func TestParameterRanges(t *testing.T) {
	q := sampleParams()
	q.ShadowResolution = 8
	err := projectio.ValidateParameters(q)
	if code := errorCode(t, err); code != model.EPara {
		t.Fatalf("resolution want E_PARA, got %s", code)
	}
	var pe *model.Error
	errors.As(err, &pe)
	if pe.Field != "parameters.shadow_resolution" || pe.Allowed == "" {
		t.Fatalf("parameter error must state field and interval: %+v", pe)
	}

	q = sampleParams()
	q.LightElevationDeg = 3
	if code := errorCode(t, projectio.ValidateParameters(q)); code != model.EPara {
		t.Fatalf("elevation want E_PARA, got %s", code)
	}
}

func sampleParams() model.QuantParameters {
	p, _ := samplegen.Generate()
	return p.Parameters
}

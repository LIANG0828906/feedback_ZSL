package schema_test

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"terrainkernel/internal/bake"
	"terrainkernel/internal/gen"
	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
	"terrainkernel/internal/schema"
)

func TestLoadThreeVersions(t *testing.T) {
	samples := mustSamples(t)
	for _, tc := range []struct {
		version int
		data    []byte
	}{
		{1, samples.V1},
		{2, samples.V2},
		{3, samples.V3},
	} {
		project, err := schema.Load(tc.data)
		if err != nil {
			t.Fatalf("load v%d: %v", tc.version, err)
		}
		if project.SourceVersion != tc.version {
			t.Fatalf("v%d source version = %d", tc.version, project.SourceVersion)
		}
		if project.ProjectID != samples.Project.ProjectID || project.ChunkCount() != samples.Project.ChunkCount() {
			t.Fatalf("v%d header mismatch", tc.version)
		}
		if len(project.Layers) != len(samples.Project.Layers) {
			t.Fatalf("v%d layer count mismatch", tc.version)
		}
	}
}

func TestProbeAndValidationErrors(t *testing.T) {
	assertKerr(t, func() error {
		_, err := schema.Load([]byte(`{"format_version":9}`))
		return err
	}(), kerr.CodeVersion, "format_version")

	assertKerr(t, func() error {
		_, err := schema.Load([]byte(`{"format_version":1,"height_unit":"cm","slope_unit":"rad","normalization":"global"}`))
		return err
	}(), kerr.CodeSchema, "project_id")

	assertKerr(t, func() error {
		_, err := schema.Load([]byte(`{"format_version":1,"project_id":"p","height_unit":"furlong","slope_unit":"rad","normalization":"global","chunk_cols":1,"chunk_rows":1,"chunk_size_px":1,"layers":[]}`))
		return err
	}(), kerr.CodeUnit, "height_unit")

	assertKerr(t, func() error {
		_, err := schema.Load([]byte(`{"formatVer":2,"projectId":"p","heightUnit":"mm","slopeUnit":"rad","normalization":"global","chunkCols":1,"chunkRows":1,"chunkSizePx":1,"layers":[]}`))
		return err
	}(), kerr.CodeUnit, "slopeUnit")

	assertKerr(t, func() error {
		_, err := schema.Load([]byte(`{"formatVer":2,"projectId":"","heightUnit":"mm","slopeUnit":"deg","normalization":"global","chunkCols":1,"chunkRows":1,"chunkSizePx":1,"layers":[]}`))
		return err
	}(), kerr.CodeSchema, "projectId")

	assertKerr(t, func() error {
		_, err := schema.Load([]byte(`{"FormatVersion":3,"ProjectID":"p","HeightUnit":"m","SlopeUnit":"deg","Normalization":"global","ChunkCols":1,"ChunkRows":1,"ChunkSizePx":1,"Layers":[{"LayerID":"x","Order":0,"Driver":"height","Tint":1}]}`))
		return err
	}(), kerr.CodeSchema, "layers[0].CurvePoints")

	assertKerr(t, func() error {
		_, err := schema.HeightToMM(math.MaxInt64, "cm", "Height")
		return err
	}(), kerr.CodeOverflow, "Height")
}

func TestUpgradeTracesAndLossyExport(t *testing.T) {
	samples := mustSamples(t)
	v1, err := schema.Load(samples.V1)
	if err != nil {
		t.Fatal(err)
	}
	up, err := schema.Upgrade(v1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Traces) == 0 {
		t.Fatal("expected transform traces")
	}
	for _, trace := range up.Traces {
		if trace.Path == "" || trace.OldValue == "" || trace.DefaultFrom == "" {
			t.Fatalf("incomplete trace: %+v", trace)
		}
	}

	data, err := schema.Marshal(samples.Project, 2)
	if err == nil {
		t.Fatal("v2 export should report noise loss")
	}
	loss, ok := kerr.AsLossError(err)
	if !ok || len(loss.Items) == 0 || len(data) == 0 {
		t.Fatalf("unexpected lossy v2 export: %v", err)
	}
	data1, err := schema.Marshal(samples.Project, 1)
	if err == nil {
		t.Fatal("v1 export should report curve/noise loss")
	}
	if _, ok := kerr.AsLossError(err); !ok || len(data1) == 0 {
		t.Fatalf("unexpected lossy v1 export: %v", err)
	}
}

func TestDeterministicRepeatedExport(t *testing.T) {
	samples := mustSamples(t)
	for _, version := range []int{1, 2, 3} {
		a, _ := schema.Marshal(samples.Project, version)
		b, _ := schema.Marshal(samples.Project, version)
		if !bytes.Equal(a, b) {
			t.Fatalf("v%d export is not byte-identical", version)
		}
	}
}

func TestSlopeCapInheritance(t *testing.T) {
	v2 := []byte(`{
		"formatVer":2,"projectId":"cap","heightUnit":"mm","slopeUnit":"deg",
		"normalization":"global","seed":1,"chunkCols":1,"chunkRows":1,"chunkSizePx":1,
		"slopeCapDeg":45000,
		"layers":[
			{"layerId":"a","order":0,"driver":"height","curvePoints":[{"x":0,"y":1}],"tint":1},
			{"layerId":"b","order":1,"driver":"height","curvePoints":[{"x":0,"y":1}],"tint":1}
		]
	}`)
	project, err := schema.Load(v2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range project.Layers {
		if project.Layers[i].SlopeCapURad != 785398 {
			t.Fatalf("layer %d cap = %d", i, project.Layers[i].SlopeCapURad)
		}
	}
	v3 := []byte(`{
		"FormatVersion":3,"ProjectID":"cap","HeightUnit":"m","SlopeUnit":"deg",
		"Normalization":"global","Seed":1,"ChunkCols":1,"ChunkRows":1,"ChunkSizePx":1,
		"Layers":[
			{"LayerID":"a","Order":0,"Driver":"height","CurvePoints":[{"X":0,"Y":1}],"Tint":1,"SlopeCapDeg":45000},
			{"LayerID":"b","Order":1,"Driver":"height","CurvePoints":[{"X":0,"Y":1}],"Tint":1,"SlopeCapDeg":30000}
		]
	}`)
	project3, err := schema.Load(v3)
	if err != nil {
		t.Fatal(err)
	}
	if project3.Layers[1].SlopeCapURad >= project3.Layers[0].SlopeCapURad {
		t.Fatal("per-layer caps were not preserved")
	}
}

func TestLosslessV2RoundTripWeightTolerance(t *testing.T) {
	samples := mustSamples(t)
	project := samples.Project.Clone()
	for i := range project.Layers {
		project.Layers[i].Noise = nil
	}
	data, err := schema.Marshal(project, 2)
	if err != nil {
		t.Fatalf("lossless v2 export failed: %v", err)
	}
	loaded, err := schema.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	assertWeightTolerance(t, project, loaded, 2)
}

func TestV1RoundTripQuantizationTolerance(t *testing.T) {
	project := &model.Project{
		SourceVersion: 3,
		ProjectID:     "roundtrip",
		NormMode:      "global",
		Seed:          7,
		ChunkCols:     1,
		ChunkRows:     1,
		ChunkSizePx:   4,
		Layers: []model.Layer{{
			ID:    "sand",
			Order: 0,
			Curve: &model.Curve{Driver: "height", Points: []model.CurvePoint{
				{X: 0, Y: 0},
				{X: 50_000, Y: 1_000_000},
				{X: 100_000, Y: 0},
			}},
			Tint: 100,
		}},
	}
	data, err := schema.Marshal(project, 1)
	if _, ok := kerr.AsLossError(err); !ok {
		t.Fatalf("expected E_LOSSY, got %v", err)
	}
	loaded, err := schema.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	assertWeightTolerance(t, project, loaded, 2)
}

func TestNormalizationSumTolerance(t *testing.T) {
	raw := make([]int64, 64)
	for i := range raw {
		raw[i] = int64((i*i + 17) % 1_000_000)
	}
	anchors := bake.LayerAnchors(&model.Project{NormMode: "global", Layers: make([]model.Layer, len(raw))})
	normalized, err := bake.Normalize("global", raw, anchors)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, value := range normalized {
		sum += value
	}
	if sum != model.WeightScale {
		t.Fatalf("normalized sum = %d", sum)
	}
}

func mustSamples(t *testing.T) *gen.SampleSet {
	t.Helper()
	samples, err := gen.Generate(gen.Options{Seed: 20261125, ChunkCols: 2, ChunkRows: 2, ChunkSizePx: 8})
	if err != nil {
		t.Fatal(err)
	}
	return samples
}

func assertWeightTolerance(t *testing.T, a, b *model.Project, tolerance int64) {
	t.Helper()
	for y := 0; y < a.ChunkRows*a.ChunkSizePx; y++ {
		for x := 0; x < a.ChunkCols*a.ChunkSizePx; x++ {
			pa, err := bake.BakePixel(a, x, y)
			if err != nil {
				t.Fatal(err)
			}
			pb, err := bake.BakePixel(b, x, y)
			if err != nil {
				t.Fatal(err)
			}
			if len(pa.Weights) != len(pb.Weights) {
				t.Fatalf("weight count differs at %d,%d", x, y)
			}
			for i := range pa.Weights {
				delta := pa.Weights[i] - pb.Weights[i]
				if delta < 0 {
					delta = -delta
				}
				if delta > tolerance {
					t.Fatalf("weight[%d] at %d,%d differs by %d", i, x, y, delta)
				}
			}
		}
	}
}

func assertKerr(t *testing.T, err error, code, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s", code)
	}
	var ke *kerr.Error
	if !errors.As(err, &ke) {
		t.Fatalf("expected kerr, got %T: %v", err, err)
	}
	if ke.Code != code || ke.Path != path {
		t.Fatalf("got %s at %s, want %s at %s", ke.Code, ke.Path, code, path)
	}
}

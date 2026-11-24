package pipeline_test

import (
	"path/filepath"
	"testing"

	"strata/internal/formats"
	"strata/internal/generator"
	"strata/internal/model"
	"strata/internal/pipeline"
)

func sampleMesh(t *testing.T) (*model.Geometry, *pipeline.Result, *generator.Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	manifest, err := generator.Generate(195, dir)
	if err != nil {
		t.Fatal(err)
	}
	geometry, err := formats.LoadFile(filepath.Join(dir, "scene-ascii-v1.ply"), model.VersionPLYASCII)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.NewBuilder().Build(geometry, pipeline.DefaultConfig(), "exact", 1)
	if err != nil {
		t.Fatal(err)
	}
	return geometry, result, manifest, dir
}

func TestDeterministicPipelineTruth(t *testing.T) {
	_, result, manifest, _ := sampleMesh(t)
	if len(result.CleanPoints) != 1813 || len(result.Removed) != 24 {
		t.Fatalf("denoise clean=%d removed=%d", len(result.CleanPoints), len(result.Removed))
	}
	if len(result.Mesh.Vertices) != 2028 || len(result.Mesh.Faces) != 3456 {
		t.Fatalf("mesh verts=%d faces=%d", len(result.Mesh.Vertices), len(result.Mesh.Faces))
	}
	if len(result.Mesh.Chunks) != 12 {
		t.Fatalf("chunks=%d", len(result.Mesh.Chunks))
	}
	for _, chunk := range result.Mesh.Chunks {
		if chunk.FaceCount != 288 || chunk.VertexCount != 169 {
			t.Fatalf("%s face/vertex count = %d/%d", chunk.ID, chunk.FaceCount, chunk.VertexCount)
		}
	}
	if !result.Mesh.Bounds.Within(manifest.Truth.CleanBounds.Expanded(manifest.Truth.BoundsExpansion), 1e-9) {
		t.Fatalf("mesh bounds escaped source bounds: %+v outside %+v", result.Mesh.Bounds, manifest.Truth.CleanBounds)
	}
	if err := featureError(result.Features, manifest.Truth.Features); err > manifest.Truth.FeatureTolerance {
		t.Fatalf("feature error %.9f > %.9f", err, manifest.Truth.FeatureTolerance)
	}
}

func TestCrossVersionMeshIsVertexIdentical(t *testing.T) {
	dir := t.TempDir()
	if _, err := generator.Generate(195, dir); err != nil {
		t.Fatal(err)
	}
	cfg := pipeline.DefaultConfig()
	hashes := map[string]string{}
	for _, version := range formats.Versions() {
		geometry, err := formats.LoadFile(filepath.Join(dir, version.File), version.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.NewBuilder().Build(geometry, cfg, "exact", 1)
		if err != nil {
			t.Fatal(err)
		}
		hashes[version.ID] = result.Mesh.ContentHash
	}
	var first string
	for version, hash := range hashes {
		if first == "" {
			first = hash
		}
		if hash != first {
			t.Fatalf("%s mesh hash differs: %s != %s", version, hash, first)
		}
	}
}

func TestIncrementalPlanReusesUnaffectedChunks(t *testing.T) {
	_, result, _, _ := sampleMesh(t)
	clean := append([]model.Point(nil), result.CleanPoints...)
	changed := clean[0]
	changed.Z += 0.02
	clean[0] = changed
	reasons := pipeline.PlanInvalidation(result.Mesh, "C00", "point-edit")
	if len(reasons) != 4 {
		t.Fatalf("corner edit should invalidate 4 chunks, got %v", reasons)
	}
	incremental, err := pipeline.NewBuilder().BuildIncremental(clean, pipeline.DefaultConfig(), result.Mesh, reasons, 2, "incremental", 1)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, reused := 0, 0
	for _, chunk := range incremental.Chunks {
		if chunk.Rebuilt {
			rebuilt++
		}
		if chunk.Reused {
			reused++
		}
	}
	if rebuilt != 4 || reused != 8 {
		t.Fatalf("rebuilt=%d reused=%d", rebuilt, reused)
	}
	for _, chunk := range incremental.Chunks {
		if chunk.ID == "C02" && chunk.ContentHash != result.Mesh.Chunks[2].ContentHash {
			t.Fatalf("unaffected C02 content changed")
		}
	}
}

func TestParameterValidationAndWorkerDeterminism(t *testing.T) {
	geometry, _, _, _ := sampleMesh(t)
	cfg := pipeline.DefaultConfig()
	cfg.MaxWorkers = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected E_PARA for maxWorkers")
	} else if model.AsAppError(err).Code != model.EPARA {
		t.Fatalf("got %v", err)
	}
	cfg = pipeline.DefaultConfig()
	cfg.MaxWorkers = 1
	a, err := pipeline.NewBuilder().Build(geometry, cfg, "exact", 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxWorkers = 4
	b, err := pipeline.NewBuilder().Build(geometry, cfg, "exact", 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Mesh.ContentHash != b.Mesh.ContentHash {
		t.Fatalf("worker count changed deterministic mesh")
	}
}

func featureError(a, b model.FeatureSet) float64 {
	maxErr := 0.0
	pairs := [][2]model.Vec3{
		{a.Peak, b.Peak}, {a.Trough, b.Trough}, {a.Ridge, b.Ridge},
		{a.MinCorner, b.MinCorner}, {a.MaxCorner, b.MaxCorner},
	}
	for _, pair := range pairs {
		d := pair[0].Sub(pair[1]).Length()
		if d > maxErr {
			maxErr = d
		}
	}
	return maxErr
}

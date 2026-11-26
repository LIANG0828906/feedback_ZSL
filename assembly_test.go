package silhouette

import (
	"context"
	"reflect"
	"testing"
)

func TestBoundaryOwnershipIsUnique(t *testing.T) {
	scene := GenerateScene(DefaultSeed)
	tests := []struct {
		point Vec2
		tile  string
	}{
		{logicalPoint(320, 0), "tile-1"},
		{logicalPoint(320, 320), "tile-3"},
		{logicalPoint(0, 320), "tile-2"},
		{logicalPoint(640, 640), "tile-3"},
		{logicalPoint(320, 640), "tile-2"},
	}
	for _, tt := range tests {
		owner, ok := OwnTile(scene.Tiles, tt.point)
		if !ok || owner.ID != tt.tile {
			t.Fatalf("point %s owner=%v want %s", tt.point, owner.ID, tt.tile)
		}
	}
	sampling, err := SampleScene(scene, SamplingOptions{Tolerance: DefaultTolerance, Budget: 100000})
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := Assemble(scene, sampling)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]string)
	total := 0
	for _, tile := range assembly.Tiles {
		for _, ts := range tile.Segments {
			for _, vertex := range ts.Vertices {
				key := ts.SegmentID + ":" + strconvI(vertex.Index)
				if other, ok := seen[key]; ok {
					t.Fatalf("vertex %s counted in %s and %s", key, other, tile.TileID)
				}
				seen[key] = tile.TileID
				total++
			}
		}
	}
	if total != totalVertices(sampling.Segments) {
		t.Fatalf("assembled vertices=%d sampled vertices=%d", total, totalVertices(sampling.Segments))
	}
}

func TestAssemblyHashesStableAcrossParallelism(t *testing.T) {
	scene := GenerateScene(DefaultSeed)
	sampling, err := SampleScene(scene, SamplingOptions{Tolerance: DefaultTolerance, Budget: 90})
	if err != nil {
		t.Fatal(err)
	}
	sequential, err := AssembleContext(context.Background(), scene, sampling, 1, DefaultVerticesPerWorker)
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := AssembleContext(context.Background(), scene, sampling, 3, DefaultVerticesPerWorker)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sequential.Tiles, parallel.Tiles) {
		t.Fatal("parallel assembly differs from sequential assembly")
	}
	versionOnly := scene.Clone()
	versionOnly.SourceVersion = 1
	changedScene, err := Assemble(versionOnly, sampling)
	if err != nil {
		t.Fatal(err)
	}
	for i := range sequential.Tiles {
		if sequential.Tiles[i].ContentHash != changedScene.Tiles[i].ContentHash {
			t.Fatal("content hash depends on source version instead of content")
		}
	}
	metadata := scene.Clone()
	metadata.Segments[0].MetadataVersion = "curve-meta/test-change"
	changed, err := Assemble(metadata, sampling)
	if err != nil {
		t.Fatal(err)
	}
	if sequential.Tiles[0].ContentHash == changed.Tiles[0].ContentHash {
		t.Fatal("content hash must include curve metadata version")
	}
}

func TestCacheKeyDependsOnEveryDeclaredInput(t *testing.T) {
	base := ComputeCacheKey("input", []string{"seg-0", "seg-1"}, "tol-v1", "budget-v1", "upstream")
	cases := []string{
		ComputeCacheKey("input-2", []string{"seg-0", "seg-1"}, "tol-v1", "budget-v1", "upstream"),
		ComputeCacheKey("input", []string{"seg-0"}, "tol-v1", "budget-v1", "upstream"),
		ComputeCacheKey("input", []string{"seg-0", "seg-1"}, "tol-v2", "budget-v1", "upstream"),
		ComputeCacheKey("input", []string{"seg-0", "seg-1"}, "tol-v1", "budget-v2", "upstream"),
		ComputeCacheKey("input", []string{"seg-0", "seg-1"}, "tol-v1", "budget-v1", "upstream-2"),
	}
	for i, value := range cases {
		if value == base {
			t.Fatalf("cache key did not change for input variation %d", i)
		}
	}
}

func strconvI(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

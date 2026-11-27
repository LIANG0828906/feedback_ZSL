package grid

import "testing"

func TestEndpointEditPropagationAndPixelDiff(t *testing.T) {
	level := Generate(DefaultSeed)
	cam := DefaultCamera()
	versions := DefaultSemanticVersions()
	plan, err := EditObstacle(level, cam, &versions, "OB03", "endpoint", func(o *Obstacle) {
		o.B.X += 500 * PositionScale
	})
	if err != nil {
		t.Fatal(err)
	}
	if versions.Endpoint != 2 || len(plan.Tiles) == 0 {
		t.Fatalf("endpoint version=%d tiles=%v", versions.Endpoint, plan.Tiles)
	}
	foundDirect := false
	for _, item := range plan.Items {
		if item.OldVersions.Endpoint != 1 {
			t.Fatalf("old endpoint component = %d", item.OldVersions.Endpoint)
		}
		for _, reason := range item.Reasons {
			if reason == "direct-endpoint:OB03" {
				foundDirect = true
			}
		}
	}
	if !foundDirect {
		t.Fatalf("missing direct endpoint reason: %+v", plan.Items)
	}

	a, err := ProjectScene(Generate(DefaultSeed), cam)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ProjectScene(Generate(DefaultSeed), cam)
	if err != nil {
		t.Fatal(err)
	}
	b.Tiles[2].Color[0] ^= 0xff
	ge := CompareScenes(a, b)
	if ge == nil || ge.Code != CodeDiff || ge.Tile == nil || ge.Tile.String() != "2,0" {
		t.Fatalf("pixel diff = %#v", ge)
	}
}

func TestRecoverableVersionComponentMismatch(t *testing.T) {
	fresh := &Tile{ContentHash: "h", ColorDigest: "c", DepthDigest: "d"}
	cached := &CachedTile{
		ContentHash: "h", ColorDigest: "c", DepthDigest: "d",
		StructureVersion: 2, CameraVersion: 1, AlgorithmVersion: AlgorithmVersion,
	}
	ok, field := cached.Recoverable(fresh, DefaultSemanticVersions(), 1)
	if ok || field != "structureVersion" {
		t.Fatalf("recoverable=%v field=%s", ok, field)
	}
}

package terrain

import (
	"testing"
)

func TestMeshCoversEveryGridPoint(t *testing.T) {
	project := GenerateSample(42)
	plan, err := PlanAlignment(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range project.Blocks {
		mesh, err := BuildBlockMesh(block, BuildOptions{ParameterVersion: project.ParameterVersion, ForcedPoints: plan.Forced[block.BlockNo]})
		if err != nil {
			t.Fatal(err)
		}
		for ti, tri := range mesh.Triangles {
			if tri.Vertices[0] == tri.Vertices[1] || tri.Vertices[1] == tri.Vertices[2] || tri.Vertices[0] == tri.Vertices[2] {
				t.Fatalf("block %d degenerate triangle %d: %+v", block.BlockNo, ti, tri.Vertices)
			}
			for _, vi := range tri.Vertices {
				if vi < 0 || vi >= len(mesh.Vertices) {
					t.Fatalf("block %d triangle %d has bad vertex %d", block.BlockNo, ti, vi)
				}
			}
		}
		for y := 0; y < block.Grid.Height; y++ {
			for x := 0; x < block.Grid.Width; x++ {
				query := dVertex{x: float64(x), z: float64(y)}
				covered := false
				for _, tri := range mesh.Triangles {
					a := mesh.Vertices[tri.Vertices[0]]
					b := mesh.Vertices[tri.Vertices[1]]
					c := mesh.Vertices[tri.Vertices[2]]
					da := dVertex{x: float64(a.GridX), z: float64(a.GridY)}
					db := dVertex{x: float64(b.GridX), z: float64(b.GridY)}
					dc := dVertex{x: float64(c.GridX), z: float64(c.GridY)}
					if _, _, _, ok := barycentric(query, da, db, dc); ok {
						covered = true
						break
					}
				}
				if !covered {
					t.Fatalf("block %d grid point (%d,%d) is outside every triangle", block.BlockNo, x, y)
				}
			}
		}
	}
}

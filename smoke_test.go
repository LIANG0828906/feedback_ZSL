package terrain

import (
	"testing"
)

func TestSmokeCompile(t *testing.T) {
	p := GenerateSample(42)
	c, err := CompileProject(p)
	if err != nil {
		t.Fatal(err)
	}
	for no, m := range c.Meshes {
		t.Logf("block %d verts=%d tris=%d maxErr=%.4f budget=%d", no, len(m.Vertices), len(m.Triangles), m.MaxHeightError, p.Blocks[no].SampleBudget)
	}
	for id, r := range c.Reports {
		t.Logf("boundary %s angle=%.3f cat=%s aligned=%v counts=%d/%d ev=%v", id, r.MaxFoldAngleDeg, r.Category, r.Aligned, r.VertexCountA, r.VertexCountB, r.Evidence)
	}
	for _, tc := range p.Truth.VertexChecks {
		m := c.Meshes[tc.BlockNo]
		found := false
		for _, v := range m.Vertices {
			if v.GridX == tc.GridX && v.GridY == tc.GridY {
				found = true
				d := abs(v.Y - tc.ExpectedHeight)
				if d > tc.Tolerance {
					t.Fatalf("truth block %d (%d,%d): d=%g tol=%g", tc.BlockNo, tc.GridX, tc.GridY, d, tc.Tolerance)
				}
			}
		}
		if !found {
			t.Fatalf("truth point not vertex: %+v", tc)
		}
	}
	for _, be := range p.Truth.BlockErrorBounds {
		if c.Meshes[be.BlockNo].MaxHeightError > be.DeclaredMaxHeightError {
			t.Fatalf("block %d max err %g > bound %g", be.BlockNo, c.Meshes[be.BlockNo].MaxHeightError, be.DeclaredMaxHeightError)
		}
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

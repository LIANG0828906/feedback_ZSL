package pipeline

import (
	"testing"

	"meshdoctor/internal/sample"
)

func TestSmokeAllSamples(t *testing.T) {
	samples, err := sample.GenerateAll(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		s := s
		t.Run(s.Manifest.ID, func(t *testing.T) {
			res, err := NewEngine(s.Manifest.ID, &s.Manifest, DefaultParams()).RunAll(&s.Mesh)
			if err != nil {
				t.Fatalf("RunAll failed: %v", err)
			}
			if len(res.Stages) != len(Stages) {
				t.Fatalf("stages=%d want %d", len(res.Stages), len(Stages))
			}
			last := res.FinalStats
			if last.Vertices != s.Manifest.Truth.Vertices || last.Faces != s.Manifest.Truth.Faces || last.Euler != s.Manifest.Truth.Euler {
				t.Fatalf("final topology=%+v truth=%+v", last, s.Manifest.Truth)
			}
			if last.BoundaryEdges != 0 || last.NonManifoldEdges != 0 {
				t.Fatalf("final not closed/manifold: boundary=%d nonmanifold=%d", last.BoundaryEdges, last.NonManifoldEdges)
			}
			t.Logf("final stats: %+v", last)
			for _, st := range res.Stages {
				if st.Truth != nil {
					for _, p := range st.Truth.Points {
						t.Logf("stage=%s point=%s d=%.6g tol=%.6g pass=%v", st.Stage, p.PointID, p.Distance, p.Tolerance, p.Passed)
					}
					for _, l := range st.Truth.Loops {
						t.Logf("stage=%s loop=%s err=%.6g tol=%.6g pass=%v", st.Stage, l.RingID, l.ClosureError, l.Tolerance, l.Passed)
					}
				}
			}
		})
	}
}

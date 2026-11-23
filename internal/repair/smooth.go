package repair

import (
	"fmt"
	"math"
	"sort"

	"meshforge/internal/errs"
	"meshforge/internal/generator"
	"meshforge/internal/mesh"
)

type SmoothOptions struct {
	Weight     float64
	Iterations int
}

type PointError struct {
	Mean float64 `json:"mean"`
	Max  float64 `json:"max"`
}

type SmoothReport struct {
	Weight            float64    `json:"weight"`
	Mu                float64    `json:"mu"`
	Iterations        int        `json:"iterations"`
	KnownBefore       PointError `json:"known_error_before"`
	KnownAfter        PointError `json:"known_error_after"`
	TopologyPreserved bool       `json:"topology_preserved"`
	FacesUnchanged    bool       `json:"faces_unchanged"`
	Converged         bool       `json:"converged"`
}

func ValidateSmoothOptions(opt SmoothOptions) *errs.CodedError {
	if math.IsNaN(opt.Weight) || math.IsInf(opt.Weight, 0) ||
		opt.Weight <= 0 || opt.Weight > 0.75 {
		return errs.New(errs.CodePara,
			"laplacian weight must be in (0,0.75], got %g", opt.Weight)
	}
	if opt.Iterations < 1 || opt.Iterations > 500 {
		return errs.New(errs.CodePara,
			"laplacian iterations must be in [1,500], got %d", opt.Iterations)
	}
	return nil
}

func SmoothLaplacian(in mesh.Mesh, opt SmoothOptions, truth generator.Truth) (mesh.Mesh, SmoothReport, *errs.CodedError) {
	if e := ValidateSmoothOptions(opt); e != nil {
		return mesh.Mesh{}, SmoothReport{}, e
	}
	topoBefore := mesh.BuildTopology(in)
	countsBefore := [4]int{len(in.Vertices), len(in.Faces), len(topoBefore.Edges),
		len(in.Vertices) - len(topoBefore.Edges) + len(in.Faces)}
	facesBefore := append([]mesh.Face(nil), in.Faces...)
	adj := oneRingAdjacency(in)
	out := in.Clone()
	report := SmoothReport{Weight: opt.Weight, Iterations: opt.Iterations}
	report.Mu = -opt.Weight / (1 - 0.1*opt.Weight)
	report.KnownBefore = knownPointError(in, truth.KnownPoints)

	for iter := 0; iter < opt.Iterations; iter++ {
		out.Vertices = laplacianPass(out, adj, opt.Weight)
		out.Vertices = laplacianPass(out, adj, report.Mu)
	}
	report.KnownAfter = knownPointError(out, truth.KnownPoints)
	report.FacesUnchanged = facesEqual(facesBefore, out.Faces)
	topoAfter := mesh.BuildTopology(out)
	countsAfter := [4]int{len(out.Vertices), len(out.Faces), len(topoAfter.Edges),
		len(out.Vertices) - len(topoAfter.Edges) + len(out.Faces)}
	report.TopologyPreserved = countsBefore == countsAfter && report.FacesUnchanged
	report.Converged = report.KnownAfter.Mean <= report.KnownBefore.Mean+1e-15
	if !report.TopologyPreserved {
		return mesh.Mesh{}, report, errs.New(errs.CodeGeom,
			"laplacian smoothing changed vertex/face/edge/euler counts")
	}
	if !finiteMesh(out) {
		return mesh.Mesh{}, report, errs.New(errs.CodePara,
			"laplacian weight %g produced non-finite vertices", opt.Weight)
	}
	return out, report, nil
}

func oneRingAdjacency(m mesh.Mesh) [][]int {
	sets := make([]map[int]bool, len(m.Vertices))
	for _, f := range m.Faces {
		for _, e := range [][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}} {
			if sets[e[0]] == nil {
				sets[e[0]] = make(map[int]bool)
			}
			if sets[e[1]] == nil {
				sets[e[1]] = make(map[int]bool)
			}
			sets[e[0]][e[1]] = true
			sets[e[1]][e[0]] = true
		}
	}
	out := make([][]int, len(m.Vertices))
	for i, s := range sets {
		out[i] = make([]int, 0, len(s))
		for j := range s {
			out[i] = append(out[i], j)
		}
		sort.Ints(out[i])
	}
	return out
}

func laplacianPass(m mesh.Mesh, adj [][]int, weight float64) []mesh.Vec3 {
	next := append([]mesh.Vec3(nil), m.Vertices...)
	for i, v := range m.Vertices {
		if len(adj[i]) == 0 {
			continue
		}
		avg := mesh.Vec3{}
		for _, j := range adj[i] {
			avg = avg.Add(m.Vertices[j])
		}
		avg = avg.Mul(1 / float64(len(adj[i])))
		next[i] = v.Add(avg.Sub(v).Mul(weight))
	}
	return next
}

func knownPointError(m mesh.Mesh, known []generator.KnownPoint) PointError {
	if len(known) == 0 {
		return PointError{}
	}
	var sum, maxErr float64
	for _, kp := range known {
		if kp.Vertex < 0 || kp.Vertex >= len(m.Vertices) {
			continue
		}
		d := m.Vertices[kp.Vertex].Distance(kp.Position)
		sum += d
		if d > maxErr {
			maxErr = d
		}
	}
	return PointError{Mean: sum / float64(len(known)), Max: maxErr}
}

func facesEqual(a, b []mesh.Face) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func finiteMesh(m mesh.Mesh) bool {
	for _, v := range m.Vertices {
		if math.IsNaN(v.X) || math.IsNaN(v.Y) || math.IsNaN(v.Z) ||
			math.IsInf(v.X, 0) || math.IsInf(v.Y, 0) || math.IsInf(v.Z, 0) {
			return false
		}
	}
	return true
}

func (s SmoothReport) String() string {
	return fmt.Sprintf("w=%.3f iter=%d mean %.6g->%.6g", s.Weight, s.Iterations,
		s.KnownBefore.Mean, s.KnownAfter.Mean)
}

package repair

import (
	"fmt"
	"math"
	"sort"

	"meshforge/internal/errs"
	"meshforge/internal/generator"
	"meshforge/internal/mesh"
)

type Diagnostic struct {
	Stats            mesh.Stats                `json:"stats"`
	Expected         generator.DiagnosticTruth `json:"expected"`
	BoundaryLoops    mesh.BoundaryAnalysis     `json:"boundary_analysis"`
	Orientation      mesh.OrientationReport    `json:"-"`
	StructuralChecks []string                  `json:"structural_checks"`
}

func Diagnose(m mesh.Mesh, truth generator.Truth) (Diagnostic, *errs.CodedError) {
	if len(m.Faces) != truth.InputFaces {
		face := truth.InputFaces
		if len(m.Faces) < face {
			face = len(m.Faces)
		}
		e := errs.New(errs.CodeGeom,
			"mesh face count %d does not match manifest declaration %d",
			len(m.Faces), truth.InputFaces)
		return Diagnostic{}, errs.WithFace(e, face)
	}
	for fi, f := range m.Faces {
		for _, vi := range f.Indices() {
			if vi < 0 || vi >= len(m.Vertices) {
				e := errs.New(errs.CodeGeom,
					"face %d references vertex %d outside [0,%d)",
					fi, vi, len(m.Vertices))
				return Diagnostic{}, errs.WithFace(e, fi)
			}
		}
		if f.A == f.B || f.B == f.C || f.C == f.A {
			e := errs.New(errs.CodeGeom, "face %d repeats a vertex index", fi)
			return Diagnostic{}, errs.WithFace(e, fi)
		}
		area := m.FaceArea(fi)
		if area <= 1e-14 || math.IsNaN(area) || math.IsInf(area, 0) {
			e := errs.New(errs.CodeGeom, "face %d is degenerate (area %.9g)", fi, area)
			return Diagnostic{}, errs.WithFace(e, fi)
		}
	}

	topo := mesh.BuildTopology(m)
	orientation := mesh.AnalyzeOrientation(m)
	boundary := mesh.BoundaryLoops(m)
	comps := mesh.FaceComponents(m, mesh.ComponentAllSharedEdges)
	var nonManifold int
	minArea := math.Inf(1)
	for _, e := range topo.SortedEdges() {
		if len(e.Uses) > 2 {
			nonManifold++
		}
	}
	for i := range m.Faces {
		if a := m.FaceArea(i); a < minArea {
			minArea = a
		}
	}
	actual := generator.DiagnosticTruth{
		NonManifoldEdges:         nonManifold,
		DuplicateVertices:        mesh.DuplicateVertexCount(m),
		FlippedFaces:             orientation.FlippedFaces,
		OrientationConflictEdges: orientation.ConflictEdges,
		BoundaryLoops:            len(boundary.Loops),
		OpenBoundaryChains:       boundary.OpenChains,
		Components:               len(comps),
		ThinSheetComponents:      mesh.ThinSheetComponentCount(m),
		SelfIntersections:        mesh.CountSelfIntersections(m),
	}
	stats := mesh.Stats{
		Vertices:                 len(m.Vertices),
		Faces:                    len(m.Faces),
		Edges:                    len(topo.Edges),
		EulerCharacteristic:      len(m.Vertices) - len(topo.Edges) + len(m.Faces),
		NonManifoldEdges:         actual.NonManifoldEdges,
		DuplicateVertices:        actual.DuplicateVertices,
		FlippedFaces:             actual.FlippedFaces,
		OrientationConflictEdges: actual.OrientationConflictEdges,
		BoundaryLoops:            actual.BoundaryLoops,
		OpenBoundaryChains:       actual.OpenBoundaryChains,
		Components:               actual.Components,
		ThinSheetComponents:      actual.ThinSheetComponents,
		SelfIntersections:        actual.SelfIntersections,
		UnusedVertices:           mesh.UnusedVertexCount(m),
		MinArea:                  minArea,
		TotalArea:                m.SurfaceArea(),
		SignedVolume:             m.SignedVolume(),
	}
	mismatches := compareDiagnostics(actual, truth.Defects)
	if len(mismatches) > 0 {
		first := mismatches[0]
		e := errs.New(errs.CodeGeom, "diagnostic truth mismatch: %s", first)
		e.Details = mismatches
		errs.WithFace(e, 0)
		return Diagnostic{Stats: stats, Expected: truth.Defects, BoundaryLoops: boundary, Orientation: orientation}, e
	}
	return Diagnostic{
		Stats:            stats,
		Expected:         truth.Defects,
		BoundaryLoops:    boundary,
		Orientation:      orientation,
		StructuralChecks: []string{"indices", "faces", "areas"},
	}, nil
}

func compareDiagnostics(a, b generator.DiagnosticTruth) []string {
	type item struct {
		name string
		x, y int
	}
	items := []item{
		{"non_manifold_edges", a.NonManifoldEdges, b.NonManifoldEdges},
		{"duplicate_vertices", a.DuplicateVertices, b.DuplicateVertices},
		{"flipped_faces", a.FlippedFaces, b.FlippedFaces},
		{"orientation_conflict_edges", a.OrientationConflictEdges, b.OrientationConflictEdges},
		{"boundary_loops", a.BoundaryLoops, b.BoundaryLoops},
		{"open_boundary_chains", a.OpenBoundaryChains, b.OpenBoundaryChains},
		{"components", a.Components, b.Components},
		{"thin_sheet_components", a.ThinSheetComponents, b.ThinSheetComponents},
		{"self_intersections", a.SelfIntersections, b.SelfIntersections},
	}
	var out []string
	for _, it := range items {
		if it.x != it.y {
			out = append(out, fmt.Sprintf("%s actual=%d expected=%d", it.name, it.x, it.y))
		}
	}
	sort.Strings(out)
	return out
}

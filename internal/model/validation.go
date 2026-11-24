package model

import (
	"fmt"
	"math"
)

const DefaultClipTolerance = 1e-9

func ValidateDataset(ds Dataset) *Diagnostic {
	if ds.Seed == 0 {
		return Diag(CodeSchema, "missing field seed")
	}
	if ds.Tolerance <= 0 || math.IsNaN(ds.Tolerance) || math.IsInf(ds.Tolerance, 0) {
		return DiagPatch(CodeSchema, -1, "tolerance", "dataset tolerance must be positive and finite")
	}
	if len(ds.Patches) == 0 {
		return Diag(CodeSchema, "missing field patches")
	}
	if len(ds.Meshes) == 0 {
		return Diag(CodeSchema, "missing field meshes")
	}
	if len(ds.Truth.Samples) == 0 {
		return Diag(CodeSchema, "missing field truth.samples")
	}
	for _, p := range ds.Patches {
		if d := ValidatePatch(p); d != nil {
			return d
		}
	}
	for _, m := range ds.Meshes {
		if d := ValidateSubdivMesh(m); d != nil {
			return d
		}
	}
	return nil
}

func ValidatePatch(p SurfacePatch) *Diagnostic {
	pid := p.ID
	if p.Name == "" {
		return DiagPatch(CodeSchema, pid, "name", "missing surface patch name")
	}
	if p.UDegree < 1 || p.VDegree < 1 {
		return DiagPatch(CodeSchema, pid, "degree", "degrees must be positive")
	}
	if p.UCount < p.UDegree+1 || p.VCount < p.VDegree+1 {
		return DiagPatch(CodeSchema, pid, "uCount/vCount", "control point counts do not match degrees")
	}
	if len(p.Control) == 0 {
		return DiagPatch(CodeGeom, pid, "controlPoints", "control point missing")
	}
	if len(p.Control) != p.UCount*p.VCount {
		if len(p.Control) < p.UCount*p.VCount {
			return DiagPatch(CodeGeom, pid, "controlPoints", fmt.Sprintf("control point missing: have %d, expected %d", len(p.Control), p.UCount*p.VCount))
		}
		return DiagPatch(CodeSchema, pid, "controlPoints", fmt.Sprintf("have %d control points, expected %d", len(p.Control), p.UCount*p.VCount))
	}
	if len(p.UKnots) != p.UCount+p.UDegree+1 {
		return DiagPatch(CodeSchema, pid, "uKnots", "knot vector length does not match control count and degree")
	}
	if len(p.VKnots) != p.VCount+p.VDegree+1 {
		return DiagPatch(CodeSchema, pid, "vKnots", "knot vector length does not match control count and degree")
	}
	if d := validateKnots(pid, "uKnots", p.UKnots, p.UDegree, p.UCount); d != nil {
		return d
	}
	if d := validateKnots(pid, "vKnots", p.VKnots, p.VDegree, p.VCount); d != nil {
		return d
	}
	for i, cp := range p.Control {
		field := fmt.Sprintf("controlPoints[%d]", i)
		if cp.Row < 0 || cp.Row >= p.VCount || cp.Col < 0 || cp.Col >= p.UCount {
			return DiagPatch(CodeSchema, pid, field+".row/col", "control point grid coordinate out of range")
		}
		if !cp.Position.IsFinite() {
			return DiagPatch(CodeSchema, pid, field+".position", "control point coordinate must be finite")
		}
		if math.IsNaN(cp.Weight) || math.IsInf(cp.Weight, 0) || cp.Weight <= 0 {
			return DiagPatch(CodeWeight, pid, field+".weight", "control point weight must be positive and finite")
		}
	}
	if len(p.Trim) < 4 {
		return DiagPatch(CodeClip, pid, "trimBoundary", "trim boundary must contain at least four points")
	}
	clipTol := p.Tolerance
	if clipTol <= 0 {
		clipTol = DefaultClipTolerance
	}
	first, last := p.Trim[0], p.Trim[len(p.Trim)-1]
	if math.Hypot(first.U-last.U, first.V-last.V) > clipTol {
		return DiagPatch(CodeClip, pid, "trimBoundary", "trim boundary is not closed")
	}
	for i, pt := range p.Trim {
		if math.IsNaN(pt.U) || math.IsNaN(pt.V) || math.IsInf(pt.U, 0) || math.IsInf(pt.V, 0) {
			return DiagPatch(CodeClip, pid, fmt.Sprintf("trimBoundary[%d]", i), "trim coordinate must be finite")
		}
	}
	return nil
}

func validateKnots(patchID int, field string, knots []float64, degree, count int) *Diagnostic {
	prev := math.Inf(-1)
	positive := 0
	for i, k := range knots {
		if math.IsNaN(k) || math.IsInf(k, 0) {
			return DiagPatch(CodeSchema, patchID, fmt.Sprintf("%s[%d]", field, i), "knot must be finite")
		}
		if k < prev {
			return DiagPatch(CodeKnot, patchID, fmt.Sprintf("%s[%d]", field, i), "knot vector must be nondecreasing")
		}
		if i > 0 && k > prev {
			positive++
		}
		prev = k
	}
	if positive == 0 {
		return DiagPatch(CodeKnot, patchID, field, "knot vector has no positive span")
	}
	if knots[degree] >= knots[count] {
		return DiagPatch(CodeKnot, patchID, field, "parameter domain is empty")
	}
	return nil
}

func ValidateSubdivMesh(m SubdivMesh) *Diagnostic {
	if m.Name == "" {
		return DiagPatch(CodeSchema, -1, fmt.Sprintf("meshes[%d].name", m.ID), "missing subdivision mesh name")
	}
	if len(m.Vertices) == 0 {
		return DiagPatch(CodeSchema, -1, fmt.Sprintf("meshes[%d].vertices", m.ID), "missing subdivision mesh vertices")
	}
	if len(m.Normals) != 0 && len(m.Normals) != len(m.Vertices) {
		return DiagPatch(CodeSchema, -1, fmt.Sprintf("meshes[%d].normals", m.ID), "normal count does not match vertex count")
	}
	if len(m.Indices) == 0 || len(m.Indices)%3 != 0 {
		return DiagPatch(CodeSchema, -1, fmt.Sprintf("meshes[%d].indices", m.ID), "indices must be a non-empty triangle list")
	}
	for i, idx := range m.Indices {
		if idx < 0 || idx >= len(m.Vertices) {
			return DiagPatch(CodeSchema, -1, fmt.Sprintf("meshes[%d].indices[%d]", m.ID, i), "subdivision mesh index out of range")
		}
	}
	for i, v := range m.Vertices {
		if !v.IsFinite() {
			return DiagPatch(CodeSchema, -1, fmt.Sprintf("meshes[%d].vertices[%d]", m.ID, i), "mesh vertex must be finite")
		}
	}
	return nil
}

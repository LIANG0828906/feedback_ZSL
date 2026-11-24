package volume

import (
	"math"

	"volumediag/internal/model"
)

const geomEpsilon = 1e-9

type meshHit struct {
	T      float64
	MeshID string
	Color  model.Color
}

// MeshIntersect returns the nearest positive intersection of a ray with a
// depth mesh. Each cell is a planar patch offset along the mesh normal.
func MeshIntersect(m *model.DepthMesh, origin, dir model.Vec3, maxT float64) (meshHit, bool) {
	normal, ok := m.Normal.Normalize()
	if !ok {
		return meshHit{}, false
	}
	if d, flat := flatDepth(m); flat {
		if t, hit := intersectPatch(m, normal, origin, dir, 0, 1, 0, 1, d); hit && t <= maxT {
			return meshHit{T: t, MeshID: m.ID, Color: m.Color}, true
		}
		return meshHit{}, false
	}
	best := math.Inf(1)
	for row := 0; row < m.Rows; row++ {
		for col := 0; col < m.Columns; col++ {
			u0 := float64(col) / float64(m.Columns)
			u1 := float64(col+1) / float64(m.Columns)
			v0 := float64(row) / float64(m.Rows)
			v1 := float64(row+1) / float64(m.Rows)
			t, hit := intersectPatch(m, normal, origin, dir, u0, u1, v0, v1, m.Depths[row][col])
			if hit && t <= maxT && t < best {
				best = t
			}
		}
	}
	if math.IsInf(best, 1) {
		return meshHit{}, false
	}
	return meshHit{T: best, MeshID: m.ID, Color: m.Color}, true
}

func flatDepth(m *model.DepthMesh) (float64, bool) {
	if len(m.Depths) == 0 || len(m.Depths[0]) == 0 {
		return 0, false
	}
	d := m.Depths[0][0]
	for r := range m.Depths {
		for c := range m.Depths[r] {
			if m.Depths[r][c] != d {
				return 0, false
			}
		}
	}
	return d, true
}

func intersectPatch(m *model.DepthMesh, normal, origin, dir model.Vec3, u0, u1, v0, v1, depth float64) (float64, bool) {
	point := m.Origin.Add(normal.Mul(depth))
	denom := dir.Dot(normal)
	if math.Abs(denom) < geomEpsilon {
		return 0, false
	}
	t := point.Sub(origin).Dot(normal) / denom
	if t < geomEpsilon {
		return 0, false
	}
	p := origin.Add(dir.Mul(t))
	rel := p.Sub(point)
	uLen2 := m.AxisU.Length2()
	vLen2 := m.AxisV.Length2()
	if uLen2 <= geomEpsilon || vLen2 <= geomEpsilon {
		return 0, false
	}
	u := rel.Dot(m.AxisU) / uLen2
	v := rel.Dot(m.AxisV) / vLen2
	const edge = 1e-9
	if u < u0-edge || u > u1+edge || v < v0-edge || v > v1+edge {
		return 0, false
	}
	return t, true
}

func SegmentIntersectsMesh(m *model.DepthMesh, from, to model.Vec3) bool {
	delta := to.Sub(from)
	length := delta.Length()
	if length <= geomEpsilon {
		return false
	}
	dir := delta.Mul(1 / length)
	normal, ok := m.Normal.Normalize()
	if !ok {
		return false
	}
	if d, flat := flatDepth(m); flat {
		t, hit := intersectPatch(m, normal, from, dir, 0, 1, 0, 1, d)
		return hit && t < length-geomEpsilon && t > geomEpsilon
	}
	for row := 0; row < m.Rows; row++ {
		for col := 0; col < m.Columns; col++ {
			u0 := float64(col) / float64(m.Columns)
			u1 := float64(col+1) / float64(m.Columns)
			v0 := float64(row) / float64(m.Rows)
			v1 := float64(row+1) / float64(m.Rows)
			t, hit := intersectPatch(m, normal, from, dir, u0, u1, v0, v1, m.Depths[row][col])
			if hit && t > geomEpsilon && t < length-geomEpsilon {
				return true
			}
		}
	}
	return false
}

func SceneOccluded(scene *model.Scene, light *model.Light, p model.Vec3) bool {
	for _, id := range light.OccluderIDs {
		mesh, idx := scene.FindMesh(id)
		if idx < 0 {
			continue
		}
		if SegmentIntersectsMesh(mesh, p, light.Position) {
			return true
		}
	}
	return false
}

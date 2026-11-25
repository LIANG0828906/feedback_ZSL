// Package geom provides the screen-space triangle test shared by the generator
// and rasterizer.
package geom

import (
	"math"

	"spectralforge/internal/domain"
)

type Hit struct {
	MeshIndex     int
	TriangleIndex int
	ObjectID      string
	Indices       [3]int
	Bary          [3]float64
	Depth         float64
}

func Trace(scene *domain.Scene, x, y float64) (Hit, bool) {
	bestDepth := math.Inf(1)
	var best Hit
	found := false
	for mi := range scene.Meshes {
		mesh := &scene.Meshes[mi]
		for ti := 0; ti+2 < len(mesh.Indices); ti += 3 {
			indices := [3]int{mesh.Indices[ti], mesh.Indices[ti+1], mesh.Indices[ti+2]}
			bary, depth, ok := barycentric(mesh.Vertices, indices, x, y)
			if !ok {
				continue
			}
			if depth < bestDepth {
				bestDepth = depth
				best = Hit{
					MeshIndex:     mi,
					TriangleIndex: ti / 3,
					ObjectID:      mesh.ID,
					Indices:       indices,
					Bary:          bary,
					Depth:         depth,
				}
				found = true
			}
		}
	}
	return best, found
}

func Barycentric(vertices []domain.Vertex, indices [3]int, x, y float64) ([3]float64, float64, bool) {
	return barycentric(vertices, indices, x, y)
}

func barycentric(vertices []domain.Vertex, indices [3]int, x, y float64) ([3]float64, float64, bool) {
	v0 := vertices[indices[0]].Position
	v1 := vertices[indices[1]].Position
	v2 := vertices[indices[2]].Position
	denom := (v1[1]-v2[1])*(v0[0]-v2[0]) + (v2[0]-v1[0])*(v0[1]-v2[1])
	if math.Abs(denom) < 1e-12 {
		return [3]float64{}, 0, false
	}
	w0 := ((v1[1]-v2[1])*(x-v2[0]) + (v2[0]-v1[0])*(y-v2[1])) / denom
	w1 := ((v2[1]-v0[1])*(x-v2[0]) + (v0[0]-v2[0])*(y-v2[1])) / denom
	w2 := 1 - w0 - w1
	const eps = -1e-9
	if w0 < eps || w1 < eps || w2 < eps {
		return [3]float64{}, 0, false
	}
	depth := w0*v0[2] + w1*v1[2] + w2*v2[2]
	return [3]float64{w0, w1, w2}, depth, true
}

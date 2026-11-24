package render

import (
	"math"

	"cadlab/internal/geom"
	"cadlab/internal/model"
)

// PatchCoverage rasterizes the actual screen footprint of one patch into tile
// IDs. uvRange optionally restricts triangles to a parameter rectangle.
func PatchCoverage(scene *Scene, patchID int, uvRange *[4]float64) []int {
	var patch *PatchDraw
	for i := range scene.Patches {
		if scene.Patches[i].PatchID == patchID {
			patch = &scene.Patches[i]
			break
		}
	}
	if patch == nil || patch.Mesh == nil {
		return nil
	}
	flags := make([]bool, scene.Tiles.Count())
	vb := newViewBasis(scene.Camera)
	m := patch.Mesh
	for i := 0; i+2 < len(m.Indices); i += 3 {
		a := m.Vertices[m.Indices[i]]
		b := m.Vertices[m.Indices[i+1]]
		c := m.Vertices[m.Indices[i+2]]
		if uvRange != nil && !triIntersectsRange(a.UV, b.UV, c.UV, *uvRange) {
			continue
		}
		tri := rasterTri{
			p: [3]model.Vec3{a.Position, b.Position, c.Position},
			n: [3]model.Vec3{a.Normal, b.Normal, c.Normal},
		}
		for _, poly := range clipTriangle(tri, scene.Clip) {
			for j := 1; j+1 < len(poly); j++ {
				var p [3]screenVertex
				ok := true
				for k, src := range []int{0, j, j + 1} {
					sp, valid := vb.project(poly[src].p, poly[src].n)
					if !valid {
						ok = false
						break
					}
					p[k] = sp
				}
				if ok {
					markCoverage(scene.Tiles, flags, p)
				}
			}
		}
	}
	var out []int
	for id, hit := range flags {
		if hit {
			out = append(out, id)
		}
	}
	return out
}

func triIntersectsRange(a, b, c model.Vec2, r [4]float64) bool {
	minU := math.Min(a.U, math.Min(b.U, c.U))
	maxU := math.Max(a.U, math.Max(b.U, c.U))
	minV := math.Min(a.V, math.Min(b.V, c.V))
	maxV := math.Max(a.V, math.Max(b.V, c.V))
	return maxU >= r[0] && minU <= r[1] && maxV >= r[2] && minV <= r[3]
}

func markCoverage(grid model.TileGrid, flags []bool, p [3]screenVertex) {
	area := edge(p[0].x, p[0].y, p[1].x, p[1].y, p[2].x, p[2].y)
	if math.Abs(area) < 1e-10 {
		return
	}
	minX := int(math.Floor(math.Min(p[0].x, math.Min(p[1].x, p[2].x))))
	maxX := int(math.Ceil(math.Max(p[0].x, math.Max(p[1].x, p[2].x))))
	minY := int(math.Floor(math.Min(p[0].y, math.Min(p[1].y, p[2].y))))
	maxY := int(math.Ceil(math.Max(p[0].y, math.Max(p[1].y, p[2].y))))
	minX = maxInt(0, minX)
	maxX = minInt(grid.Width, maxX)
	minY = maxInt(0, minY)
	maxY = minInt(grid.Height, maxY)
	if minX >= maxX || minY >= maxY {
		return
	}
	c0, c1 := minX*grid.Cols/grid.Width, (maxX-1)*grid.Cols/grid.Width
	r0, r1 := minY*grid.Rows/grid.Height, (maxY-1)*grid.Rows/grid.Height
	for row := r0; row <= r1; row++ {
		for col := c0; col <= c1; col++ {
			id := row*grid.Cols + col
			if flags[id] {
				continue
			}
			x0, y0, x1, y1 := grid.Rect(id)
			inside := false
			for y := y0; y < y1 && !inside; y++ {
				for x := x0; x < x1; x++ {
					px, py := float64(x)+0.5, float64(y)+0.5
					w0 := edge(p[1].x, p[1].y, p[2].x, p[2].y, px, py) / area
					w1 := edge(p[2].x, p[2].y, p[0].x, p[0].y, px, py) / area
					w2 := 1 - w0 - w1
					if w0 >= -1e-9 && w1 >= -1e-9 && w2 >= -1e-9 {
						inside = true
						break
					}
				}
			}
			if inside {
				flags[id] = true
			}
		}
	}
}

func _unusedGeomVertex(_ geom.Vertex) {}

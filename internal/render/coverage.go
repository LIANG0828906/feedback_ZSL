package render

import (
	"math"

	"cadpipe/internal/cad"
	"cadpipe/internal/tessellate"
)

func boundaryEdges(mesh *tessellate.Mesh) [][2]int {
	counts := make(map[[2]int]int)
	for _, tri := range mesh.Triangles {
		edges := [][2]int{{tri[0], tri[1]}, {tri[1], tri[2]}, {tri[2], tri[0]}}
		for _, e := range edges {
			if e[0] > e[1] {
				e[0], e[1] = e[1], e[0]
			}
			counts[e]++
		}
	}
	var out [][2]int
	for e, n := range counts {
		if n == 1 {
			out = append(out, e)
		}
	}
	return out
}

func markOutline(
	cam *Camera,
	projected []ProjectedVertex,
	mesh *tessellate.Mesh,
	x0, y0, x1, y1, stride int,
	patchOutline, outlineMask []bool,
) {
	for _, edge := range boundaryEdges(mesh) {
		a := projected[edge[0]]
		b := projected[edge[1]]
		if a.Depth == math.MaxFloat64 || b.Depth == math.MaxFloat64 {
			continue
		}
		minX := maxInt(x0, int(math.Floor(math.Min(a.ScreenX, b.ScreenX)))-1)
		maxX := minInt(x1-1, int(math.Ceil(math.Max(a.ScreenX, b.ScreenX)))+1)
		minY := maxInt(y0, int(math.Floor(math.Min(a.ScreenY, b.ScreenY)))-1)
		maxY := minInt(y1-1, int(math.Ceil(math.Max(a.ScreenY, b.ScreenY)))+1)
		for py := minY; py <= maxY; py++ {
			for px := minX; px <= maxX; px++ {
				p := cad.Vec2{U: float64(px) + 0.5, V: float64(py) + 0.5}
				da := cad.Vec2{U: a.ScreenX, V: a.ScreenY}
				db := cad.Vec2{U: b.ScreenX, V: b.ScreenY}
				if distancePointSegment2(p, da, db) <= 1.15 {
					idx := (py-y0)*stride + (px - x0)
					if idx >= 0 && idx < len(patchOutline) {
						patchOutline[idx] = true
						outlineMask[idx] = true
					}
				}
			}
		}
	}
}

func distancePointSegment2(p, a, b cad.Vec2) float64 {
	vx, vy := b.U-a.U, b.V-a.V
	wx, wy := p.U-a.U, p.V-a.V
	l2 := vx*vx + vy*vy
	if l2 == 0 {
		return math.Hypot(p.U-a.U, p.V-a.V)
	}
	t := math.Max(0, math.Min(1, (wx*vx+wy*vy)/l2))
	return math.Hypot(p.U-(a.U+t*vx), p.V-(a.V+t*vy))
}

func ExactCoveredTiles(cam *Camera, patch *cad.Patch, mesh *tessellate.Mesh) map[int]bool {
	_, tris := projectPatch(cam, mesh)
	out := make(map[int]bool)
	nx := cam.TileCountX()
	for _, tri := range tris {
		a, b, c := tri.vertices[0], tri.vertices[1], tri.vertices[2]
		minX := maxInt(0, int(math.Floor(math.Min(math.Min(a.ScreenX, b.ScreenX), c.ScreenX))))
		maxX := minInt(cam.Width-1, int(math.Ceil(math.Max(math.Max(a.ScreenX, b.ScreenX), c.ScreenX))))
		minY := maxInt(0, int(math.Floor(math.Min(math.Min(a.ScreenY, b.ScreenY), c.ScreenY))))
		maxY := minInt(cam.Height-1, int(math.Ceil(math.Max(math.Max(a.ScreenY, b.ScreenY), c.ScreenY))))
		for py := minY; py <= maxY; py++ {
			for px := minX; px <= maxX; px++ {
				q := cad.Vec2{U: float64(px) + 0.5, V: float64(py) + 0.5}
				_, _, _, ok := barycentric(q,
					cad.Vec2{U: a.ScreenX, V: a.ScreenY},
					cad.Vec2{U: b.ScreenX, V: b.ScreenY},
					cad.Vec2{U: c.ScreenX, V: c.ScreenY})
				if !ok {
					continue
				}
				tx := px / cam.TileWidth
				ty := py / cam.TileHeight
				if tx < 0 || tx >= cam.TileCountX() || ty < 0 || ty >= cam.TileCountY() {
					continue
				}
				out[ty*nx+tx] = true
			}
		}
	}
	return out
}

func ProbeTile(cam *Camera, patch *cad.Patch, u, v float64) (int, bool) {
	p := tessellate.Evaluate(patch, u, v)
	screen, ok := cam.Project(tessellate.Vertex{Position: p, UV: cad.Vec2{U: u, V: v}})
	if !ok {
		return 0, false
	}
	if screen.ScreenX < 0 || screen.ScreenX >= float64(cam.Width) ||
		screen.ScreenY < 0 || screen.ScreenY >= float64(cam.Height) {
		return 0, false
	}
	tx := int(screen.ScreenX) / cam.TileWidth
	ty := int(screen.ScreenY) / cam.TileHeight
	return cam.TileID(tx, ty), true
}

package render

import (
	"math"

	"cadpipe/internal/cad"
	"cadpipe/internal/tessellate"
)

type Intrinsics struct {
	Fx float64 `json:"fx"`
	Fy float64 `json:"fy"`
	Cx float64 `json:"cx"`
	Cy float64 `json:"cy"`
}

type Camera struct {
	Width      int        `json:"width"`
	Height     int        `json:"height"`
	TileWidth  int        `json:"tile_width"`
	TileHeight int        `json:"tile_height"`
	Eye        cad.Vec3   `json:"eye"`
	Target     cad.Vec3   `json:"target"`
	Up         cad.Vec3   `json:"up"`
	Near       float64    `json:"near"`
	Far        float64    `json:"far"`
	Intrinsics Intrinsics `json:"intrinsics"`
	Version    int64      `json:"version"`
	forward    cad.Vec3
	right      cad.Vec3
	up         cad.Vec3
}

func FitCamera(scene *cad.Scene, width, height, tile int) *Camera {
	min := cad.Vec3{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)}
	max := cad.Vec3{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)}
	for _, p := range scene.Patches {
		mesh, err := tessellate.Tessellate(p, nil)
		if err != nil {
			continue
		}
		for _, v := range mesh.Vertices {
			q := v.Position
			min.X = math.Min(min.X, q.X)
			min.Y = math.Min(min.Y, q.Y)
			min.Z = math.Min(min.Z, q.Z)
			max.X = math.Max(max.X, q.X)
			max.Y = math.Max(max.Y, q.Y)
			max.Z = math.Max(max.Z, q.Z)
		}
	}
	center := cad.Vec3{X: (min.X + max.X) / 2, Y: (min.Y + max.Y) / 2, Z: (min.Z + max.Z) / 2}
	halfW := math.Max((max.X-min.X)/2, 0.01)
	halfH := math.Max((max.Y-min.Y)/2, 0.01)
	f := math.Min(float64(width), float64(height)) * 1.75
	fx, fy := f, f
	margin := 20.0
	dx := fx * halfW / (float64(width)/2 - margin)
	dy := fy * halfH / (float64(height)/2 - margin)
	distance := math.Max(math.Max(dx, dy)*1.12, 0.08)
	cam := &Camera{
		Width: width, Height: height, TileWidth: tile, TileHeight: tile,
		Eye:    cad.Vec3{X: center.X, Y: center.Y, Z: center.Z + distance},
		Target: center, Up: cad.Vec3{X: 0, Y: 1, Z: 0},
		Near: 0.01, Far: 10,
		Intrinsics: Intrinsics{Fx: fx, Fy: fy, Cx: float64(width) / 2, Cy: float64(height) / 2},
		Version:    1,
	}
	cam.rebuildBasis()
	return cam
}

func (c *Camera) rebuildBasis() {
	c.forward = normalize3(sub3(c.Target, c.Eye))
	c.right = normalize3(cross3(c.forward, c.Up))
	c.up = cross3(c.right, c.forward)
}

func (c *Camera) TileCountX() int { return (c.Width + c.TileWidth - 1) / c.TileWidth }
func (c *Camera) TileCountY() int { return (c.Height + c.TileHeight - 1) / c.TileHeight }
func (c *Camera) TileCount() int  { return c.TileCountX() * c.TileCountY() }

func (c *Camera) TileID(tx, ty int) int { return ty*c.TileCountX() + tx }

func (c *Camera) TileXY(id int) (int, int) {
	nx := c.TileCountX()
	return id % nx, id / nx
}

func (c *Camera) TileRect(id int) (x0, y0, x1, y1 int) {
	tx, ty := c.TileXY(id)
	x0 = tx * c.TileWidth
	y0 = ty * c.TileHeight
	x1 = minInt(x0+c.TileWidth, c.Width)
	y1 = minInt(y0+c.TileHeight, c.Height)
	return
}

type ProjectedVertex struct {
	ScreenX float64
	ScreenY float64
	Depth   float64
	World   cad.Vec3
	Normal  cad.Vec3
	UV      cad.Vec2
}

func (c *Camera) Project(v tessellate.Vertex) (ProjectedVertex, bool) {
	if c.forward.X == 0 && c.forward.Y == 0 && c.forward.Z == 0 {
		c.rebuildBasis()
	}
	rel := sub3(v.Position, c.Eye)
	z := dot3(rel, c.forward)
	if z < c.Near || z > c.Far {
		return ProjectedVertex{}, false
	}
	xc := dot3(rel, c.right)
	yc := dot3(rel, c.up)
	return ProjectedVertex{
		ScreenX: c.Intrinsics.Cx + c.Intrinsics.Fx*xc/z,
		ScreenY: c.Intrinsics.Cy - c.Intrinsics.Fy*yc/z,
		Depth:   z, World: v.Position, Normal: v.Normal, UV: v.UV,
	}, true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sub3(a, b cad.Vec3) cad.Vec3 {
	return cad.Vec3{X: a.X - b.X, Y: a.Y - b.Y, Z: a.Z - b.Z}
}
func add3(a, b cad.Vec3) cad.Vec3 {
	return cad.Vec3{X: a.X + b.X, Y: a.Y + b.Y, Z: a.Z + b.Z}
}
func scale3(a cad.Vec3, s float64) cad.Vec3 {
	return cad.Vec3{X: a.X * s, Y: a.Y * s, Z: a.Z * s}
}
func dot3(a, b cad.Vec3) float64 { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }
func cross3(a, b cad.Vec3) cad.Vec3 {
	return cad.Vec3{X: a.Y*b.Z - a.Z*b.Y, Y: a.Z*b.X - a.X*b.Z, Z: a.X*b.Y - a.Y*b.X}
}
func len3(a cad.Vec3) float64 { return math.Sqrt(dot3(a, a)) }
func normalize3(a cad.Vec3) cad.Vec3 {
	l := len3(a)
	if l == 0 {
		return cad.Vec3{}
	}
	return scale3(a, 1/l)
}

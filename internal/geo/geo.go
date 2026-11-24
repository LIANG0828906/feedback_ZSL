package geo

import (
	"math"

	"terrain-occlusion/internal/model"
)

type Vec3 = model.Vec3
type Box3 = model.Box3

func Add(a, b Vec3) Vec3 {
	return Vec3{X: a.X + b.X, Y: a.Y + b.Y, Z: a.Z + b.Z}
}
func Sub(a, b Vec3) Vec3 {
	return Vec3{X: a.X - b.X, Y: a.Y - b.Y, Z: a.Z - b.Z}
}
func Scale(a Vec3, s float64) Vec3 {
	return Vec3{X: a.X * s, Y: a.Y * s, Z: a.Z * s}
}
func Dot(a, b Vec3) float64 { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }
func Cross(a, b Vec3) Vec3 {
	return Vec3{
		X: a.Y*b.Z - a.Z*b.Y,
		Y: a.Z*b.X - a.X*b.Z,
		Z: a.X*b.Y - a.Y*b.X,
	}
}
func Norm(a Vec3) float64 { return math.Sqrt(Dot(a, a)) }
func Normalize(a Vec3) Vec3 {
	n := Norm(a)
	if n == 0 {
		return Vec3{}
	}
	return Scale(a, 1/n)
}
func Lerp(a, b Vec3, t float64) Vec3 {
	return Vec3{
		X: a.X + (b.X-a.X)*t,
		Y: a.Y + (b.Y-a.Y)*t,
		Z: a.Z + (b.Z-a.Z)*t,
	}
}

func BoxCorners(b Box3) [8]Vec3 {
	return [8]Vec3{
		{X: b.Min.X, Y: b.Min.Y, Z: b.Min.Z},
		{X: b.Max.X, Y: b.Min.Y, Z: b.Min.Z},
		{X: b.Min.X, Y: b.Max.Y, Z: b.Min.Z},
		{X: b.Max.X, Y: b.Max.Y, Z: b.Min.Z},
		{X: b.Min.X, Y: b.Min.Y, Z: b.Max.Z},
		{X: b.Max.X, Y: b.Min.Y, Z: b.Max.Z},
		{X: b.Min.X, Y: b.Max.Y, Z: b.Max.Z},
		{X: b.Max.X, Y: b.Max.Y, Z: b.Max.Z},
	}
}

func RectCorners(r model.Rect2, y float64) [4]Vec3 {
	return [4]Vec3{
		{X: r.MinX, Y: y, Z: r.MinZ},
		{X: r.MaxX, Y: y, Z: r.MinZ},
		{X: r.MaxX, Y: y, Z: r.MaxZ},
		{X: r.MinX, Y: y, Z: r.MaxZ},
	}
}

// Camera is a simple perspective camera with depth along the forward vector.
type Camera struct {
	Eye     Vec3
	Forward Vec3
	Right   Vec3
	Up      Vec3
	TanX    float64
	TanY    float64
	Near    float64
	Far     float64
	Width   int
	Height  int
}

func NewCamera(s model.CameraSample, width, height int) Camera {
	f := Normalize(Sub(s.Target, s.Position))
	worldUp := Vec3{X: 0, Y: 1, Z: 0}
	r := Normalize(Cross(f, worldUp))
	if Norm(r) < 1e-9 {
		r = Vec3{X: 1, Z: 0}
	}
	u := Cross(r, f)
	tanY := math.Tan(s.FovDeg * math.Pi / 360.0)
	aspect := float64(width) / math.Max(1, float64(height))
	return Camera{
		Eye: s.Position, Forward: f, Right: r, Up: u,
		TanX: tanY * aspect, TanY: tanY,
		Near: s.Near, Far: s.Far, Width: width, Height: height,
	}
}

func (c Camera) ToCamera(p Vec3) Vec3 {
	v := Sub(p, c.Eye)
	return Vec3{X: Dot(v, c.Right), Y: Dot(v, c.Up), Z: Dot(v, c.Forward)}
}

func (c Camera) Project(p Vec3) (screenX, screenY, depth, ndcX, ndcY float64, ok bool) {
	q := c.ToCamera(p)
	if q.Z <= c.Near {
		return 0, 0, q.Z, 0, 0, false
	}
	ndcX = q.X / (q.Z * c.TanX)
	ndcY = q.Y / (q.Z * c.TanY)
	screenX = (ndcX*0.5 + 0.5) * float64(c.Width)
	screenY = (1 - (ndcY*0.5 + 0.5)) * float64(c.Height)
	return screenX, screenY, q.Z, ndcX, ndcY, q.Z <= c.Far && ndcX >= -1.25 && ndcX <= 1.25 && ndcY >= -1.25 && ndcY <= 1.25
}

// FrustumMargin returns a conservative signed margin. Positive values mean
// the camera-space AABB is confidently inside all frustum planes.
func (c Camera) FrustumMargin(b Box3) (margin float64, inside bool, screen [4]float64) {
	corners := BoxCorners(b)
	var minQ, maxQ Vec3
	for i, p := range corners {
		q := c.ToCamera(p)
		if i == 0 {
			minQ, maxQ = q, q
		} else {
			minQ = Vec3{X: math.Min(minQ.X, q.X), Y: math.Min(minQ.Y, q.Y), Z: math.Min(minQ.Z, q.Z)}
			maxQ = Vec3{X: math.Max(maxQ.X, q.X), Y: math.Max(maxQ.Y, q.Y), Z: math.Max(maxQ.Z, q.Z)}
		}
	}

	planes := []struct{ a, b, cc, d float64 }{
		{1, 0, c.TanX, 0},  // left:  x + z*tanX >= 0
		{-1, 0, c.TanX, 0}, // right: -x + z*tanX >= 0
		{0, 1, c.TanY, 0},  // bottom
		{0, -1, c.TanY, 0}, // top
		{0, 0, 1, -c.Near}, // near
		{0, 0, -1, c.Far},  // far
	}
	margin = math.Inf(1)
	allInside := true
	for _, p := range planes {
		normalLen := math.Sqrt(p.a*p.a + p.b*p.b + p.cc*p.cc)
		a, bb, cc, d := p.a/normalLen, p.b/normalLen, p.cc/normalLen, p.d/normalLen
		minVal := d
		if a >= 0 {
			minVal += a * minQ.X
		} else {
			minVal += a * maxQ.X
		}
		if bb >= 0 {
			minVal += bb * minQ.Y
		} else {
			minVal += bb * maxQ.Y
		}
		if cc >= 0 {
			minVal += cc * minQ.Z
		} else {
			minVal += cc * maxQ.Z
		}
		margin = math.Min(margin, minVal)
		if minVal < 0 {
			allInside = false
		}
	}

	// Project all corners for a stable screen-space bounding rectangle.
	screen = [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	any := false
	for _, p := range corners {
		x, y, z, _, _, ok := c.Project(p)
		if z <= 0 {
			continue
		}
		any = true
		_ = ok
		screen[0] = math.Min(screen[0], x)
		screen[1] = math.Min(screen[1], y)
		screen[2] = math.Max(screen[2], x)
		screen[3] = math.Max(screen[3], y)
	}
	if !any {
		screen = [4]float64{}
	}
	return margin, allInside, screen
}

// LightDirection converts declared azimuth/elevation to ray travel direction.
func LightDirection(azimuthDeg, elevationDeg float64) Vec3 {
	az := azimuthDeg * math.Pi / 180
	el := elevationDeg * math.Pi / 180
	return Normalize(Vec3{
		X: math.Cos(el) * math.Sin(az),
		Y: -math.Sin(el),
		Z: math.Cos(el) * math.Cos(az),
	})
}

type LightRig struct {
	Dir    Vec3
	Center Vec3
	Right  Vec3
	Up     Vec3
	MinX   float64
	MaxX   float64
	MinY   float64
	MaxY   float64
	MinZ   float64
	MaxZ   float64
	Size   int
}

func NewLightRig(t model.Terrain, size int, az, el float64) LightRig {
	dir := LightDirection(az, el)
	right := Normalize(Cross(dir, Vec3{Y: 1}))
	up := Cross(right, dir)
	width := float64(t.WidthCells) * t.CellSize
	depth := float64(t.DepthCells) * t.CellSize
	center := Vec3{X: width / 2, Y: t.HeightScale * 0.45, Z: depth / 2}

	var minX, maxX, minY, maxY, minZ, maxZ = math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for iz := 0; iz <= t.DepthCells; iz++ {
		for ix := 0; ix <= t.WidthCells; ix++ {
			p := TerrainVertex(t, ix, iz)
			v := Sub(p, center)
			lx, ly, lz := Dot(v, right), Dot(v, up), Dot(v, dir)
			minX, maxX = math.Min(minX, lx), math.Max(maxX, lx)
			minY, maxY = math.Min(minY, ly), math.Max(maxY, ly)
			minZ, maxZ = math.Min(minZ, lz), math.Max(maxZ, lz)
		}
	}
	padX := math.Max(1, (maxX-minX)*0.08)
	padY := math.Max(1, (maxY-minY)*0.08)
	padZ := math.Max(1, (maxZ-minZ)*0.02)
	return LightRig{
		Dir: dir, Center: center, Right: right, Up: up, Size: size,
		MinX: minX - padX, MaxX: maxX + padX,
		MinY: minY - padY, MaxY: maxY + padY,
		MinZ: minZ - padZ, MaxZ: maxZ + padZ,
	}
}

func TerrainVertex(t model.Terrain, ix, iz int) Vec3 {
	idx := iz*(t.WidthCells+1) + ix
	h := 0.0
	if idx >= 0 && idx < len(t.Heights) {
		h = t.Heights[idx]
	}
	return Vec3{X: float64(ix) * t.CellSize, Y: h, Z: float64(iz) * t.CellSize}
}

func (r LightRig) Project(p Vec3) (px, py, depth float64) {
	v := Sub(p, r.Center)
	lx := Dot(v, r.Right)
	ly := Dot(v, r.Up)
	lz := Dot(v, r.Dir)
	u := (lx - r.MinX) / (r.MaxX - r.MinX) * float64(r.Size-1)
	vv := 1 - (ly-r.MinY)/(r.MaxY-r.MinY)
	py = vv * float64(r.Size-1)
	depth = (lz - r.MinZ) / (r.MaxZ - r.MinZ)
	return u, py, depth
}

func (r LightRig) DepthAt(p Vec3) (mapDepth, targetDepth float64, ok bool) {
	px, py, d := r.Project(p)
	x := int(math.Round(px))
	y := int(math.Round(py))
	if x < 0 || y < 0 || x >= r.Size || y >= r.Size {
		return 0, d, false
	}
	return 0, d, true
}

type DepthMap struct {
	Resolution int       `json:"resolution"`
	Depth      []float64 `json:"depth"`
	Rig        LightRig  `json:"-"`
}

// RenderShadowMap rasterizes terrain quads from the declared light direction.
func RenderShadowMap(project model.Project) *DepthMap {
	p := project.Parameters
	r := NewLightRig(project.Terrain, p.ShadowResolution, p.LightAzimuthDeg, p.LightElevationDeg)
	m := &DepthMap{Resolution: p.ShadowResolution, Depth: make([]float64, p.ShadowResolution*p.ShadowResolution), Rig: r}
	for i := range m.Depth {
		m.Depth[i] = 1
	}
	t := project.Terrain
	for iz := 0; iz < t.DepthCells; iz++ {
		for ix := 0; ix < t.WidthCells; ix++ {
			a := TerrainVertex(t, ix, iz)
			b := TerrainVertex(t, ix+1, iz)
			c := TerrainVertex(t, ix+1, iz+1)
			d := TerrainVertex(t, ix, iz+1)
			m.rasterTriangle(a, b, c)
			m.rasterTriangle(a, c, d)
		}
	}
	return m
}

func (m *DepthMap) rasterTriangle(a0, b0, c0 Vec3) {
	type pv struct {
		x, y, z float64
		p       Vec3
	}
	aX, aY, aZ := m.Rig.Project(a0)
	bX, bY, bZ := m.Rig.Project(b0)
	cX, cY, cZ := m.Rig.Project(c0)
	a := pv{aX, aY, aZ, a0}
	b := pv{bX, bY, bZ, b0}
	c := pv{cX, cY, cZ, c0}
	minX := int(math.Max(0, math.Floor(math.Min(a.x, math.Min(b.x, c.x)))))
	maxX := int(math.Min(float64(m.Resolution-1), math.Ceil(math.Max(a.x, math.Max(b.x, c.x)))))
	minY := int(math.Max(0, math.Floor(math.Min(a.y, math.Min(b.y, c.y)))))
	maxY := int(math.Min(float64(m.Resolution-1), math.Ceil(math.Max(a.y, math.Max(b.y, c.y)))))
	area := (b.x-a.x)*(c.y-a.y) - (b.y-a.y)*(c.x-a.x)
	if math.Abs(area) < 1e-9 {
		return
	}
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			px := float64(x) + 0.5
			py := float64(y) + 0.5
			w0 := ((b.x-px)*(c.y-py) - (b.y-py)*(c.x-px)) / area
			w1 := ((c.x-px)*(a.y-py) - (c.y-py)*(a.x-px)) / area
			w2 := 1 - w0 - w1
			if w0 < -1e-6 || w1 < -1e-6 || w2 < -1e-6 {
				continue
			}
			z := w0*a.z + w1*b.z + w2*c.z
			idx := y*m.Resolution + x
			if z < m.Depth[idx] {
				m.Depth[idx] = z
			}
		}
	}
}

// ShadowOcclusionMargin returns targetDepth-mapDepth at the nearest texel.
// Positive means the probe is farther from the light than the blocker.
func (m *DepthMap) ShadowOcclusionMargin(p Vec3) (margin, mapDepth, targetDepth float64, ok bool) {
	px, py, td := m.Rig.Project(p)
	x := int(math.Round(px))
	y := int(math.Round(py))
	if x < 1 || y < 1 || x >= m.Resolution-1 || y >= m.Resolution-1 {
		return 0, 0, td, false
	}
	// 3x3 minimum gives a conservative blocker depth.
	md := 1.0
	for yy := y - 1; yy <= y+1; yy++ {
		for xx := x - 1; xx <= x+1; xx++ {
			md = math.Min(md, m.Depth[yy*m.Resolution+xx])
		}
	}
	return td - md, md, td, true
}

func QuantizeFixed(v, step float64) float64 {
	if step <= 0 {
		return v
	}
	return math.Floor(v/step) * step
}

func IntsToSortedSet(ids []int) map[int]bool {
	out := map[int]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

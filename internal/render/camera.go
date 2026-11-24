package render

import (
	"math"

	"cadlab/internal/model"
)

type viewBasis struct {
	eye, forward, right, up model.Vec3
	focal, cx, cy           float64
	near, far               float64
}

func newViewBasis(c model.Camera) viewBasis {
	forward := c.Target.Sub(c.Eye).Normalize()
	right := forward.Cross(c.Up).Normalize()
	if right == (model.Vec3{}) {
		right = forward.Cross(model.Vec3{Z: 1}).Normalize()
	}
	up := right.Cross(forward).Normalize()
	focal := 1 / math.Tan(c.FovY*math.Pi/360)
	return viewBasis{
		eye: c.Eye, forward: forward, right: right, up: up,
		focal: focal, cx: float64(c.Width) / 2, cy: float64(c.Height) / 2,
		near: c.Near, far: c.Far,
	}
}

type screenVertex struct {
	x, y, z float64
	world   model.Vec3
	normal  model.Vec3
}

func (v viewBasis) project(p model.Vec3, n model.Vec3) (screenVertex, bool) {
	rel := p.Sub(v.eye)
	z := rel.Dot(v.forward)
	if z <= v.near || z >= v.far {
		return screenVertex{}, false
	}
	x := rel.Dot(v.right)
	y := rel.Dot(v.up)
	sx := v.cx + x*v.focal*v.cy/z
	sy := v.cy - y*v.focal*v.cy/z
	ndc := (z - v.near) / (v.far - v.near)
	return screenVertex{x: sx, y: sy, z: ndc, world: p, normal: n}, true
}

func (v viewBasis) depth(p model.Vec3) float64 {
	rel := p.Sub(v.eye)
	return rel.Dot(v.forward)
}

// ProjectPoint exposes the deterministic camera projection for diagnostics
// overlays such as profile measurements.
func ProjectPoint(c model.Camera, p model.Vec3) (x, y float64, visible bool) {
	v := newViewBasis(c)
	sv, ok := v.project(p, model.Vec3{})
	if !ok {
		return 0, 0, false
	}
	return sv.x, sv.y, true
}

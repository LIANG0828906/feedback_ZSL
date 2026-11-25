package fog

import (
	"fmt"
	"math"
)

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3 { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3 { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Mul(s float64) Vec3 {
	return Vec3{v.X * s, v.Y * s, v.Z * s}
}
func (v Vec3) Neg() Vec3          { return v.Mul(-1) }
func (v Vec3) Dot(o Vec3) float64 { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }
func (v Vec3) Len() float64       { return math.Sqrt(v.Dot(v)) }

func (v Vec3) Normalize() Vec3 {
	n := v.Len()
	if n == 0 {
		return Vec3{}
	}
	return v.Mul(1 / n)
}

type AABB struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

func (b AABB) Size() Vec3 { return b.Max.Sub(b.Min) }

func (b AABB) Contains(p Vec3) bool {
	return p.X >= b.Min.X && p.X <= b.Max.X &&
		p.Y >= b.Min.Y && p.Y <= b.Max.Y &&
		p.Z >= b.Min.Z && p.Z <= b.Max.Z
}

// IntersectRay returns Euclidean ray parameters for a normalized direction.
// The interval is inclusive and uses the robust slab formulation.
func (b AABB) IntersectRay(origin, dir Vec3) (tMin, tMax float64, ok bool) {
	tMin = -math.MaxFloat64
	tMax = math.MaxFloat64

	axes := [3][2]float64{
		{origin.X, dir.X},
		{origin.Y, dir.Y},
		{origin.Z, dir.Z},
	}
	bounds := [3][2]float64{
		{b.Min.X, b.Max.X},
		{b.Min.Y, b.Max.Y},
		{b.Min.Z, b.Max.Z},
	}
	for axis := 0; axis < 3; axis++ {
		o := axes[axis][0]
		d := axes[axis][1]
		lo := bounds[axis][0]
		hi := bounds[axis][1]
		if math.Abs(d) < 1e-15 {
			if o < lo || o > hi {
				return 0, 0, false
			}
			continue
		}
		t1 := (lo - o) / d
		t2 := (hi - o) / d
		if t1 > t2 {
			t1, t2 = t2, t1
		}
		if t1 > tMin {
			tMin = t1
		}
		if t2 < tMax {
			tMax = t2
		}
		if tMin > tMax+1e-12 || tMax < -1e-12 {
			return 0, 0, false
		}
	}
	if tMax < 0 {
		return 0, 0, false
	}
	if tMin < 0 {
		tMin = 0
	}
	if tMin > tMax {
		tMin, tMax = tMax, tMin
	}
	return tMin, tMax, true
}

type Ray struct {
	ID     string  `json:"id"`
	PixelX int     `json:"pixel_x"`
	PixelY int     `json:"pixel_y"`
	TileID int     `json:"tile_id"`
	Origin Vec3    `json:"origin"`
	Dir    Vec3    `json:"dir"`
	Near   float64 `json:"near"`
	Far    float64 `json:"far"`
}

func (r Ray) Point(t float64) Vec3 { return r.Origin.Add(r.Dir.Mul(t)) }

func (r Ray) String() string {
	return fmt.Sprintf("Ray(%s px=%d,%d o=%.4f,%.4f,%.4f d=%.4f,%.4f,%.4f)",
		r.ID, r.PixelX, r.PixelY, r.Origin.X, r.Origin.Y, r.Origin.Z, r.Dir.X, r.Dir.Y, r.Dir.Z)
}

// Camera is a simple perspective camera. Right and Up are derived from
// Yaw/Pitch by CameraState, but retained for stable snapshots.
type Camera struct {
	ID       string  `json:"id"`
	Time     float64 `json:"time"`
	Position Vec3    `json:"position"`
	Forward  Vec3    `json:"forward"`
	Right    Vec3    `json:"right"`
	Up       Vec3    `json:"up"`
	FovY     float64 `json:"fov_y_degrees"`
	Near     float64 `json:"near"`
	Far      float64 `json:"far"`
	Version  string  `json:"version"`
}

type CameraTrajectory struct {
	Cameras []Camera `json:"cameras"`
}

// RayForPixel uses pixel-center sampling. The +0.5 convention is deliberate:
// tile ownership is derived from integer pixel coordinates before this offset,
// so half-pixel samples cannot drift into an adjacent tile.
func (c Camera) RayForPixel(pixelX, pixelY, width, height, tileID int) Ray {
	forward := c.Forward.Normalize()
	right := c.Right.Normalize()
	up := c.Up.Normalize()
	aspect := float64(width) / float64(height)
	scale := math.Tan((c.FovY * math.Pi / 180) / 2)
	u := (float64(pixelX) + 0.5 - float64(width)/2) / (float64(width) / 2)
	v := (float64(pixelY) + 0.5 - float64(height)/2) / (float64(height) / 2)
	dir := forward.
		Add(right.Mul(u * scale * aspect)).
		Add(up.Mul(-v * scale)).
		Normalize()
	return Ray{
		ID:     fmt.Sprintf("cam-%s-px-%04d-%04d", c.ID, pixelX, pixelY),
		PixelX: pixelX,
		PixelY: pixelY,
		TileID: tileID,
		Origin: c.Position,
		Dir:    dir,
		Near:   c.Near,
		Far:    c.Far,
	}
}

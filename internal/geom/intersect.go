package geom

import (
	"math"

	"volumekernel/internal/model"
)

type Interval struct {
	T0 float64
	T1 float64
}

func RayAABB(origin, dir model.Vec3, box model.AABB) (Interval, bool) {
	tmin := math.Inf(-1)
	tmax := math.Inf(1)
	axes := [3]struct {
		o, d, lo, hi float64
	}{
		{origin.X, dir.X, box.Min.X, box.Max.X},
		{origin.Y, dir.Y, box.Min.Y, box.Max.Y},
		{origin.Z, dir.Z, box.Min.Z, box.Max.Z},
	}
	for _, a := range axes {
		if math.Abs(a.d) < 1e-15 {
			if a.o < a.lo || a.o > a.hi {
				return Interval{}, false
			}
			continue
		}
		inv := 1 / a.d
		t0 := (a.lo - a.o) * inv
		t1 := (a.hi - a.o) * inv
		if t0 > t1 {
			t0, t1 = t1, t0
		}
		if t0 > tmin {
			tmin = t0
		}
		if t1 < tmax {
			tmax = t1
		}
		if tmin > tmax {
			return Interval{}, false
		}
	}
	if tmax < 0 {
		return Interval{}, false
	}
	tmin = math.Max(0, tmin)
	if tmax < tmin {
		return Interval{}, false
	}
	return Interval{T0: tmin, T1: tmax}, true
}

func RayVoxel(origin, dir model.Vec3, medium model.Medium, i, j, k int) (Interval, bool) {
	if i < 0 || j < 0 || k < 0 || i >= medium.Density.NX || j >= medium.Density.NY || k >= medium.Density.NZ {
		return Interval{}, false
	}
	ext := medium.Bounds.Extent()
	cell := model.Vec3{
		X: ext.X / float64(medium.Density.NX),
		Y: ext.Y / float64(medium.Density.NY),
		Z: ext.Z / float64(medium.Density.NZ),
	}
	box := model.AABB{
		Min: model.Vec3{
			X: medium.Bounds.Min.X + float64(i)*cell.X,
			Y: medium.Bounds.Min.Y + float64(j)*cell.Y,
			Z: medium.Bounds.Min.Z + float64(k)*cell.Z,
		},
	}
	box.Max = model.Vec3{X: box.Min.X + cell.X, Y: box.Min.Y + cell.Y, Z: box.Min.Z + cell.Z}
	return RayAABB(origin, dir, box)
}

type SweepItem struct {
	ID    string
	T0    float64
	T1    float64
	Value any
}

type SweepSpan struct {
	T0    float64
	T1    float64
	Items []SweepItem
}

func Sweep(items []SweepItem) []SweepSpan {
	if len(items) == 0 {
		return nil
	}
	points := make([]float64, 0, len(items)*2)
	for _, it := range items {
		if it.T1 <= it.T0 {
			continue
		}
		points = append(points, it.T0, it.T1)
	}
	sortFloats(points)
	var spans []SweepSpan
	for i := 0; i+1 < len(points); i++ {
		t0, t1 := points[i], points[i+1]
		if t1 <= t0 {
			continue
		}
		mid := (t0 + t1) * 0.5
		var active []SweepItem
		for _, it := range items {
			if it.T1 > it.T0 && mid >= it.T0 && mid < it.T1 {
				active = append(active, it)
			}
		}
		if len(active) == 0 {
			continue
		}
		sortItems(active)
		if len(spans) > 0 && spans[len(spans)-1].T1 == t0 && sameItems(spans[len(spans)-1].Items, active) {
			spans[len(spans)-1].T1 = t1
			continue
		}
		spans = append(spans, SweepSpan{T0: t0, T1: t1, Items: active})
	}
	return spans
}

type edge struct {
	t    float64
	item int
}

func sortItems(items []SweepItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].ID < items[j-1].ID; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func sameItems(a, b []SweepItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].T0 != b[i].T0 || a[i].T1 != b[i].T1 {
			return false
		}
	}
	return true
}

func sortFloats(xs []float64) {
	if len(xs) < 2 {
		return
	}
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && (xs[j] < xs[j-1] || (xs[j] == xs[j-1] && j > 0)); j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

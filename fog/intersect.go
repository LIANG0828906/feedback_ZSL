package fog

import (
	"encoding/json"
	"math"
	"sort"
)

type RawInterval struct {
	MediumID string  `json:"medium_id"`
	T0       float64 `json:"t0"`
	T1       float64 `json:"t1"`
}

// Segment is an elementary interval over which the ordered active medium set
// is constant. Overlaps are represented by splitting at every boundary; the
// integrator never adds a whole overlapping interval twice.
type Segment struct {
	Index           int      `json:"index"`
	T0              int64    `json:"t0"`
	T1              int64    `json:"t1"`
	RawT0           float64  `json:"raw_t0"`
	RawT1           float64  `json:"raw_t1"`
	ResidualT0      float64  `json:"residual_t0"`
	ResidualT1      float64  `json:"residual_t1"`
	ActiveMedia     []string `json:"active_media"`
	CoveredMedia    []string `json:"covered_media"`
	ContainsOverlap bool     `json:"contains_overlap"`
}

func (s Segment) Length(scale int64) float64 {
	return FixedToFloat(s.T1-s.T0, scale)
}

type Intersection struct {
	RayID     string        `json:"ray_id"`
	Scale     int64         `json:"scale"`
	Intervals []RawInterval `json:"intervals"`
	Segments  []Segment     `json:"segments"`
	Digest    string        `json:"digest"`
}

type rawBoundary struct {
	q   int64
	raw float64
}

func IntersectRay(scene *Scene, ray Ray) Intersection {
	scale := scene.Recipe.FixedScale
	maxT := scene.Recipe.MaxRayDistance
	if maxT <= 0 {
		maxT = math.Inf(1)
	}
	intervals := make([]RawInterval, 0, len(scene.Media))
	for _, m := range scene.SortedMedia() {
		t0, t1, ok := intersectAABB(ray.Origin, ray.Direction, m.Bounds)
		if !ok {
			continue
		}
		t0 = math.Max(t0, 0)
		t1 = math.Min(t1, maxT)
		if t1 <= t0 {
			continue
		}
		q0, _ := Quantize(t0, scale)
		q1, _ := Quantize(t1, scale)
		if q1 <= q0 {
			continue
		}
		intervals = append(intervals, RawInterval{MediumID: m.ID, T0: t0, T1: t1})
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].T0 != intervals[j].T0 {
			return intervals[i].T0 < intervals[j].T0
		}
		if intervals[i].T1 != intervals[j].T1 {
			return intervals[i].T1 < intervals[j].T1
		}
		return intervals[i].MediumID < intervals[j].MediumID
	})
	byQ := map[int64]rawBoundary{}
	addBoundary := func(raw float64) int64 {
		q, _ := Quantize(raw, scale)
		if old, ok := byQ[q]; !ok || raw < old.raw {
			byQ[q] = rawBoundary{q: q, raw: raw}
		}
		return q
	}
	type quantInterval struct {
		id string
		q0 int64
		q1 int64
	}
	qintervals := make([]quantInterval, 0, len(intervals))
	for _, in := range intervals {
		q0 := addBoundary(in.T0)
		q1 := addBoundary(in.T1)
		if q1 > q0 {
			qintervals = append(qintervals, quantInterval{id: in.MediumID, q0: q0, q1: q1})
		}
	}
	boundaries := make([]int64, 0, len(byQ))
	for q := range byQ {
		boundaries = append(boundaries, q)
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i] < boundaries[j] })
	segments := make([]Segment, 0, maxInt(0, len(boundaries)-1))
	for i := 0; i+1 < len(boundaries); i++ {
		q0, q1 := boundaries[i], boundaries[i+1]
		if q1 <= q0 {
			continue
		}
		active := make([]string, 0, 2)
		covered := make([]string, 0, len(qintervals))
		for _, in := range qintervals {
			if in.q0 <= q0 && in.q1 >= q1 {
				active = append(active, in.id)
			}
			if in.q0 < q1 && in.q1 > q0 {
				covered = append(covered, in.id)
			}
		}
		if len(active) == 0 {
			continue
		}
		sort.Strings(active)
		sort.Strings(covered)
		b0 := byQ[q0]
		b1 := byQ[q1]
		seg := Segment{
			Index: len(segments),
			T0:    q0, T1: q1,
			RawT0: b0.raw, RawT1: b1.raw,
			ResidualT0:      FixedToFloat(q0, scale) - b0.raw,
			ResidualT1:      FixedToFloat(q1, scale) - b1.raw,
			ActiveMedia:     active,
			CoveredMedia:    covered,
			ContainsOverlap: len(active) > 1,
		}
		segments = append(segments, seg)
	}
	out := Intersection{RayID: ray.ID, Scale: scale, Intervals: intervals, Segments: segments}
	out.Digest = intersectionDigest(out)
	return out
}

func intersectionDigest(in Intersection) string {
	type part struct {
		RayID    string    `json:"ray_id"`
		Scale    int64     `json:"scale"`
		Segments []Segment `json:"segments"`
	}
	b, _ := json.Marshal(part{RayID: in.RayID, Scale: in.Scale, Segments: in.Segments})
	return hashStrings(string(b))
}

func intersectAABB(origin, dir Vec3, b AABB) (float64, float64, bool) {
	tmin := math.Inf(-1)
	tmax := math.Inf(1)
	axes := [3][2]float64{
		{b.Min.X, b.Max.X},
		{b.Min.Y, b.Max.Y},
		{b.Min.Z, b.Max.Z},
	}
	o := [3]float64{origin.X, origin.Y, origin.Z}
	d := [3]float64{dir.X, dir.Y, dir.Z}
	for i := 0; i < 3; i++ {
		if math.Abs(d[i]) < 1e-15 {
			if o[i] < axes[i][0] || o[i] > axes[i][1] {
				return 0, 0, false
			}
			continue
		}
		t0 := (axes[i][0] - o[i]) / d[i]
		t1 := (axes[i][1] - o[i]) / d[i]
		if t0 > t1 {
			t0, t1 = t1, t0
		}
		tmin = math.Max(tmin, t0)
		tmax = math.Min(tmax, t1)
		if tmin > tmax {
			return 0, 0, false
		}
	}
	if tmax < 0 || math.IsInf(tmin, -1) {
		return 0, 0, false
	}
	return tmin, tmax, true
}

// VoxelsAlongSegment returns every voxel whose AABB overlaps the quantized
// segment. It is used for explicit density invalidation, not for shading.
func VoxelsAlongSegment(m Medium, ray Ray, seg Segment, scale int64) []int {
	nx, ny, nz := m.DensityRes[0], m.DensityRes[1], m.DensityRes[2]
	if nx <= 0 || ny <= 0 || nz <= 0 {
		return nil
	}
	t0 := FixedToFloat(seg.T0, scale)
	t1 := FixedToFloat(seg.T1, scale)
	out := make([]int, 0, 8)
	for i := 0; i < nx*ny*nz; i++ {
		vb, ok := m.VoxelBounds(i)
		if !ok {
			continue
		}
		a, b, hit := intersectAABB(ray.Origin, ray.Direction, vb)
		if !hit {
			continue
		}
		a = math.Max(a, t0)
		b = math.Min(b, t1)
		if b > a+1e-12 {
			out = append(out, i)
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

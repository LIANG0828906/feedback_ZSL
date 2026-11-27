package volumefog

import (
	"math"
	"sort"
)

// Ray is a fixed-point primary or shadow ray. Direction is normalized to
// roughly one world unit and encoded in Q10, so t is also Q10 world distance.
type Ray struct {
	ID          string `json:"id"`
	OriginQ     Vec3i  `json:"origin_q"`
	DirQ        Vec3i  `json:"dir_q"`
	NearQ       int64  `json:"near_q"`
	FarQ        int64  `json:"far_q"`
	CameraIndex int    `json:"camera_index"`
}

func (r Ray) DirectionFloat() [3]float64 {
	return [3]float64{
		float64(r.DirQ[0]) / float64(FixedScale),
		float64(r.DirQ[1]) / float64(FixedScale),
		float64(r.DirQ[2]) / float64(FixedScale),
	}
}

func (r Ray) PointAtQ(tQ int64) Vec3i {
	return Vec3i{
		r.OriginQ[0] + r.DirQ[0]*tQ/FixedScale,
		r.OriginQ[1] + r.DirQ[1]*tQ/FixedScale,
		r.OriginQ[2] + r.DirQ[2]*tQ/FixedScale,
	}
}

// Interval is a quantized medium/ray intersection interval.
type Interval struct {
	MediaID        string  `json:"media_id"`
	StartQ         int64   `json:"start_q"`
	EndQ           int64   `json:"end_q"`
	StartResidualQ float64 `json:"start_residual_q"`
	EndResidualQ   float64 `json:"end_residual_q"`
}

// Segment is an atomic interval after all overlap boundaries are inserted.
// Active media are combined as one composite medium; their independent
// contributions are not simply added as separate whole intervals.
type Segment struct {
	Index     int      `json:"index"`
	T0Q       int64    `json:"t0_q"`
	T1Q       int64    `json:"t1_q"`
	Active    []string `json:"active"`
	ResidualQ float64  `json:"residual_q"`
}

func floorDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	q := a / b
	r := a % b
	if r != 0 && ((r < 0) != (b < 0)) {
		q--
	}
	return q
}

func ceilDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return -floorDiv(-a, b)
}

func absI64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func residualQ(num, den, q int64) float64 {
	if den == 0 {
		return 0
	}
	exact := float64(num) / float64(den)
	return math.Abs(exact - float64(q))
}

// IntersectRay intersects all media with r using only fixed-point interval
// endpoints. The returned intervals are not merged.
func IntersectRay(media []Medium, r Ray) []Interval {
	var out []Interval
	for i := range media {
		m := &media[i]
		var tmin int64 = math.MinInt64 / 4
		var tmax int64 = math.MaxInt64 / 4
		var resMin, resMax float64
		miss := false
		for a := 0; a < 3; a++ {
			d := r.DirQ[a]
			if d == 0 {
				if r.OriginQ[a] < m.MinQ[a] || r.OriginQ[a] >= m.MaxQ[a] {
					miss = true
					break
				}
				continue
			}
			numA := (m.MinQ[a] - r.OriginQ[a]) * FixedScale
			numB := (m.MaxQ[a] - r.OriginQ[a]) * FixedScale
			var loNum, hiNum int64
			if d > 0 {
				loNum, hiNum = numA, numB
			} else {
				loNum, hiNum = numB, numA
			}
			lo := ceilDiv(loNum, d)
			hi := floorDiv(hiNum, d)
			if lo > tmin {
				tmin = lo
				resMin = residualQ(loNum, d, lo)
			}
			if hi < tmax {
				tmax = hi
				resMax = residualQ(hiNum, d, hi)
			}
		}
		if miss {
			continue
		}
		if tmin < 0 {
			tmin = 0
			resMin = 0
		}
		if tmax > r.FarQ {
			tmax = r.FarQ
			resMax = 0
		}
		if tmin < r.NearQ {
			tmin = r.NearQ
			resMin = 0
		}
		if tmax <= tmin {
			continue
		}
		out = append(out, Interval{
			MediaID:        m.ID,
			StartQ:         tmin,
			EndQ:           tmax,
			StartResidualQ: resMin,
			EndResidualQ:   resMax,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartQ != out[j].StartQ {
			return out[i].StartQ < out[j].StartQ
		}
		if out[i].EndQ != out[j].EndQ {
			return out[i].EndQ < out[j].EndQ
		}
		return out[i].MediaID < out[j].MediaID
	})
	return out
}

// BuildSegments inserts every interval endpoint and creates one composite
// segment per atomic span. This is the declared overlap rule: active media are
// composited inside each atomic span, and spans are integrated in t order.
func BuildSegments(intervals []Interval) []Segment {
	if len(intervals) == 0 {
		return nil
	}
	residual := make(map[int64]float64)
	boundarySet := make(map[int64]struct{})
	for _, in := range intervals {
		boundarySet[in.StartQ] = struct{}{}
		boundarySet[in.EndQ] = struct{}{}
		if in.StartResidualQ > residual[in.StartQ] {
			residual[in.StartQ] = in.StartResidualQ
		}
		if in.EndResidualQ > residual[in.EndQ] {
			residual[in.EndQ] = in.EndResidualQ
		}
	}
	bounds := make([]int64, 0, len(boundarySet))
	for b := range boundarySet {
		bounds = append(bounds, b)
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
	var out []Segment
	for i := 0; i+1 < len(bounds); i++ {
		lo, hi := bounds[i], bounds[i+1]
		if hi <= lo {
			continue
		}
		var active []string
		for _, in := range intervals {
			if in.StartQ <= lo && in.EndQ >= hi {
				active = append(active, in.MediaID)
			}
		}
		if len(active) == 0 {
			continue
		}
		sort.Strings(active)
		out = append(out, Segment{
			Index:     len(out),
			T0Q:       lo,
			T1Q:       hi,
			Active:    active,
			ResidualQ: math.Max(residual[lo], residual[hi]),
		})
	}
	return out
}

func RaySegments(media []Medium, r Ray) []Segment {
	return BuildSegments(IntersectRay(media, r))
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

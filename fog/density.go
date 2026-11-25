package fog

import (
	"math"
	"sort"
)

type DensitySegment struct {
	MediumID string  `json:"medium_id"`
	X        int     `json:"x"`
	Y        int     `json:"y"`
	Z        int     `json:"z"`
	TMin     float64 `json:"t_min"`
	TMax     float64 `json:"t_max"`
	Density  float64 `json:"density"`
}

// DensitySegments traverses the integer voxel field with an Amanatides-Woo
// style boundary walk. The ray direction is expected to be normalized.
func DensitySegments(m *Medium, ray Ray, tMin, tMax float64) []DensitySegment {
	if tMax <= tMin {
		return nil
	}
	const eps = 1e-10
	startT := tMin + eps
	p := ray.Point(startT)
	x, y, z, ok := m.Field.VoxelAtWorld(m.Bounds, p)
	if !ok {
		// Try the midpoint in case start touches a boundary.
		p = ray.Point((tMin + tMax) / 2)
		x, y, z, ok = m.Field.VoxelAtWorld(m.Bounds, p)
		if !ok {
			return nil
		}
		startT = tMin
	}

	stepX, tMaxX, deltaX := voxelWalkAxis(ray.Origin.X, ray.Dir.X, m.Bounds.Min.X, m.Field.VoxelSize, x)
	stepY, tMaxY, deltaY := voxelWalkAxis(ray.Origin.Y, ray.Dir.Y, m.Bounds.Min.Y, m.Field.VoxelSize, y)
	stepZ, tMaxZ, deltaZ := voxelWalkAxis(ray.Origin.Z, ray.Dir.Z, m.Bounds.Min.Z, m.Field.VoxelSize, z)

	segments := make([]DensitySegment, 0, 8)
	curT := tMin
	for curT < tMax-eps {
		nextT := tMax
		if tMaxX > curT+eps && tMaxX < nextT {
			nextT = tMaxX
		}
		if tMaxY > curT+eps && tMaxY < nextT {
			nextT = tMaxY
		}
		if tMaxZ > curT+eps && tMaxZ < nextT {
			nextT = tMaxZ
		}
		if nextT <= curT+eps {
			// Boundary degeneracy: advance the axis whose boundary is at curT.
			switch {
			case math.Abs(tMaxX-curT) < eps:
				x += stepX
				tMaxX += deltaX
			case math.Abs(tMaxY-curT) < eps:
				y += stepY
				tMaxY += deltaY
			default:
				z += stepZ
				tMaxZ += deltaZ
			}
			continue
		}
		endT := math.Min(nextT, tMax)
		d := float64(m.Field.Values[m.Field.Index(x, y, z)])
		segments = append(segments, DensitySegment{
			MediumID: m.ID, X: x, Y: y, Z: z,
			TMin: curT, TMax: endT, Density: d,
		})
		curT = endT
		if curT >= tMax-eps {
			break
		}
		if math.Abs(tMaxX-curT) < eps {
			x += stepX
			tMaxX += deltaX
		}
		if math.Abs(tMaxY-curT) < eps {
			y += stepY
			tMaxY += deltaY
		}
		if math.Abs(tMaxZ-curT) < eps {
			z += stepZ
			tMaxZ += deltaZ
		}
		if !m.Field.InBounds(x, y, z) {
			break
		}
	}
	return mergeDensitySegments(segments)
}

func voxelWalkAxis(origin, dir, boundMin, voxelSize float64, cell int) (step int, tMaxBoundary, tDelta float64) {
	if math.Abs(dir) < 1e-15 {
		return 0, math.Inf(1), math.Inf(1)
	}
	if dir > 0 {
		step = 1
		boundary := boundMin + float64(cell+1)*voxelSize
		tMaxBoundary = (boundary - origin) / dir
		tDelta = voxelSize / dir
	} else {
		step = -1
		boundary := boundMin + float64(cell)*voxelSize
		tMaxBoundary = (boundary - origin) / dir
		tDelta = voxelSize / -dir
	}
	if tMaxBoundary < 0 {
		tMaxBoundary = 0
	}
	return step, tMaxBoundary, tDelta
}

func mergeDensitySegments(in []DensitySegment) []DensitySegment {
	if len(in) == 0 {
		return in
	}
	out := []DensitySegment{in[0]}
	for _, seg := range in[1:] {
		last := &out[len(out)-1]
		if last.X == seg.X && last.Y == seg.Y && last.Z == seg.Z &&
			last.MediumID == seg.MediumID && math.Abs(last.TMax-seg.TMin) < 1e-9 {
			last.TMax = seg.TMax
			continue
		}
		out = append(out, seg)
	}
	return out
}

// IntegrateDensity returns the raw integer-density line integral.
func IntegrateDensity(m *Medium, ray Ray, tMin, tMax float64) (float64, []DensitySegment) {
	segments := DensitySegments(m, ray, tMin, tMax)
	var sum float64
	for _, seg := range segments {
		sum += seg.Density * (seg.TMax - seg.TMin)
	}
	return sum, segments
}

type MediumInterval struct {
	MediumID     string  `json:"medium_id"`
	TMinQ        int64   `json:"t_min_q"`
	TMaxQ        int64   `json:"t_max_q"`
	TMinResidual float64 `json:"t_min_residual"`
	TMaxResidual float64 `json:"t_max_residual"`
}

type SpanKind string

const (
	SpanEmpty  SpanKind = "empty"
	SpanMedium SpanKind = "medium"
)

type RaySpan struct {
	Index     int              `json:"index"`
	Kind      SpanKind         `json:"kind"`
	TMinQ     int64            `json:"t_min_q"`
	TMaxQ     int64            `json:"t_max_q"`
	TMin      float64          `json:"t_min"`
	TMax      float64          `json:"t_max"`
	MediumIDs []string         `json:"medium_ids,omitempty"`
	Intervals []MediumInterval `json:"intervals,omitempty"`
	VoxelKeys []string         `json:"voxel_keys,omitempty"`
	Rule      string           `json:"rule"`
}

type TraceResult struct {
	RayID     string           `json:"ray_id"`
	TileID    int              `json:"tile_id"`
	Ray       Ray              `json:"ray"`
	Intervals []MediumInterval `json:"intervals"`
	Spans     []RaySpan        `json:"spans"`
	Digest    string           `json:"digest"`
}

// TraceRay deterministically intersects all media and partitions the ray at
// every interval endpoint. Empty gaps are retained as explicit empty spans.
func TraceRay(ray Ray, media []*Medium, scale int64) TraceResult {
	if scale <= 0 {
		scale = DefaultFixedScale
	}
	intervals := make([]MediumInterval, 0, len(media))
	endpoints := []int64{}
	nearQ, _ := Quantize(ray.Near, scale)
	farQ, _ := Quantize(ray.Far, scale)
	endpoints = append(endpoints, nearQ, farQ)

	for _, m := range media {
		rawMin, rawMax, ok := m.Bounds.IntersectRay(ray.Origin, ray.Dir)
		if !ok {
			continue
		}
		minQ, minRes := Quantize(rawMin, scale)
		maxQ, maxRes := Quantize(rawMax, scale)
		if minQ < nearQ {
			minQ = nearQ
		}
		if maxQ > farQ {
			maxQ = farQ
		}
		if maxQ < minQ {
			continue
		}
		iv := MediumInterval{
			MediumID: m.ID, TMinQ: minQ, TMaxQ: maxQ,
			TMinResidual: minRes, TMaxResidual: maxRes,
		}
		intervals = append(intervals, iv)
		endpoints = append(endpoints, minQ, maxQ)
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].TMinQ != intervals[j].TMinQ {
			return intervals[i].TMinQ < intervals[j].TMinQ
		}
		return intervals[i].MediumID < intervals[j].MediumID
	})
	endpoints = uniqueSortedInt64(endpoints)

	spans := make([]RaySpan, 0, len(endpoints)-1)
	spanIndex := 0
	for i := 0; i+1 < len(endpoints); i++ {
		loQ := endpoints[i]
		hiQ := endpoints[i+1]
		if hiQ <= loQ {
			continue
		}
		mid := FromFixed((loQ+hiQ)/2, scale)
		active := make([]MediumInterval, 0)
		ids := make([]string, 0)
		for _, iv := range intervals {
			if iv.TMinQ <= loQ && iv.TMaxQ >= hiQ {
				active = append(active, iv)
				ids = append(ids, iv.MediumID)
			}
		}
		kind := SpanEmpty
		rule := "explicit-empty"
		if len(active) > 0 {
			kind = SpanMedium
			rule = "split-at-all-endpoints;composite-sum-within-overlap"
		}
		tMin := FromFixed(loQ, scale)
		tMax := FromFixed(hiQ, scale)
		span := RaySpan{
			Index: spanIndex, Kind: kind, TMinQ: loQ, TMaxQ: hiQ,
			TMin: tMin, TMax: tMax, MediumIDs: ids, Intervals: active, Rule: rule,
		}
		if kind == SpanMedium {
			span.VoxelKeys = collectVoxelKeys(media, ray, span)
		}
		spans = append(spans, span)
		spanIndex++
		_ = mid
	}
	if len(spans) == 0 {
		spans = append(spans, RaySpan{
			Index: 0, Kind: SpanEmpty, TMinQ: nearQ, TMaxQ: farQ,
			TMin: ray.Near, TMax: ray.Far, Rule: "explicit-empty-no-valid-domain",
		})
	}

	result := TraceResult{
		RayID: ray.ID, TileID: ray.TileID, Ray: ray,
		Intervals: intervals, Spans: spans,
	}
	result.Digest = ContentHash("trace", struct {
		RayID     string           `json:"ray_id"`
		Intervals []MediumInterval `json:"intervals"`
		Spans     []RaySpan        `json:"spans"`
	}{ray.ID, intervals, spans})
	return result
}

func collectVoxelKeys(media []*Medium, ray Ray, span RaySpan) []string {
	byID := make(map[string]*Medium, len(media))
	for _, m := range media {
		byID[m.ID] = m
	}
	keys := map[string]struct{}{}
	for _, id := range span.MediumIDs {
		m := byID[id]
		if m == nil {
			continue
		}
		for _, seg := range DensitySegments(m, ray, span.TMin, span.TMax) {
			keys[voxelKey(id, seg.X, seg.Y, seg.Z)] = struct{}{}
		}
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func voxelKey(mediumID string, x, y, z int) string {
	return mediumID + "/" + itoa(x) + "/" + itoa(y) + "/" + itoa(z)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func uniqueSortedInt64(in []int64) []int64 {
	sort.Slice(in, func(i, j int) bool { return in[i] < in[j] })
	out := in[:0]
	for _, v := range in {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

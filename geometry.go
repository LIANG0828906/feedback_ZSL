package fog

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

const MaxRayParameter = 1_000_000

func crossVec3(a, b Vec3) Vec3 {
	return Vec3{
		X: a.Y*b.Z - a.Z*b.Y,
		Y: a.Z*b.X - a.X*b.Z,
		Z: a.X*b.Y - a.Y*b.X,
	}
}

func RayForPixel(camera CameraSample, viewport Viewport, x, y int) Ray {
	forward := camera.Forward.Normalize()
	up := camera.Up.Normalize()
	right := crossVec3(forward, up).Normalize()
	tanHalf := math.Tan(camera.FovYDegrees * math.Pi / 360)
	aspect := float64(viewport.Width) / float64(viewport.Height)
	sx := (float64(x)+0.5)/float64(viewport.Width)*2 - 1
	sy := 1 - ((float64(y)+0.5)/float64(viewport.Height)*2 - 1)
	dir := forward.Add(right.Scale(sx * tanHalf * aspect)).Add(up.Scale(sy * tanHalf)).Normalize()
	id := fmt.Sprintf("ray-%03d-%03d", x, y)
	return Ray{
		ID:     id,
		Pixel:  PixelCoord{X: x, Y: y},
		Origin: camera.Position,
		Dir:    dir,
		Near:   camera.Near,
		Far:    camera.Far,
	}
}

func RaysForTile(scene Scene, tile TileCoord) []Ray {
	tw := scene.Recipe.TileWidth
	th := scene.Recipe.TileHeight
	x0 := tile.X * tw
	y0 := tile.Y * th
	x1 := minInt(x0+tw, scene.Viewport.Width)
	y1 := minInt(y0+th, scene.Viewport.Height)
	rays := make([]Ray, 0, (x1-x0)*(y1-y0))
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			rays = append(rays, RayForPixel(scene.ActiveSample(), scene.Viewport, x, y))
		}
	}
	return rays
}

func TileGrid(viewport Viewport, recipe Recipe) []TileCoord {
	cols := (viewport.Width + recipe.TileWidth - 1) / recipe.TileWidth
	rows := (viewport.Height + recipe.TileHeight - 1) / recipe.TileHeight
	tiles := make([]TileCoord, 0, cols*rows)
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			tiles = append(tiles, TileCoord{X: x, Y: y})
		}
	}
	return tiles
}

type MediumHit struct {
	MediumID string
	Near     float64
	Far      float64
}

func SlabIntersection(ray Ray, bounds AABB) (near, far float64, ok bool) {
	o := ray.Origin
	d := ray.Dir
	min := bounds.Min.Float64()
	max := bounds.Max.Float64()
	rayNear := ray.Near.Float64()
	rayFar := ray.Far.Float64()
	tmin := rayNear
	tmax := rayFar

	axes := []struct {
		origin, dir, lo, hi float64
	}{
		{o.X, d.X, min.X, max.X},
		{o.Y, d.Y, min.Y, max.Y},
		{o.Z, d.Z, min.Z, max.Z},
	}
	for _, ax := range axes {
		if math.Abs(ax.dir) < 1e-12 {
			if ax.origin < ax.lo-1e-10 || ax.origin > ax.hi+1e-10 {
				return 0, 0, false
			}
			continue
		}
		t1 := (ax.lo - ax.origin) / ax.dir
		t2 := (ax.hi - ax.origin) / ax.dir
		if t1 > t2 {
			t1, t2 = t2, t1
		}
		if t1 > tmin {
			tmin = t1
		}
		if t2 < tmax {
			tmax = t2
		}
		if tmin > tmax+1e-10 {
			return 0, 0, false
		}
	}
	if tmax < rayNear-1e-10 || tmin > rayFar+1e-10 || tmax <= tmin+1e-12 {
		return 0, 0, false
	}
	return maxFloat(tmin, rayNear), minFloat(tmax, rayFar), true
}

func IntersectRayMedia(ray Ray, media []*Medium) ([]MediumHit, error) {
	if !finiteVec3(ray.Origin) || !finiteVec3(ray.Dir) || ray.Dir.Len() == 0 {
		return nil, segError(ray.ID, [2]string{}, "ray origin/direction is invalid")
	}
	if ray.Near < 0 || ray.Far <= ray.Near || int64(ray.Far) > MaxRayParameter*FixedScale {
		e := segError(ray.ID, [2]string{}, "ray parameter interval out of declared range: near=%v far=%v", ray.Near.Float64(), ray.Far.Float64())
		e.Field = "ray.interval"
		e.Value = [2]int64{int64(ray.Near), int64(ray.Far)}
		return nil, e
	}
	hits := make([]MediumHit, 0, len(media))
	for _, medium := range media {
		if medium == nil {
			continue
		}
		near, far, ok := SlabIntersection(ray, medium.Bounds)
		if !ok {
			continue
		}
		hits = append(hits, MediumHit{MediumID: medium.ID, Near: near, Far: far})
	}
	return hits, nil
}

func quantizeEndpoint(raw float64) SegmentEndpoint {
	fixed := FixedFromFloat(raw)
	quantized := fixed.Float64()
	return SegmentEndpoint{Fixed: fixed, Raw: raw, Residual: raw - quantized}
}

func BuildRaySegments(ray Ray, hits []MediumHit) ([]RaySegment, error) {
	type quantHit struct {
		id   string
		near SegmentEndpoint
		far  SegmentEndpoint
	}
	quant := make([]quantHit, 0, len(hits))
	for _, hit := range hits {
		if hit.MediumID == "" || math.IsNaN(hit.Near) || math.IsNaN(hit.Far) || math.IsInf(hit.Near, 0) || math.IsInf(hit.Far, 0) {
			return nil, segError(ray.ID, [2]string{hit.MediumID}, "invalid medium interval endpoint")
		}
		if math.Abs(hit.Near) > MaxRayParameter || math.Abs(hit.Far) > MaxRayParameter || hit.Near > hit.Far {
			e := segError(ray.ID, [2]string{hit.MediumID}, "medium interval parameter out of order/range: near=%v far=%v", hit.Near, hit.Far)
			e.Field = "interval"
			e.Value = [2]float64{hit.Near, hit.Far}
			return nil, e
		}
		n := quantizeEndpoint(hit.Near)
		f := quantizeEndpoint(hit.Far)
		if f.Fixed <= n.Fixed {
			continue
		}
		quant = append(quant, quantHit{id: hit.MediumID, near: n, far: f})
	}

	points := make([]Fixed, 0, len(quant)*2)
	for _, h := range quant {
		points = append(points, h.near.Fixed, h.far.Fixed)
	}
	sort.Slice(points, func(i, j int) bool { return points[i] < points[j] })
	unique := points[:0]
	for _, p := range points {
		if len(unique) == 0 || unique[len(unique)-1] != p {
			unique = append(unique, p)
		}
	}
	endpointByFixed := map[Fixed]SegmentEndpoint{}
	for _, h := range quant {
		for _, endpoint := range []SegmentEndpoint{h.near, h.far} {
			existing, ok := endpointByFixed[endpoint.Fixed]
			if !ok || math.Abs(endpoint.Residual) > math.Abs(existing.Residual) {
				endpointByFixed[endpoint.Fixed] = endpoint
			}
		}
	}

	segments := make([]RaySegment, 0)
	for i := 0; i+1 < len(unique); i++ {
		lo, hi := unique[i], unique[i+1]
		if hi <= lo {
			continue
		}
		ids := make([]string, 0)
		for _, h := range quant {
			if h.near.Fixed <= lo && h.far.Fixed >= hi {
				ids = append(ids, h.id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		sort.Strings(ids)
		segments = append(segments, RaySegment{
			Index:       len(segments),
			MediumIDs:   ids,
			Near:        endpointByFixed[lo],
			Far:         endpointByFixed[hi],
			Length:      (hi - lo).Float64() * ray.Dir.Len(),
			PriorityKey: strings.Join(ids, "+"),
		})
	}
	if err := ValidateSegmentOrder(ray.ID, segments); err != nil {
		return nil, err
	}
	return segments, nil
}

func ValidateSegmentOrder(rayID string, segments []RaySegment) error {
	var previous Fixed
	for i, seg := range segments {
		if i > 0 && seg.Near.Fixed < previous {
			pair := [2]string{}
			if len(segments[i-1].MediumIDs) > 0 {
				pair[0] = segments[i-1].MediumIDs[0]
			}
			if len(seg.MediumIDs) > 0 {
				pair[1] = seg.MediumIDs[0]
			}
			return segError(rayID, pair, "overlap segment order cannot be explained at segment %d", i)
		}
		if seg.Far.Fixed < seg.Near.Fixed || seg.Length < 0 {
			pair := [2]string{}
			if len(seg.MediumIDs) > 0 {
				pair[0] = seg.MediumIDs[0]
			}
			return segError(rayID, pair, "segment endpoints are inconsistent at segment %d", i)
		}
		if expected := strings.Join(sortedCopy(seg.MediumIDs), "+"); seg.PriorityKey != expected {
			pair := [2]string{}
			if len(seg.MediumIDs) >= 1 {
				pair[0] = seg.MediumIDs[0]
			}
			if len(seg.MediumIDs) >= 2 {
				pair[1] = seg.MediumIDs[1]
			}
			return segError(rayID, pair, "overlap order key %q is not explainable; expected %q", seg.PriorityKey, expected)
		}
		previous = seg.Near.Fixed
	}
	return nil
}

func segError(rayID string, pair [2]string, format string, args ...any) *Error {
	e := newError(CodeSeg, format, args...)
	e.RayID = rayID
	e.MediumPair = pair
	if pair[0] != "" {
		e.MediumID = pair[0]
	}
	return e
}

func finiteVec3(v Vec3) bool {
	return finiteFloat(v.X) && finiteFloat(v.Y) && finiteFloat(v.Z)
}

func finiteFloat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

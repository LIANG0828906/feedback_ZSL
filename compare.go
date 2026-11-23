package spatialpick

import (
	"fmt"
	"math"
	"sort"
)

// CompareBundles returns E_DIVERGE at the first differing object/data item.
// Fixed world matrices and ambiguity locations must match exactly; floating
// derived values must agree within four fixed-point quanta.
func CompareBundles(left, right *ResultBundle) error {
	if left == nil || right == nil {
		return diverge("bundle", "nil", left == nil, right == nil)
	}
	if left.SceneVersion != right.SceneVersion {
		return diverge("bundle", "scene_version", left.SceneVersion, right.SceneVersion)
	}
	if left.Revision != right.Revision {
		return diverge("bundle", "revision", left.Revision, right.Revision)
	}
	if left.Precision != right.Precision {
		return diverge("bundle", "precision", left.Precision, right.Precision)
	}
	precision := left.Precision
	if precision < 0 {
		precision = 6
	}
	tol := math.Pow10(-precision) * 4
	ln := nodeResultsByID(left.Nodes)
	rn := nodeResultsByID(right.Nodes)
	if len(ln) != len(rn) {
		return diverge("nodes", "count", len(ln), len(rn))
	}
	ids := make([]string, 0, len(ln))
	for id := range ln {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a, ok := ln[id]
		if !ok {
			return diverge(id, "missing_left", nil, right)
		}
		b, ok := rn[id]
		if !ok {
			return diverge(id, "missing_right", left, nil)
		}
		if a.Depth != b.Depth {
			return diverge(id, "depth", a.Depth, b.Depth)
		}
		if a.WorldRaw != b.WorldRaw {
			return diverge(id, "world_raw", a.WorldRaw, b.WorldRaw)
		}
		for i := 0; i < 16; i++ {
			if math.Abs(a.World[i]-b.World[i]) > tol {
				return diverge(id, fmt.Sprintf("world[%d]", i), a.World[i], b.World[i])
			}
		}
		if a.GeometryID != b.GeometryID {
			return diverge(id, "geometry_id", a.GeometryID, b.GeometryID)
		}
		if err := compareAABB(id, "aabb", a.AABB, b.AABB, tol); err != nil {
			return err
		}
		if err := compareSphere(id, "sphere", a.Sphere, b.Sphere, tol); err != nil {
			return err
		}
		if math.Abs(a.ErrorBound-b.ErrorBound) > tol {
			return diverge(id, "error_bound", a.ErrorBound, b.ErrorBound)
		}
	}
	if len(left.Rays) != len(right.Rays) {
		return diverge("rays", "count", len(left.Rays), len(right.Rays))
	}
	for i := range left.Rays {
		a, b := left.Rays[i], right.Rays[i]
		if a.QueryID != b.QueryID {
			return diverge("rays", fmt.Sprintf("[%d].query_id", i), a.QueryID, b.QueryID)
		}
		if a.ErrorCode != b.ErrorCode {
			return diverge(a.QueryID, "ray.error_code", a.ErrorCode, b.ErrorCode)
		}
		if math.Abs(a.ErrorBound-b.ErrorBound) > tol {
			return diverge(a.QueryID, "ray.error_bound", a.ErrorBound, b.ErrorBound)
		}
		if err := comparePickHit(a.QueryID, a.First, b.First, tol); err != nil {
			return err
		}
		if len(a.Geometries) != len(b.Geometries) {
			return diverge(a.QueryID, "ray.geometry_count", len(a.Geometries), len(b.Geometries))
		}
		for j := range a.Geometries {
			ga, gb := a.Geometries[j], b.Geometries[j]
			if ga.NodeID != gb.NodeID || ga.GeometryID != gb.GeometryID {
				return diverge(a.QueryID, fmt.Sprintf("ray.geometries[%d].identity", j), ga.NodeID+"/"+ga.GeometryID, gb.NodeID+"/"+gb.GeometryID)
			}
			if ga.Hit != gb.Hit {
				return diverge(a.QueryID, fmt.Sprintf("ray.geometries[%d].hit", j), ga.Hit, gb.Hit)
			}
			if math.Abs(ga.T-gb.T) > tol || !vecNear(ga.Point, gb.Point, tol) || math.Abs(ga.Bound-gb.Bound) > tol {
				return diverge(a.QueryID, fmt.Sprintf("ray.geometries[%d]", j), ga, gb)
			}
		}
		if err := compareAmbiguities(a.QueryID, "ray.ambiguities", a.Ambiguities, b.Ambiguities, tol); err != nil {
			return err
		}
	}
	if len(left.Boxes) != len(right.Boxes) {
		return diverge("boxes", "count", len(left.Boxes), len(right.Boxes))
	}
	for i := range left.Boxes {
		a, b := left.Boxes[i], right.Boxes[i]
		if a.QueryID != b.QueryID {
			return diverge("boxes", fmt.Sprintf("[%d].query_id", i), a.QueryID, b.QueryID)
		}
		if a.ErrorCode != b.ErrorCode || !equalStrings(a.Hits, b.Hits) {
			return diverge(a.QueryID, "box.result", a, b)
		}
		if math.Abs(a.ErrorBound-b.ErrorBound) > tol {
			return diverge(a.QueryID, "box.error_bound", a.ErrorBound, b.ErrorBound)
		}
		if err := compareAmbiguities(a.QueryID, "box.ambiguities", a.Ambiguities, b.Ambiguities, tol); err != nil {
			return err
		}
	}
	if len(left.Points) != len(right.Points) {
		return diverge("points", "count", len(left.Points), len(right.Points))
	}
	for i := range left.Points {
		a, b := left.Points[i], right.Points[i]
		if a.QueryID != b.QueryID {
			return diverge("points", fmt.Sprintf("[%d].query_id", i), a.QueryID, b.QueryID)
		}
		if a.ErrorCode != b.ErrorCode || !equalStrings(a.Hits, b.Hits) {
			return diverge(a.QueryID, "point.result", a, b)
		}
		if math.Abs(a.ErrorBound-b.ErrorBound) > tol {
			return diverge(a.QueryID, "point.error_bound", a.ErrorBound, b.ErrorBound)
		}
		if err := compareAmbiguities(a.QueryID, "point.ambiguities", a.Ambiguities, b.Ambiguities, tol); err != nil {
			return err
		}
	}
	return nil
}

func CompareEngines(left, right *Engine) error {
	return CompareBundles(ptr(left.Bundle()), ptr(right.Bundle()))
}

func ptr[T any](v T) *T { return &v }

func nodeResultsByID(nodes []NodeResult) map[string]NodeResult {
	out := make(map[string]NodeResult, len(nodes))
	for _, n := range nodes {
		out[n.NodeID] = n
	}
	return out
}

func comparePickHit(queryID string, a, b *PickHit, tol float64) error {
	if a == nil || b == nil {
		if a == nil && b == nil {
			return nil
		}
		return diverge(queryID, "ray.first", a, b)
	}
	if a.NodeID != b.NodeID || a.GeometryID != b.GeometryID || a.Depth != b.Depth {
		return diverge(queryID, "ray.first.identity", a, b)
	}
	if math.Abs(a.T-b.T) > tol || !vecNear(a.Point, b.Point, tol) {
		return diverge(queryID, "ray.first", a, b)
	}
	return nil
}

func compareAmbiguities(queryID, item string, a, b []Ambiguity, tol float64) error {
	if len(a) != len(b) {
		return diverge(queryID, item+".count", a, b)
	}
	for i := range a {
		if a[i].QueryID != b[i].QueryID || a[i].NodeID != b[i].NodeID || a[i].GeometryID != b[i].GeometryID || a[i].Depth != b[i].Depth {
			return diverge(queryID, fmt.Sprintf("%s[%d].location", item, i), a[i], b[i])
		}
		if math.Abs(a[i].Bound-b[i].Bound) > tol {
			return diverge(queryID, fmt.Sprintf("%s[%d].bound", item, i), a[i].Bound, b[i].Bound)
		}
	}
	return nil
}

func compareAABB(objectID, item string, a, b AABB, tol float64) error {
	if !vecNear(a.Min, b.Min, tol) {
		return diverge(objectID, item+".min", a.Min, b.Min)
	}
	if !vecNear(a.Max, b.Max, tol) {
		return diverge(objectID, item+".max", a.Max, b.Max)
	}
	return nil
}

func compareSphere(objectID, item string, a, b Sphere, tol float64) error {
	if !vecNear(a.Center, b.Center, tol) {
		return diverge(objectID, item+".center", a.Center, b.Center)
	}
	if math.Abs(a.Radius-b.Radius) > tol {
		return diverge(objectID, item+".radius", a.Radius, b.Radius)
	}
	return nil
}

func vecNear(a, b Vec3, tol float64) bool {
	return math.Abs(a.X-b.X) <= tol && math.Abs(a.Y-b.Y) <= tol && math.Abs(a.Z-b.Z) <= tol
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func diverge(objectID, item string, left, right any) *CodedError {
	return &CodedError{
		Code: CodeDivergence, Op: "compare", ObjectID: objectID, DataItem: item,
		Left: left, Right: right, Message: "paths differ",
	}
}

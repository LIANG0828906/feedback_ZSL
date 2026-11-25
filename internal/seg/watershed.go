package seg

import (
	"container/heap"
	"sort"

	"iconseg/internal/core"
)

type RegionKey struct {
	Kind      core.SeedKind
	Component int
}

type WatershedResult struct {
	W            int
	H            int
	Labels       []int32
	RegionKeys   map[int32]RegionKey
	FloodSamples int
	Reached      int
}

type queueItem struct {
	grad  float64
	x     int
	y     int
	label int32
}

type priorityQueue []queueItem

func (q priorityQueue) Len() int { return len(q) }
func (q priorityQueue) Less(i, j int) bool {
	if q[i].grad != q[j].grad {
		return q[i].grad < q[j].grad
	}
	if q[i].y != q[j].y {
		return q[i].y < q[j].y
	}
	if q[i].x != q[j].x {
		return q[i].x < q[j].x
	}
	return q[i].label < q[j].label
}
func (q priorityQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *priorityQueue) Push(x any)   { *q = append(*q, x.(queueItem)) }
func (q *priorityQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[:n-1]
	return item
}

func markerData(w, h int, seeds []core.Seed) ([]int32, map[int32]RegionKey) {
	labels := make([]int32, w*h)
	keys := make(map[int32]RegionKey, len(seeds))
	for i, seed := range seeds {
		label := int32(i + 1)
		keys[label] = RegionKey{Kind: seed.Kind, Component: seed.Component}
		for y := seed.Y - 1; y <= seed.Y+1; y++ {
			for x := seed.X - 1; x <= seed.X+1; x++ {
				if x < 0 || y < 0 || x >= w || y >= h {
					continue
				}
				labels[y*w+x] = label
			}
		}
	}
	return labels, keys
}

func MakeMarkers(w, h int, seeds []core.Seed) ([]int32, map[int32]RegionKey) {
	return markerData(w, h, seeds)
}

// Flood performs a priority-queue watershed. The deterministic tie-break is
// (gradient, y, x, seed-label). The flood stops when the minimum queued
// gradient exceeds the configured threshold.
func Flood(g Gradient, seeds []core.Seed, threshold float64) WatershedResult {
	labels, keys := markerData(g.W, g.H, seeds)
	q := make(priorityQueue, 0, g.W*g.H)
	pushNeighbors := func(x, y int, label int32) {
		for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			nx, ny := x+d[0], y+d[1]
			if nx < 0 || ny < 0 || nx >= g.W || ny >= g.H {
				continue
			}
			if labels[ny*g.W+nx] != 0 {
				continue
			}
			heap.Push(&q, queueItem{grad: g.At(nx, ny), x: nx, y: ny, label: label})
		}
	}
	for y := 0; y < g.H; y++ {
		for x := 0; x < g.W; x++ {
			if label := labels[y*g.W+x]; label != 0 {
				pushNeighbors(x, y, label)
			}
		}
	}
	heap.Init(&q)
	reached := 0
	for y := 0; y < g.H; y++ {
		for x := 0; x < g.W; x++ {
			if labels[y*g.W+x] != 0 {
				reached++
			}
		}
	}
	samples := 0
	for q.Len() > 0 {
		item := heap.Pop(&q).(queueItem)
		if item.grad > threshold {
			break
		}
		samples++
		i := item.y*g.W + item.x
		if labels[i] != 0 {
			continue
		}
		labels[i] = item.label
		reached++
		pushNeighbors(item.x, item.y, item.label)
	}
	return WatershedResult{W: g.W, H: g.H, Labels: labels, RegionKeys: keys, FloodSamples: samples, Reached: reached}
}

// FillUnlabeled completes barriers left by the threshold. Frontier pixels are
// ordered by (gradient, y, x, neighboring label), independent of goroutine or
// scheduler order.
func (r *WatershedResult) FillUnlabeled(g Gradient) {
	q := make(priorityQueue, 0, g.W*g.H)
	pushFrontier := func(x, y int) {
		if x < 0 || y < 0 || x >= g.W || y >= g.H {
			return
		}
		if r.Labels[y*g.W+x] != 0 {
			return
		}
		bestLabel := int32(0)
		for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			nx, ny := x+d[0], y+d[1]
			if nx < 0 || ny < 0 || nx >= g.W || ny >= g.H {
				continue
			}
			label := r.Labels[ny*g.W+nx]
			if label != 0 && (bestLabel == 0 || label < bestLabel) {
				bestLabel = label
			}
		}
		if bestLabel != 0 {
			heap.Push(&q, queueItem{grad: g.At(x, y), x: x, y: y, label: bestLabel})
		}
	}
	for y := 0; y < g.H; y++ {
		for x := 0; x < g.W; x++ {
			if r.Labels[y*g.W+x] == 0 {
				pushFrontier(x, y)
			}
		}
	}
	heap.Init(&q)
	for q.Len() > 0 {
		item := heap.Pop(&q).(queueItem)
		i := item.y*g.W + item.x
		if r.Labels[i] != 0 {
			continue
		}
		bestLabel := int32(0)
		for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			nx, ny := item.x+d[0], item.y+d[1]
			if nx < 0 || ny < 0 || nx >= g.W || ny >= g.H {
				continue
			}
			label := r.Labels[ny*g.W+nx]
			if label != 0 && (bestLabel == 0 || label < bestLabel) {
				bestLabel = label
			}
		}
		if bestLabel == 0 {
			continue
		}
		r.Labels[i] = bestLabel
		r.Reached++
		for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			pushFrontier(item.x+d[0], item.y+d[1])
		}
	}
}

type RegionInfo struct {
	ID        int32
	Key       RegionKey
	Area      int
	FirstX    int
	FirstY    int
	Boundary  float64
	Neighbors map[int32]float64
}

type MergeResult struct {
	Labels     []int32
	Regions    map[int32]RegionInfo
	MergeCount int
	Isolated   []RegionInfo
}

type mergeCandidate struct {
	a, b   int32
	cost   float64
	areaA  int
	areaB  int
	firstA int
	firstB int
}

// MergeRegions builds a region adjacency graph and repeatedly applies the
// globally cheapest legal merge. Ties are resolved by (area, smaller ID,
// larger ID), then by row-major first-pixel position. Only regions with the
// same seed ownership are merged, preserving foreground/background and
// distinct-component boundaries.
func MergeRegions(w, h int, labels []int32, keys map[int32]RegionKey, grad Gradient,
	params core.SegmentParams) MergeResult {
	area := make(map[int32]int)
	first := make(map[int32]int)
	neighborMin := make(map[[2]int32]float64)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			label := labels[i]
			if label == 0 {
				continue
			}
			area[label]++
			if _, ok := first[label]; !ok {
				first[label] = i
			}
			for _, d := range [][2]int{{1, 0}, {0, 1}} {
				nx, ny := x+d[0], y+d[1]
				if nx >= w || ny >= h {
					continue
				}
				other := labels[ny*w+nx]
				if other == 0 || other == label {
					continue
				}
				a, b := label, other
				if a > b {
					a, b = b, a
				}
				cost := grad.At(x, y)
				if other := grad.At(nx, ny); other < cost {
					cost = other
				}
				pair := [2]int32{a, b}
				if old, ok := neighborMin[pair]; !ok || cost < old {
					neighborMin[pair] = cost
				}
			}
		}
	}
	parent := make(map[int32]int32)
	for label := range area {
		parent[label] = label
	}
	var find func(int32) int32
	find = func(label int32) int32 {
		if parent[label] != label {
			parent[label] = find(parent[label])
		}
		return parent[label]
	}
	mergeCount := 0
	for {
		neighbors := make(map[[2]int32]float64)
		for pair, boundary := range neighborMin {
			a, b := find(pair[0]), find(pair[1])
			if a == b || keys[a] != keys[b] {
				continue
			}
			if a > b {
				a, b = b, a
			}
			p := [2]int32{a, b}
			if old, ok := neighbors[p]; !ok || boundary < old {
				neighbors[p] = boundary
			}
		}
		var best *mergeCandidate
		for pair, boundary := range neighbors {
			a, b := pair[0], pair[1]
			areaA, areaB := area[a], area[b]
			areaDiff := areaA - areaB
			if areaDiff < 0 {
				areaDiff = -areaDiff
			}
			if areaDiff > params.MergeAreaDiff {
				continue
			}
			cost := boundary + 0.02*float64(areaDiff)
			if cost > params.MergeCostLimit {
				continue
			}
			c := mergeCandidate{a: a, b: b, cost: cost, areaA: areaA, areaB: areaB, firstA: first[a], firstB: first[b]}
			if best == nil || betterCandidate(c, *best) {
				copy := c
				best = &copy
			}
		}
		if best == nil {
			break
		}
		keep, drop := best.a, best.b
		if best.areaB < best.areaA || (best.areaB == best.areaA && best.firstB < best.firstA) {
			keep, drop = best.b, best.a
		}
		parent[drop] = keep
		area[keep] += area[drop]
		delete(area, drop)
		if first[drop] < first[keep] {
			first[keep] = first[drop]
		}
		delete(first, drop)
		mergeCount++
	}
	regions := make(map[int32]RegionInfo)
	finalLabels := make([]int32, len(labels))
	for i, label := range labels {
		if label == 0 {
			continue
		}
		root := find(label)
		finalLabels[i] = root
	}
	for i, label := range finalLabels {
		if label == 0 {
			continue
		}
		info := regions[label]
		info.ID = label
		info.Key = keys[label]
		info.Area++
		if info.FirstX == 0 && info.FirstY == 0 {
			info.FirstX = i % w
			info.FirstY = i / w
		}
		regions[label] = info
	}
	isolated := make([]RegionInfo, 0)
	for id, info := range regions {
		if info.Area < params.MinRegionSize {
			info.ID = id
			isolated = append(isolated, info)
		}
	}
	sort.Slice(isolated, func(i, j int) bool {
		if isolated[i].Area != isolated[j].Area {
			return isolated[i].Area < isolated[j].Area
		}
		return isolated[i].ID < isolated[j].ID
	})
	return MergeResult{Labels: finalLabels, Regions: regions, MergeCount: mergeCount, Isolated: isolated}
}

func betterCandidate(a, b mergeCandidate) bool {
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	minAreaA, minAreaB := a.areaA, a.areaB
	if minAreaB < minAreaA {
		minAreaA, minAreaB = minAreaB, minAreaA
	}
	bMinA, bMinB := b.areaA, b.areaB
	if bMinB < bMinA {
		bMinA, bMinB = bMinB, bMinA
	}
	if minAreaA != bMinA {
		return minAreaA < bMinA
	}
	if minAreaB != bMinB {
		return minAreaB < bMinB
	}
	if a.a != b.a {
		return a.a < b.a
	}
	if a.b != b.b {
		return a.b < b.b
	}
	if a.firstA != b.firstA {
		return a.firstA < b.firstA
	}
	return a.firstB < b.firstB
}

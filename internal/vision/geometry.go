package vision

import (
	"sort"

	"maskpipe/internal/model"
)

type labelQueue []int

func AnalyzeMask(mask []byte, w, h, minArea int) []model.Region {
	labels := make([]int32, w*h)
	var regions []model.Region
	current := int32(0)

	for start := 0; start < w*h; start++ {
		if mask[start] == 0 || labels[start] != 0 {
			continue
		}
		current++
		queue := []int{start}
		labels[start] = current
		var sumX, sumY, area int64
		minX, minY := w, h
		maxX, maxY := -1, -1
		for len(queue) > 0 {
			idx := queue[0]
			queue = queue[1:]
			x := idx % w
			y := idx / w
			area++
			sumX += int64(x)
			sumY += int64(y)
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					nx, ny := x+dx, y+dy
					if nx < 0 || nx >= w || ny < 0 || ny >= h {
						continue
					}
					ni := ny*w + nx
					if mask[ni] != 0 && labels[ni] == 0 {
						labels[ni] = current
						queue = append(queue, ni)
					}
				}
			}
		}
		if int(area) < minArea {
			continue
		}
		id := len(regions) + 1
		regionMask := make([]byte, w*h)
		for i, label := range labels {
			if label == current {
				regionMask[i] = 1
			}
		}
		contour := traceContour(regionMask, w, h)
		regions = append(regions, model.Region{
			ID:       id,
			Centroid: model.Point{X: float64(sumX)/float64(area) + 0.5, Y: float64(sumY)/float64(area) + 0.5},
			Area:     int(area),
			BBox: model.BBox{
				MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY,
				Width: maxX - minX + 1, Height: maxY - minY + 1,
			},
			Contour: contour,
			Closed:  len(contour) > 2,
		})
	}

	sort.SliceStable(regions, func(i, j int) bool {
		if regions[i].BBox.MinY != regions[j].BBox.MinY {
			return regions[i].BBox.MinY < regions[j].BBox.MinY
		}
		return regions[i].BBox.MinX < regions[j].BBox.MinX
	})
	for i := range regions {
		regions[i].ID = i + 1
	}
	return regions
}

type corner struct{ x, y int }
type edge struct{ from, to corner }

func traceContour(mask []byte, w, h int) []model.Point {
	var edges []edge
	addEdge := func(x1, y1, x2, y2 int) {
		edges = append(edges, edge{corner{x1, y1}, corner{x2, y2}})
	}
	fg := func(x, y int) bool {
		return x >= 0 && x < w && y >= 0 && y < h && mask[y*w+x] != 0
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if !fg(x, y) {
				continue
			}
			if !fg(x, y-1) {
				addEdge(x, y, x+1, y)
			}
			if !fg(x+1, y) {
				addEdge(x+1, y, x+1, y+1)
			}
			if !fg(x, y+1) {
				addEdge(x+1, y+1, x, y+1)
			}
			if !fg(x-1, y) {
				addEdge(x, y+1, x, y)
			}
		}
	}
	if len(edges) == 0 {
		return nil
	}

	outgoing := map[corner][]int{}
	for i, e := range edges {
		outgoing[e.from] = append(outgoing[e.from], i)
	}
	used := make([]bool, len(edges))
	var best []model.Point
	for startIdx := range edges {
		if used[startIdx] {
			continue
		}
		loop := []model.Point{{X: float64(edges[startIdx].from.x), Y: float64(edges[startIdx].from.y)}}
		used[startIdx] = true
		currentEnd := edges[startIdx].to
		for steps := 0; steps <= len(edges)+1; steps++ {
			loop = append(loop, model.Point{X: float64(currentEnd.x), Y: float64(currentEnd.y)})
			if currentEnd == edges[startIdx].from {
				break
			}
			candidates := outgoing[currentEnd]
			next := -1
			for _, idx := range candidates {
				if !used[idx] {
					next = idx
					break
				}
			}
			if next < 0 {
				break
			}
			used[next] = true
			currentEnd = edges[next].to
		}
		if len(loop) > len(best) {
			best = loop
		}
	}
	return simplifyContour(best)
}

func simplifyContour(points []model.Point) []model.Point {
	if len(points) <= 8 {
		return points
	}
	out := make([]model.Point, 0, len(points)/2+1)
	for i, p := range points {
		if i%2 == 0 {
			out = append(out, p)
		}
	}
	if len(out) > 1 && out[0] != out[len(out)-1] {
		out = append(out, out[0])
	}
	return out
}

func MergeChunkRegions(chunks [][]model.Region) []model.Region {
	var all []model.Region
	for _, rs := range chunks {
		all = append(all, rs...)
	}
	return all
}

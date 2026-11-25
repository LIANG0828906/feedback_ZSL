package segment

import (
	"sort"

	"maskpreview/internal/core"
)

type RegionStat struct {
	ID       int          `json:"id"`
	Area     int          `json:"area"`
	Centroid core.PointF  `json:"centroid"`
	BBox     core.BBox    `json:"bbox"`
	Contour  []core.Point `json:"contour"`
}

func Analyze(mask []uint8, width, height int) ([]RegionStat, []int) {
	return analyze(mask, width, height)
}

func analyze(mask []uint8, width, height int) ([]RegionStat, []int) {
	labels := make([]int, len(mask))
	type acc struct {
		area       int
		sumX, sumY float64
		x0, y0     int
		x1, y1     int
	}
	accum := map[int]*acc{}
	nextID := 0
	stack := make([]core.Point, 0, 64)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			idx := y*width + x
			if mask[idx] == 0 || labels[idx] != 0 {
				continue
			}
			nextID++
			a := &acc{x0: x, y0: y, x1: x, y1: y}
			labels[idx] = nextID
			stack = append(stack[:0], core.Point{X: x, Y: y})
			for len(stack) > 0 {
				p := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				a.area++
				a.sumX += float64(p.X) + 0.5
				a.sumY += float64(p.Y) + 0.5
				if p.X < a.x0 {
					a.x0 = p.X
				}
				if p.Y < a.y0 {
					a.y0 = p.Y
				}
				if p.X > a.x1 {
					a.x1 = p.X
				}
				if p.Y > a.y1 {
					a.y1 = p.Y
				}
				for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					nx, ny := p.X+d[0], p.Y+d[1]
					if nx < 0 || ny < 0 || nx >= width || ny >= height {
						continue
					}
					ni := ny*width + nx
					if mask[ni] != 0 && labels[ni] == 0 {
						labels[ni] = nextID
						stack = append(stack, core.Point{X: nx, Y: ny})
					}
				}
			}
			accum[nextID] = a
		}
	}
	ids := make([]int, 0, len(accum))
	for id := range accum {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	regions := make([]RegionStat, 0, len(ids))
	for _, id := range ids {
		a := accum[id]
		regions = append(regions, RegionStat{
			ID:       id,
			Area:     a.area,
			Centroid: core.PointF{X: a.sumX / float64(a.area), Y: a.sumY / float64(a.area)},
			BBox:     core.BBox{X0: a.x0, Y0: a.y0, X1: a.x1, Y1: a.y1},
			Contour:  traceOuterContour(labels, width, height, id),
		})
	}
	return regions, labels
}

type boundaryEdge struct {
	from core.Point
	to   core.Point
}

func traceOuterContour(labels []int, width, height, id int) []core.Point {
	edges := make([]boundaryEdge, 0, 64)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if labels[y*width+x] != id {
				continue
			}
			if y == 0 || labels[(y-1)*width+x] != id {
				edges = append(edges, boundaryEdge{from: core.Point{X: x, Y: y}, to: core.Point{X: x + 1, Y: y}})
			}
			if x == width-1 || labels[y*width+x+1] != id {
				edges = append(edges, boundaryEdge{from: core.Point{X: x + 1, Y: y}, to: core.Point{X: x + 1, Y: y + 1}})
			}
			if y == height-1 || labels[(y+1)*width+x] != id {
				edges = append(edges, boundaryEdge{from: core.Point{X: x + 1, Y: y + 1}, to: core.Point{X: x, Y: y + 1}})
			}
			if x == 0 || labels[y*width+x-1] != id {
				edges = append(edges, boundaryEdge{from: core.Point{X: x, Y: y + 1}, to: core.Point{X: x, Y: y}})
			}
		}
	}
	if len(edges) == 0 {
		return nil
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].from.Y != edges[j].from.Y {
			return edges[i].from.Y < edges[j].from.Y
		}
		if edges[i].from.X != edges[j].from.X {
			return edges[i].from.X < edges[j].from.X
		}
		if edges[i].to.Y != edges[j].to.Y {
			return edges[i].to.Y < edges[j].to.Y
		}
		return edges[i].to.X < edges[j].to.X
	})
	outgoing := map[core.Point][]int{}
	for i, e := range edges {
		outgoing[e.from] = append(outgoing[e.from], i)
	}
	used := make([]bool, len(edges))
	var best []core.Point
	for startIdx := range edges {
		if used[startIdx] {
			continue
		}
		loop := make([]core.Point, 0, 32)
		cur := startIdx
		for {
			if used[cur] {
				break
			}
			used[cur] = true
			e := edges[cur]
			if len(loop) == 0 || loop[len(loop)-1] != e.from {
				loop = append(loop, e.from)
			}
			next, ok := chooseNextEdge(edges, outgoing[e.to], used, e.from, e.to)
			if !ok {
				if e.to == loop[0] {
					loop = append(loop, e.to)
				}
				break
			}
			cur = next
			if edges[cur].from == loop[0] {
				loop = append(loop, edges[cur].from)
				break
			}
		}
		if len(loop) > len(best) {
			best = loop
		}
	}
	if len(best) > 1 && best[0] != best[len(best)-1] {
		best = append(best, best[0])
	}
	return best
}

func chooseNextEdge(edges []boundaryEdge, candidates []int, used []bool, from, at core.Point) (int, bool) {
	dx, dy := at.X-from.X, at.Y-from.Y
	preferred := [][2]int{
		{-dy, dx}, // right turn
		{dx, dy},  // straight
		{dy, -dx}, // left turn
		{-dx, -dy},
	}
	for _, dir := range preferred {
		want := core.Point{X: at.X + dir[0], Y: at.Y + dir[1]}
		for _, idx := range candidates {
			if !used[idx] && edges[idx].to == want {
				return idx, true
			}
		}
	}
	return 0, false
}

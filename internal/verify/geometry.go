package verify

import (
	"sort"

	"maskpipe/internal/model"
)

// AnalyzeRegionsIndependent is deliberately separate from the main vision
// implementation. It recomputes connected components and scalar geometry from
// persisted raw masks.
func AnalyzeRegionsIndependent(mask []byte, w, h, minArea int) []model.Region {
	visited := make([]byte, w*h)
	var regions []model.Region
	for start := 0; start < w*h; start++ {
		if mask[start] == 0 || visited[start] != 0 {
			continue
		}
		queue := []int{start}
		visited[start] = 1
		var area, sumX, sumY int64
		minX, minY, maxX, maxY := w, h, -1, -1
		for len(queue) > 0 {
			idx := queue[0]
			queue = queue[1:]
			x, y := idx%w, idx/w
			area++
			sumX += int64(x)
			sumY += int64(y)
			minX = minInt(minX, x)
			maxX = maxInt(maxX, x)
			minY = minInt(minY, y)
			maxY = maxInt(maxY, y)
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
					if mask[ni] != 0 && visited[ni] == 0 {
						visited[ni] = 1
						queue = append(queue, ni)
					}
				}
			}
		}
		if int(area) < minArea {
			continue
		}
		regions = append(regions, model.Region{
			Centroid: model.Point{X: float64(sumX)/float64(area) + 0.5, Y: float64(sumY)/float64(area) + 0.5},
			Area:     int(area),
			BBox: model.BBox{
				MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY,
				Width: maxX - minX + 1, Height: maxY - minY + 1,
			},
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

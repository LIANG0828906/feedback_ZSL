package iconmask

import (
	"fmt"
	"sort"
)

type labeledComponent struct {
	pixels []int
	minX   int
	minY   int
	maxX   int
	maxY   int
}

func AnalyzeMask(mask Mask) RegionStats {
	components := connectedComponents(mask)
	stats := RegionStats{Count: len(components), Regions: make([]ComponentTruth, 0, len(components))}
	for i, component := range components {
		region := analyzeComponent(i, component, mask.Width, mask.Height)
		stats.TotalArea += region.Area
		stats.Regions = append(stats.Regions, region)
	}
	return stats
}

func analyzeComponent(index int, component labeledComponent, width, height int) ComponentTruth {
	area := len(component.pixels)
	sumX, sumY := 0.0, 0.0
	inside := make([]bool, width*height)
	for _, idx := range component.pixels {
		x, y := idx%width, idx/width
		sumX += float64(x) + 0.5
		sumY += float64(y) + 0.5
		inside[idx] = true
	}
	bbox := Rect{
		X:      component.minX,
		Y:      component.minY,
		Width:  component.maxX - component.minX + 1,
		Height: component.maxY - component.minY + 1,
	}
	return ComponentTruth{
		ID:       fmt.Sprintf("r%d", index),
		Centroid: Point{X: sumX / float64(area), Y: sumY / float64(area)},
		Area:     area,
		BBox:     bbox,
		Holes:    countHoles(inside, bbox, width),
	}
}

func connectedComponents(mask Mask) []labeledComponent {
	if mask.Width <= 0 || mask.Height <= 0 || len(mask.Data) != mask.Width*mask.Height {
		return nil
	}
	visited := make([]bool, len(mask.Data))
	components := make([]labeledComponent, 0)
	queue := make([]int, 0, 64)
	for y := 0; y < mask.Height; y++ {
		for x := 0; x < mask.Width; x++ {
			start := y*mask.Width + x
			if visited[start] || mask.Data[start] == 0 {
				continue
			}
			component := labeledComponent{
				minX: x, minY: y, maxX: x, maxY: y,
			}
			visited[start] = true
			queue = queue[:0]
			queue = append(queue, start)
			for len(queue) > 0 {
				idx := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				component.pixels = append(component.pixels, idx)
				cx, cy := idx%mask.Width, idx/mask.Width
				if cx < component.minX {
					component.minX = cx
				}
				if cx > component.maxX {
					component.maxX = cx
				}
				if cy < component.minY {
					component.minY = cy
				}
				if cy > component.maxY {
					component.maxY = cy
				}
				if cx > 0 {
					addComponentNeighbor(mask, visited, &queue, idx-1)
				}
				if cx+1 < mask.Width {
					addComponentNeighbor(mask, visited, &queue, idx+1)
				}
				if cy > 0 {
					addComponentNeighbor(mask, visited, &queue, idx-mask.Width)
				}
				if cy+1 < mask.Height {
					addComponentNeighbor(mask, visited, &queue, idx+mask.Width)
				}
			}
			components = append(components, component)
		}
	}
	sort.Slice(components, func(i, j int) bool {
		if components[i].minY != components[j].minY {
			return components[i].minY < components[j].minY
		}
		return components[i].minX < components[j].minX
	})
	return components
}

func addComponentNeighbor(mask Mask, visited []bool, queue *[]int, idx int) {
	if !visited[idx] && mask.Data[idx] != 0 {
		visited[idx] = true
		*queue = append(*queue, idx)
	}
}

// countHoles counts background components fully enclosed by a foreground
// component's bounding box.
func countHoles(foreground []bool, bbox Rect, width int) int {
	if bbox.Empty() {
		return 0
	}
	seen := make([]bool, len(foreground))
	queue := make([]int, 0, 32)
	holes := 0
	for y := bbox.Y; y < bbox.Y+bbox.Height; y++ {
		for x := bbox.X; x < bbox.X+bbox.Width; x++ {
			idx := y*width + x
			if foreground[idx] || seen[idx] {
				continue
			}
			seen[idx] = true
			queue = queue[:0]
			queue = append(queue, idx)
			touchesBorder := false
			for len(queue) > 0 {
				cur := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				cx, cy := cur%width, cur/width
				if cx == bbox.X || cy == bbox.Y || cx == bbox.X+bbox.Width-1 || cy == bbox.Y+bbox.Height-1 {
					touchesBorder = true
				}
				tryBackgroundNeighbor(foreground, seen, &queue, bbox, width, cx-1, cy)
				tryBackgroundNeighbor(foreground, seen, &queue, bbox, width, cx+1, cy)
				tryBackgroundNeighbor(foreground, seen, &queue, bbox, width, cx, cy-1)
				tryBackgroundNeighbor(foreground, seen, &queue, bbox, width, cx, cy+1)
			}
			if !touchesBorder {
				holes++
			}
		}
	}
	return holes
}

func tryBackgroundNeighbor(foreground, seen []bool, queue *[]int, bbox Rect, width, x, y int) {
	if x < bbox.X || y < bbox.Y || x >= bbox.X+bbox.Width || y >= bbox.Y+bbox.Height {
		return
	}
	idx := y*width + x
	if foreground[idx] || seen[idx] {
		return
	}
	seen[idx] = true
	*queue = append(*queue, idx)
}

func LabelMask(mask Mask) []int {
	components := connectedComponents(mask)
	labels := make([]int, len(mask.Data))
	for i := range labels {
		labels[i] = -1
	}
	for componentIndex, component := range components {
		for _, idx := range component.pixels {
			labels[idx] = componentIndex
		}
	}
	return labels
}

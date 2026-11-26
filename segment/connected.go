package segment

import (
	"sort"

	"iconflow/model"
)

type Component struct {
	Area     int            `json:"area"`
	Centroid model.Centroid `json:"centroid"`
	Pixels   []int          `json:"-"`
}

type BlockStats struct {
	Coord       model.BlockCoord `json:"coord"`
	Foreground  int              `json:"foreground"`
	Components  []Component      `json:"components"`
	RemovedArea int              `json:"removed_area"`
	FilledHoles int              `json:"filled_holes"`
	Mask        []bool           `json:"mask"`
}

func neighbors(index, width, height, connectivity int) []int {
	x := index % width
	y := index / width
	var out []int
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if dx == 0 && dy == 0 {
				continue
			}
			if connectivity == 4 && dx != 0 && dy != 0 {
				continue
			}
			nx := x + dx
			ny := y + dy
			if nx < 0 || nx >= width || ny < 0 || ny >= height {
				continue
			}
			out = append(out, ny*width+nx)
		}
	}
	return out
}

func labelComponents(mask []bool, width, height, connectivity int) [][]int {
	labels := make([]int, width*height)
	var components [][]int
	nextLabel := 0
	for start := 0; start < width*height; start++ {
		if !mask[start] || labels[start] != 0 {
			continue
		}
		nextLabel++
		queue := []int{start}
		labels[start] = nextLabel
		var pixels []int
		for len(queue) > 0 {
			idx := queue[0]
			queue = queue[1:]
			pixels = append(pixels, idx)
			for _, candidate := range neighbors(idx, width, height, connectivity) {
				if mask[candidate] && labels[candidate] == 0 {
					labels[candidate] = nextLabel
					queue = append(queue, candidate)
				}
			}
		}
		components = append(components, pixels)
	}
	sort.Slice(components, func(i, j int) bool { return components[i][0] < components[j][0] })
	return components
}

func fillHoles(mask []bool, width, height int) (filled []bool, holeCount int) {
	filled = append([]bool(nil), mask...)
	visited := make([]bool, width*height)
	queue := []int{}
	for x := 0; x < width; x++ {
		for _, y := range []int{0, height - 1} {
			idx := y*width + x
			if !filled[idx] && !visited[idx] {
				visited[idx] = true
				queue = append(queue, idx)
			}
		}
	}
	for y := 0; y < height; y++ {
		for _, x := range []int{0, width - 1} {
			idx := y*width + x
			if !filled[idx] && !visited[idx] {
				visited[idx] = true
				queue = append(queue, idx)
			}
		}
	}
	for len(queue) > 0 {
		idx := queue[0]
		queue = queue[1:]
		x := idx % width
		y := idx / width
		for _, delta := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			nx, ny := x+delta[0], y+delta[1]
			if nx < 0 || nx >= width || ny < 0 || ny >= height {
				continue
			}
			nidx := ny*width + nx
			if filled[nidx] || visited[nidx] {
				continue
			}
			visited[nidx] = true
			queue = append(queue, nidx)
		}
	}
	for idx := range filled {
		if !filled[idx] && !visited[idx] {
			filled[idx] = true
			holeCount++
		}
	}
	return filled, holeCount
}

func Analyze(mask []bool, coord model.BlockCoord, rules model.Rules) *BlockStats {
	width, height := coord.Width, coord.Height
	if len(mask) != width*height {
		return nil
	}
	connectivity := rules.Connectivity
	if connectivity != 4 {
		connectivity = 8
	}
	cleaned := append([]bool(nil), mask...)
	removedArea := 0
	for _, pixels := range labelComponents(cleaned, width, height, connectivity) {
		if len(pixels) >= rules.MinComponentArea {
			continue
		}
		for _, idx := range pixels {
			cleaned[idx] = false
			removedArea++
		}
	}

	holeCount := 0
	if rules.FillHoles {
		cleaned, holeCount = fillHoles(cleaned, width, height)
	}

	finalComponentsPixels := labelComponents(cleaned, width, height, connectivity)
	components := make([]Component, 0, len(finalComponentsPixels))
	foreground := 0
	for _, pixels := range finalComponentsPixels {
		var sumX, sumY int64
		for _, idx := range pixels {
			x := idx % width
			y := idx / width
			sumX += int64(x)
			sumY += int64(y)
		}
		area := len(pixels)
		foreground += area
		components = append(components, Component{
			Area:   area,
			Pixels: append([]int(nil), pixels...),
			Centroid: model.Centroid{
				XMilli: sumX * 1000 / int64(area),
				YMilli: sumY * 1000 / int64(area),
			},
		})
	}
	return &BlockStats{
		Coord: coord, Foreground: foreground, Components: components,
		RemovedArea: removedArea, FilledHoles: holeCount, Mask: cleaned,
	}
}

package imaging

import (
	"fmt"

	"iconbench/internal/model"
)

func ConnectedComponents(mask []byte, w, h int) ([]int32, []model.RegionStat) {
	labels := make([]int32, w*h)
	var stats []model.RegionStat
	visited := make([]bool, w*h)

	for start := 0; start < w*h; start++ {
		if mask[start] == 0 || visited[start] {
			continue
		}
		id := int32(len(stats) + 1)
		stack := []int{start}
		visited[start] = true
		labels[start] = id
		stat := model.RegionStat{Index: len(stats), ID: fmt.Sprintf("R%d", id)}
		minX, minY, maxX, maxY := w, h, -1, -1
		var sumX, sumY int64

		for len(stack) > 0 {
			p := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := p%w, p/w
			stat.Area++
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
					q := ny*w + nx
					if visited[q] || mask[q] == 0 {
						continue
					}
					visited[q] = true
					labels[q] = id
					stack = append(stack, q)
				}
			}
		}

		stat.CentroidX = float64(sumX) / float64(stat.Area)
		stat.CentroidY = float64(sumY) / float64(stat.Area)
		stat.BBox = model.BBox{X: minX, Y: minY, Width: maxX - minX + 1, Height: maxY - minY + 1}
		stat.Holes = countHoles(labels, w, h, id, stat.BBox)
		stats = append(stats, stat)
	}
	return labels, stats
}

func countHoles(labels []int32, w, h int, id int32, bbox model.BBox) int {
	x0 := bbox.X - 1
	y0 := bbox.Y - 1
	pw := bbox.Width + 2
	ph := bbox.Height + 2
	padded := make([]byte, pw*ph)
	for py := 0; py < ph; py++ {
		y := y0 + py
		if y < 0 || y >= h {
			continue
		}
		for px := 0; px < pw; px++ {
			x := x0 + px
			if x < 0 || x >= w {
				continue
			}
			if labels[y*w+x] == id {
				padded[py*pw+px] = 1
			}
		}
	}

	visited := make([]bool, pw*ph)
	stack := make([]int, 0, pw+ph)
	seed := func(x, y int) {
		i := y*pw + x
		if !visited[i] && padded[i] == 0 {
			visited[i] = true
			stack = append(stack, i)
		}
	}
	for x := 0; x < pw; x++ {
		seed(x, 0)
		seed(x, ph-1)
	}
	for y := 0; y < ph; y++ {
		seed(0, y)
		seed(pw-1, y)
	}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		x, y := p%pw, p/pw
		neighbors := [4]int{}
		count := 0
		if x > 0 {
			neighbors[count] = p - 1
			count++
		}
		if x+1 < pw {
			neighbors[count] = p + 1
			count++
		}
		if y > 0 {
			neighbors[count] = p - pw
			count++
		}
		if y+1 < ph {
			neighbors[count] = p + pw
			count++
		}
		for _, q := range neighbors[:count] {
			if !visited[q] && padded[q] == 0 {
				visited[q] = true
				stack = append(stack, q)
			}
		}
	}

	holes := 0
	for i := 0; i < pw*ph; i++ {
		if padded[i] != 0 || visited[i] {
			continue
		}
		holes++
		visited[i] = true
		stack = append(stack[:0], i)
		for len(stack) > 0 {
			p := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := p%pw, p/pw
			if x > 0 {
				q := p - 1
				if !visited[q] && padded[q] == 0 {
					visited[q] = true
					stack = append(stack, q)
				}
			}
			if x+1 < pw {
				q := p + 1
				if !visited[q] && padded[q] == 0 {
					visited[q] = true
					stack = append(stack, q)
				}
			}
			if y > 0 {
				q := p - pw
				if !visited[q] && padded[q] == 0 {
					visited[q] = true
					stack = append(stack, q)
				}
			}
			if y+1 < ph {
				q := p + pw
				if !visited[q] && padded[q] == 0 {
					visited[q] = true
					stack = append(stack, q)
				}
			}
		}
	}
	return holes
}

func MaskMetrics(a, b []byte) model.DivergenceMetrics {
	var inter, union, mismatch int
	for i := range a {
		ai := a[i] != 0
		bi := b[i] != 0
		if ai && bi {
			inter++
		}
		if ai || bi {
			union++
		}
		if ai != bi {
			mismatch++
		}
	}
	iou := 1.0
	if union > 0 {
		iou = float64(inter) / float64(union)
	}
	return model.DivergenceMetrics{MismatchPixels: mismatch, IoU: iou}
}

func FirstMaskDifference(a, b []byte, w, h int) (int, int, bool) {
	for i := range a {
		if a[i] != b[i] {
			return i % w, i / w, true
		}
	}
	return -1, -1, false
}

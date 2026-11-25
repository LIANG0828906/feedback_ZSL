package seg

import (
	"sort"

	"iconseg/internal/core"
)

type Component = core.MaskComponent

func AnalyzeMask(mask []byte, w, h int) []Component {
	seen := make([]bool, len(mask))
	var components []Component
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			start := y*w + x
			if mask[start] == 0 || seen[start] {
				continue
			}
			stack := []int{start}
			seen[start] = true
			area := 0
			sumX, sumY := 0.0, 0.0
			minX, minY, maxX, maxY := x, y, x, y
			for len(stack) > 0 {
				p := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				px, py := p%w, p/w
				area++
				sumX += float64(px) + 0.5
				sumY += float64(py) + 0.5
				if px < minX {
					minX = px
				}
				if px > maxX {
					maxX = px
				}
				if py < minY {
					minY = py
				}
				if py > maxY {
					maxY = py
				}
				for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					nx, ny := px+d[0], py+d[1]
					if nx < 0 || ny < 0 || nx >= w || ny >= h {
						continue
					}
					np := ny*w + nx
					if mask[np] != 0 && !seen[np] {
						seen[np] = true
						stack = append(stack, np)
					}
				}
			}
			bbox := core.Rect{X: minX, Y: minY, W: maxX - minX + 1, H: maxY - minY + 1}
			components = append(components, Component{
				CentroidX: sumX / float64(area), CentroidY: sumY / float64(area),
				Area: area, BBox: bbox, HoleCount: countHoles(mask, w, h, minX-1, minY-1, maxX+1, maxY+1),
				PixelCount: area,
			})
		}
	}
	sort.Slice(components, func(i, j int) bool {
		if components[i].BBox.Y != components[j].BBox.Y {
			return components[i].BBox.Y < components[j].BBox.Y
		}
		if components[i].BBox.X != components[j].BBox.X {
			return components[i].BBox.X < components[j].BBox.X
		}
		if components[i].Area != components[j].Area {
			return components[i].Area < components[j].Area
		}
		return components[i].CentroidX < components[j].CentroidX
	})
	for i := range components {
		components[i].ID = i + 1
	}
	return components
}

func countHoles(mask []byte, w, h, minX, minY, maxX, maxY int) int {
	if minX < 0 {
		minX = 0
	}
	if minY < 0 {
		minY = 0
	}
	if maxX >= w {
		maxX = w - 1
	}
	if maxY >= h {
		maxY = h - 1
	}
	ww, hh := maxX-minX+1, maxY-minY+1
	seen := make([]bool, ww*hh)
	queue := make([]int, 0, ww)
	for x := 0; x < ww; x++ {
		queue = append(queue, x, (hh-1)*ww+x)
	}
	for y := 0; y < hh; y++ {
		queue = append(queue, y*ww, y*ww+ww-1)
	}
	for len(queue) > 0 {
		p := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[p] {
			continue
		}
		seen[p] = true
		x, y := p%ww, p/ww
		if mask[(minY+y)*w+minX+x] != 0 {
			continue
		}
		if x > 0 {
			queue = append(queue, p-1)
		}
		if x+1 < ww {
			queue = append(queue, p+1)
		}
		if y > 0 {
			queue = append(queue, p-ww)
		}
		if y+1 < hh {
			queue = append(queue, p+ww)
		}
	}
	holes := 0
	for y := 0; y < hh; y++ {
		for x := 0; x < ww; x++ {
			p := y*ww + x
			if seen[p] || mask[(minY+y)*w+minX+x] != 0 {
				continue
			}
			holes++
			seen[p] = true
			stack := []int{p}
			for len(stack) > 0 {
				q := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				qx, qy := q%ww, q/ww
				if qx > 0 && !seen[q-1] && mask[(minY+qy)*w+minX+qx-1] == 0 {
					seen[q-1] = true
					stack = append(stack, q-1)
				}
				if qx+1 < ww && !seen[q+1] && mask[(minY+qy)*w+minX+qx+1] == 0 {
					seen[q+1] = true
					stack = append(stack, q+1)
				}
				if qy > 0 && !seen[q-ww] && mask[(minY+qy-1)*w+minX+qx] == 0 {
					seen[q-ww] = true
					stack = append(stack, q-ww)
				}
				if qy+1 < hh && !seen[q+ww] && mask[(minY+qy+1)*w+minX+qx] == 0 {
					seen[q+ww] = true
					stack = append(stack, q+ww)
				}
			}
		}
	}
	return holes
}

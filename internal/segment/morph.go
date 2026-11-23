package segment

func morph(mask []bool, width, height, radius int, dilate bool) []bool {
	if radius <= 0 {
		return append([]bool(nil), mask...)
	}
	out := make([]bool, len(mask))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			value := !dilate
			if dilate {
				value = false
			}
			for dy := -radius; dy <= radius && !(dilate && value); dy++ {
				ny := y + dy
				if ny < 0 || ny >= height {
					if !dilate {
						value = false
						break
					}
					continue
				}
				for dx := -radius; dx <= radius; dx++ {
					nx := x + dx
					if nx < 0 || nx >= width {
						if !dilate {
							value = false
						}
						if !dilate && !value {
							break
						}
						continue
					}
					m := mask[ny*width+nx]
					if dilate && m {
						value = true
						break
					}
					if !dilate && !m {
						value = false
						break
					}
				}
			}
			out[y*width+x] = value
		}
	}
	return out
}

func OpenClose(mask []bool, width, height, structureElementSize int) []bool {
	radius := structureElementSize / 2
	if radius == 0 {
		return append([]bool(nil), mask...)
	}
	opened := morph(morph(mask, width, height, radius, false), width, height, radius, true)
	return morph(morph(opened, width, height, radius, true), width, height, radius, false)
}

func largestComponentTouchingSeed(mask []bool, width, height int, seedX, seedY int) []bool {
	seen := make([]bool, len(mask))
	best := []int(nil)
	bestArea := 0
	bestHasSeed := false
	queue := make([]int, 0, 1024)
	for start := range mask {
		if !mask[start] || seen[start] {
			continue
		}
		queue = queue[:0]
		queue = append(queue, start)
		seen[start] = true
		component := make([]int, 0, 256)
		hasSeed := false
		for head := 0; head < len(queue); head++ {
			idx := queue[head]
			component = append(component, idx)
			x, y := idx%width, idx/width
			if x == seedX && y == seedY {
				hasSeed = true
			}
			if y > 0 {
				n := idx - width
				if mask[n] && !seen[n] {
					seen[n] = true
					queue = append(queue, n)
				}
			}
			if x > 0 {
				n := idx - 1
				if mask[n] && !seen[n] {
					seen[n] = true
					queue = append(queue, n)
				}
			}
			if x+1 < width {
				n := idx + 1
				if mask[n] && !seen[n] {
					seen[n] = true
					queue = append(queue, n)
				}
			}
			if y+1 < height {
				n := idx + width
				if mask[n] && !seen[n] {
					seen[n] = true
					queue = append(queue, n)
				}
			}
		}
		if (hasSeed && !bestHasSeed) || (hasSeed == bestHasSeed && len(component) > bestArea) {
			best = append(best[:0], component...)
			bestArea = len(component)
			bestHasSeed = hasSeed
		}
	}
	out := make([]bool, len(mask))
	for _, idx := range best {
		out[idx] = true
	}
	return out
}

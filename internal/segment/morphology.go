package segment

// morphologyOpenClose performs opening-by-reconstruction followed by
// closing-by-reconstruction. The structuring element still controls which
// small foreground components survive; reconstruction avoids eroding the
// boundary of valid icons, so the final mask remains faithful to the analytic
// truth while isolated embedded noise and holes are removed.
func morphologyOpenClose(mask []uint8, width, height, kernel int) []uint8 {
	if kernel <= 1 {
		return append([]uint8(nil), mask...)
	}
	seed := erode(mask, width, height, kernel)
	opened := reconstruct(mask, seed, width, height)
	return fillHoles(opened, width, height)
}

func erode(src []uint8, width, height, kernel int) []uint8 {
	dst := make([]uint8, len(src))
	r := kernel / 2
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			all := true
			for yy := y - r; yy <= y+r && all; yy++ {
				if yy < 0 || yy >= height {
					all = false
					break
				}
				for xx := x - r; xx <= x+r; xx++ {
					if xx < 0 || xx >= width || src[yy*width+xx] == 0 {
						all = false
						break
					}
				}
			}
			if all {
				dst[y*width+x] = 1
			}
		}
	}
	return dst
}

func reconstruct(src, seed []uint8, width, height int) []uint8 {
	out := append([]uint8(nil), seed...)
	queue := make([]int, 0, len(seed))
	for i, v := range seed {
		if v != 0 {
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		idx := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		x, y := idx%width, idx/width
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				if dx == 0 && dy == 0 {
					continue
				}
				nx, ny := x+dx, y+dy
				if nx < 0 || ny < 0 || nx >= width || ny >= height {
					continue
				}
				ni := ny*width + nx
				if src[ni] != 0 && out[ni] == 0 {
					out[ni] = 1
					queue = append(queue, ni)
				}
			}
		}
	}
	return out
}

func fillHoles(mask []uint8, width, height int) []uint8 {
	background := make([]bool, len(mask))
	queue := make([]int, 0, len(mask))
	push := func(x, y int) {
		if x < 0 || y < 0 || x >= width || y >= height {
			return
		}
		idx := y*width + x
		if mask[idx] == 0 && !background[idx] {
			background[idx] = true
			queue = append(queue, idx)
		}
	}
	for x := 0; x < width; x++ {
		push(x, 0)
		push(x, height-1)
	}
	for y := 0; y < height; y++ {
		push(0, y)
		push(width-1, y)
	}
	for len(queue) > 0 {
		idx := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		x, y := idx%width, idx/width
		push(x-1, y)
		push(x+1, y)
		push(x, y-1)
		push(x, y+1)
	}
	out := append([]uint8(nil), mask...)
	for i := range out {
		if out[i] == 0 && !background[i] {
			out[i] = 1
		}
	}
	return out
}

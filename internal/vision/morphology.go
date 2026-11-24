package vision

func getReplicate(mask []byte, w, h, x, y int) byte {
	if x < 0 {
		x = 0
	} else if x >= w {
		x = w - 1
	}
	if y < 0 {
		y = 0
	} else if y >= h {
		y = h - 1
	}
	return mask[y*w+x]
}

func morphOp(mask []byte, w, h, radius int, dilate bool) []byte {
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			result := byte(0)
			if !dilate {
				result = 1
			}
			for dy := -radius; dy <= radius; dy++ {
				for dx := -radius; dx <= radius; dx++ {
					v := getReplicate(mask, w, h, x+dx, y+dy)
					if dilate && v != 0 {
						result = 1
						dy = radius + 1
						break
					}
					if !dilate && v == 0 {
						result = 0
						dy = radius + 1
						break
					}
				}
			}
			out[y*w+x] = result
		}
	}
	return out
}

// MorphClean applies opening followed by closing with a square structuring
// element. Border replication prevents tile-edge erosion seams.
func MorphClean(mask []byte, w, h, structureSize int) []byte {
	radius := (structureSize - 1) / 2
	if radius == 0 {
		return append([]byte(nil), mask...)
	}
	opened := morphOp(mask, w, h, radius, false)
	opened = morphOp(opened, w, h, radius, true)
	closed := morphOp(opened, w, h, radius, true)
	closed = morphOp(closed, w, h, radius, false)
	return closed
}

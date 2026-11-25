package seg

func minFilter(mask []byte, w, h, radius int) []byte {
	if radius <= 0 {
		return append([]byte(nil), mask...)
	}
	tmp := make([]byte, len(mask))
	out := make([]byte, len(mask))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			value := byte(1)
			for k := -radius; k <= radius; k++ {
				xx := x + k
				if xx < 0 || xx >= w || mask[y*w+xx] == 0 {
					value = 0
					break
				}
			}
			tmp[y*w+x] = value
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			value := byte(1)
			for k := -radius; k <= radius; k++ {
				yy := y + k
				if yy < 0 || yy >= h || tmp[yy*w+x] == 0 {
					value = 0
					break
				}
			}
			out[y*w+x] = value
		}
	}
	return out
}

func maxFilter(mask []byte, w, h, radius int) []byte {
	if radius <= 0 {
		return append([]byte(nil), mask...)
	}
	tmp := make([]byte, len(mask))
	out := make([]byte, len(mask))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			value := byte(0)
			for k := -radius; k <= radius; k++ {
				xx := x + k
				if xx < 0 || xx >= w {
					continue
				}
				if mask[y*w+xx] != 0 {
					value = 1
					break
				}
			}
			tmp[y*w+x] = value
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			value := byte(0)
			for k := -radius; k <= radius; k++ {
				yy := y + k
				if yy < 0 || yy >= h {
					continue
				}
				if tmp[yy*w+x] != 0 {
					value = 1
					break
				}
			}
			out[y*w+x] = value
		}
	}
	return out
}

func Erode(mask []byte, w, h, radius int) []byte {
	return minFilter(mask, w, h, radius)
}

func Dilate(mask []byte, w, h, radius int) []byte {
	return maxFilter(mask, w, h, radius)
}

func Open(mask []byte, w, h, radius int) []byte {
	if radius <= 0 {
		return append([]byte(nil), mask...)
	}
	return Dilate(Erode(mask, w, h, radius), w, h, radius)
}

func Close(mask []byte, w, h, radius int) []byte {
	if radius <= 0 {
		return append([]byte(nil), mask...)
	}
	return Erode(Dilate(mask, w, h, radius), w, h, radius)
}

// Clean applies opening then closing, matching the two-step cleanup contract.
func Clean(mask []byte, w, h, radius int) []byte {
	return Close(Open(mask, w, h, radius), w, h, radius)
}

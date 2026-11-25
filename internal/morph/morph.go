package morph

type Offset struct{ X, Y int }

func Disk(radius int) []Offset {
	if radius <= 0 {
		return []Offset{{0, 0}}
	}
	var offs []Offset
	r2 := radius * radius
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			if dx*dx+dy*dy <= r2+radius {
				offs = append(offs, Offset{X: dx, Y: dy})
			}
		}
	}
	return offs
}

func get(mask []byte, w, h, x, y int) byte {
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

func Erode(mask []byte, w, h, radius int) []byte {
	offs := Disk(radius)
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := byte(255)
			for _, o := range offs {
				if get(mask, w, h, x+o.X, y+o.Y) == 0 {
					v = 0
					break
				}
			}
			out[y*w+x] = v
		}
	}
	return out
}

func Dilate(mask []byte, w, h, radius int) []byte {
	offs := Disk(radius)
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := byte(0)
			for _, o := range offs {
				if get(mask, w, h, x+o.X, y+o.Y) != 0 {
					v = 255
					break
				}
			}
			out[y*w+x] = v
		}
	}
	return out
}

func Open(mask []byte, w, h, radius int) []byte {
	return Dilate(Erode(mask, w, h, radius), w, h, radius)
}

func Close(mask []byte, w, h, radius int) []byte {
	return Erode(Dilate(mask, w, h, radius), w, h, radius)
}

func OpenClose(mask []byte, w, h, radius int) []byte {
	return Close(Open(mask, w, h, radius), w, h, radius)
}

func InternalBoundary(mask []byte, w, h int) []byte {
	eroded := Erode(mask, w, h, 1)
	out := make([]byte, w*h)
	for i := range mask {
		if mask[i] != 0 && eroded[i] == 0 {
			out[i] = 255
		}
	}
	return out
}

func Threshold(pix []uint8, threshold float64) []byte {
	out := make([]byte, len(pix))
	for i, v := range pix {
		if float64(v) >= threshold {
			out[i] = 255
		}
	}
	return out
}

func Equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func FirstDifference(a, b []byte) (int, int, bool) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i, 0, true
		}
	}
	if len(a) != len(b) {
		return n, 0, true
	}
	return 0, 0, false
}

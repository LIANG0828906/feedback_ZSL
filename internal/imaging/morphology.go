package imaging

type offset struct{ dx, dy int }

func diskOffsets(radius int) []offset {
	if radius <= 0 {
		return []offset{{0, 0}}
	}
	var out []offset
	r2 := radius * radius
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			if dx*dx+dy*dy <= r2 {
				out = append(out, offset{dx, dy})
			}
		}
	}
	return out
}

func Dilate(mask []byte, w, h, radius int) []byte {
	offs := diskOffsets(radius)
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := byte(0)
			for _, o := range offs {
				xx, yy := x+o.dx, y+o.dy
				if xx >= 0 && xx < w && yy >= 0 && yy < h && mask[yy*w+xx] != 0 {
					v = 1
					break
				}
			}
			out[y*w+x] = v
		}
	}
	return out
}

func Erode(mask []byte, w, h, radius int) []byte {
	offs := diskOffsets(radius)
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := byte(1)
			for _, o := range offs {
				xx, yy := x+o.dx, y+o.dy
				if xx < 0 || xx >= w || yy < 0 || yy >= h || mask[yy*w+xx] == 0 {
					v = 0
					break
				}
			}
			out[y*w+x] = v
		}
	}
	return out
}

func Open(mask []byte, w, h, radius int) []byte {
	if radius <= 0 {
		out := make([]byte, len(mask))
		copy(out, mask)
		return out
	}
	return Dilate(Erode(mask, w, h, radius), w, h, radius)
}

func Close(mask []byte, w, h, radius int) []byte {
	if radius <= 0 {
		out := make([]byte, len(mask))
		copy(out, mask)
		return out
	}
	return Erode(Dilate(mask, w, h, radius), w, h, radius)
}

func OpenClose(mask []byte, w, h, radius int) []byte {
	return Close(Open(mask, w, h, radius), w, h, radius)
}

package strokelab

// RasterizeStroke renders one stroke into a full-canvas coverage component.
// Geometry is evaluated in fixed-point 1/16 pixel units and every distance
// comparison stays integral. The values are deterministic and independent of
// worker scheduling.
func RasterizeStroke(stroke Stroke, width, height int) []uint8 {
	out := make([]uint8, width*height)
	weightAlpha := int((int64(stroke.Weight)*255 + WeightMax/2) / WeightMax)
	if weightAlpha <= 0 {
		return out
	}
	centerAlpha := clampU8(int64(weightAlpha))
	fillRect(out, width, Rect{X: 0, Y: 0, W: width, H: height},
		stroke.Start, stroke.End, stroke.Radius, stroke.Hardness, centerAlpha)
	return out
}

// RasterizeStrokeRect renders a stroke clipped to a block rect. The returned
// buffer is local to the block and has exactly rect.W*rect.H bytes.
func RasterizeStrokeRect(stroke Stroke, rect Rect) []uint8 {
	out := make([]uint8, rect.PixelCount())
	if rect.Empty() {
		return out
	}
	weightAlpha := int((int64(stroke.Weight)*255 + WeightMax/2) / WeightMax)
	if weightAlpha <= 0 {
		return out
	}
	centerAlpha := clampU8(int64(weightAlpha))
	fillRect(out, rect.W, Rect{X: 0, Y: 0, W: rect.W, H: rect.H},
		Point{X: stroke.Start.X - rect.X, Y: stroke.Start.Y - rect.Y},
		Point{X: stroke.End.X - rect.X, Y: stroke.End.Y - rect.Y},
		stroke.Radius, stroke.Hardness, centerAlpha)
	return out
}

func fillRect(dst []uint8, stride int, clip Rect, a, b Point, radius, hardness int, weightAlpha uint8) {
	const scale int64 = 16
	const half int64 = 8
	ax := int64(a.X)*scale + half
	ay := int64(a.Y)*scale + half
	bx := int64(b.X)*scale + half
	by := int64(b.Y)*scale + half
	dx := bx - ax
	dy := by - ay
	len2 := dx*dx + dy*dy
	outer := int64(radius) * scale
	inner := outer * int64(hardness) / int64(PermilleMax)
	if inner > outer {
		inner = outer
	}
	outer2 := outer * outer
	for y := clip.Y; y < clip.Y+clip.H; y++ {
		py := int64(y)*scale + half
		for x := clip.X; x < clip.X+clip.W; x++ {
			px := int64(x)*scale + half
			wx := px - ax
			wy := py - ay

			var dist int64
			if len2 == 0 {
				dist = isqrt(wx*wx + wy*wy)
			} else {
				dot := wx*dx + wy*dy
				switch {
				case dot <= 0:
					dist = isqrt(wx*wx + wy*wy)
				case dot >= len2:
					ex := px - bx
					ey := py - by
					dist = isqrt(ex*ex + ey*ey)
				default:
					cross := dx*wy - dy*wx
					dist = isqrt(cross * cross / len2)
				}
			}
			if dist > outer {
				continue
			}
			alpha := weightAlpha
			if dist > inner {
				span := outer - inner
				if span <= 0 {
					continue
				}
				profile := (outer - dist) * 255 / span
				alpha = clampU8(int64(weightAlpha) * profile / 255)
			}
			if alpha == 0 {
				continue
			}
			idx := (y-clip.Y)*stride + (x - clip.X)
			if dst[idx] < alpha {
				dst[idx] = alpha
			}
		}
	}
	_ = outer2
}

func compositeMasks(base []uint8, components [][]uint8) []uint8 {
	out := append([]uint8(nil), base...)
	for _, component := range components {
		for i := range out {
			out[i] = alphaOver(out[i], component[i])
		}
	}
	return out
}

func applyComponent(base, component []uint8) {
	for i := range base {
		base[i] = alphaOver(base[i], component[i])
	}
}

package stitch

import "math"

type colorImage struct {
	w, h int
	pix  []float32
}

type weightImage struct {
	w, h int
	pix  []float32
}

func gaussianKernel(sigma float64) []float32 {
	if sigma < 0.4 {
		sigma = 0.4
	}
	r := int(math.Ceil(sigma * 3))
	if r < 1 {
		r = 1
	}
	k := make([]float32, 2*r+1)
	var sum float32
	for i := -r; i <= r; i++ {
		v := math.Exp(-float64(i*i) / (2 * sigma * sigma))
		k[i+r] = float32(v)
		sum += float32(v)
	}
	for i := range k {
		k[i] /= sum
	}
	return k
}

func downsampleColor(src *colorImage, sigma float64) *colorImage {
	k := gaussianKernel(sigma)
	r := len(k) / 2
	w, h := src.w/2, src.h/2
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	tmp := make([]float32, src.w*src.h*3)
	for y := 0; y < src.h; y++ {
		for x := 0; x < src.w; x++ {
			for c := 0; c < 3; c++ {
				var s float32
				for i := -r; i <= r; i++ {
					xx := x + i
					if xx < 0 {
						xx = 0
					}
					if xx >= src.w {
						xx = src.w - 1
					}
					s += src.pix[(y*src.w+xx)*3+c] * k[i+r]
				}
				tmp[(y*src.w+x)*3+c] = s
			}
		}
	}
	out := &colorImage{w: w, h: h, pix: make([]float32, w*h*3)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for c := 0; c < 3; c++ {
				var s float32
				for i := -r; i <= r; i++ {
					yy := y*2 + i
					if yy < 0 {
						yy = 0
					}
					if yy >= src.h {
						yy = src.h - 1
					}
					s += tmp[(yy*src.w+minInt(x*2, src.w-1))*3+c] * k[i+r]
				}
				out.pix[(y*w+x)*3+c] = s
			}
		}
	}
	return out
}

func downsampleWeight(src *weightImage, sigma float64) *weightImage {
	k := gaussianKernel(sigma)
	r := len(k) / 2
	tmp := make([]float32, src.w*src.h)
	for y := 0; y < src.h; y++ {
		for x := 0; x < src.w; x++ {
			var s float32
			for i := -r; i <= r; i++ {
				xx := minInt(maxInt(0, x+i), src.w-1)
				s += src.pix[y*src.w+xx] * k[i+r]
			}
			tmp[y*src.w+x] = s
		}
	}
	w, h := src.w/2, src.h/2
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	out := &weightImage{w: w, h: h, pix: make([]float32, w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var s float32
			for i := -r; i <= r; i++ {
				yy := minInt(maxInt(0, y*2+i), src.h-1)
				s += tmp[yy*src.w+minInt(x*2, src.w-1)] * k[i+r]
			}
			out.pix[y*w+x] = s
		}
	}
	return out
}

func upsampleColor(src *colorImage, w, h int) *colorImage {
	out := &colorImage{w: w, h: h, pix: make([]float32, w*h*3)}
	for y := 0; y < h; y++ {
		fy := (float64(y)+0.5)*float64(src.h)/float64(h) - 0.5
		for x := 0; x < w; x++ {
			fx := (float64(x)+0.5)*float64(src.w)/float64(w) - 0.5
			for c := 0; c < 3; c++ {
				out.pix[(y*w+x)*3+c] = float32(bilinearF(src.pix, src.w, src.h, fx, fy, c))
			}
		}
	}
	return out
}

func bilinearF(pix []float32, w, h int, x, y float64, c int) float64 {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x > float64(w-1) {
		x = float64(w - 1)
	}
	if y > float64(h-1) {
		y = float64(h - 1)
	}
	x0, y0 := int(x), int(y)
	x1, y1 := minInt(x0+1, w-1), minInt(y0+1, h-1)
	fx, fy := x-float64(x0), y-float64(y0)
	v0 := float64(pix[(y0*w+x0)*3+c])*(1-fx) + float64(pix[(y0*w+x1)*3+c])*fx
	v1 := float64(pix[(y1*w+x0)*3+c])*(1-fx) + float64(pix[(y1*w+x1)*3+c])*fx
	return v0*(1-fy) + v1*fy
}

func buildColorPyramid(img *colorImage, levels int, sigma float64) []*colorImage {
	out := []*colorImage{img}
	for len(out) < levels {
		prev := out[len(out)-1]
		if prev.w < 4 || prev.h < 4 {
			break
		}
		out = append(out, downsampleColor(prev, sigma))
	}
	return out
}

func buildWeightPyramid(img *weightImage, levels int, sigma float64) []*weightImage {
	out := []*weightImage{img}
	for len(out) < levels {
		prev := out[len(out)-1]
		if prev.w < 4 || prev.h < 4 {
			break
		}
		out = append(out, downsampleWeight(prev, sigma))
	}
	return out
}

func laplacianPyramid(gauss []*colorImage) []*colorImage {
	out := make([]*colorImage, len(gauss))
	for i := 0; i < len(gauss)-1; i++ {
		up := upsampleColor(gauss[i+1], gauss[i].w, gauss[i].h)
		lap := &colorImage{w: gauss[i].w, h: gauss[i].h, pix: make([]float32, len(gauss[i].pix))}
		for k := range lap.pix {
			lap.pix[k] = gauss[i].pix[k] - up.pix[k]
		}
		out[i] = lap
	}
	out[len(gauss)-1] = gauss[len(gauss)-1]
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

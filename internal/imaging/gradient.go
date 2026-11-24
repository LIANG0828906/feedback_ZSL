package imaging

import "math"

func GradientMagnitude(im *RGBImage, operator string) []float64 {
	gray := Luminance(im)
	w, h := im.Width, im.Height
	out := make([]float64, w*h)

	var kx [3][3]float64
	switch operator {
	case "scharr":
		kx = [3][3]float64{{-3, 0, 3}, {-10, 0, 10}, {-3, 0, 3}}
	default:
		kx = [3][3]float64{{-1, 0, 1}, {-2, 0, 2}, {-1, 0, 1}}
	}
	ky := transposeKernel(kx)
	scale := 4.0
	if operator == "scharr" {
		scale = 16.0
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var gx, gy float64
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					xx := clampInt(x+dx, 0, w-1)
					yy := clampInt(y+dy, 0, h-1)
					v := gray[yy*w+xx]
					gx += v * kx[dy+1][dx+1]
					gy += v * ky[dy+1][dx+1]
				}
			}
			mag := math.Hypot(gx, gy) / scale
			if mag > 255 {
				mag = 255
			}
			out[y*w+x] = mag
		}
	}
	return out
}

func transposeKernel(k [3][3]float64) [3][3]float64 {
	var out [3][3]float64
	for y := 0; y < 3; y++ {
		for x := 0; x < 3; x++ {
			out[y][x] = k[x][y]
		}
	}
	return out
}

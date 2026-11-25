package seg

import (
	"image"
	"math"

	"iconseg/internal/core"
)

type Gradient struct {
	W int
	H int
	V []float64
}

func (g Gradient) At(x, y int) float64 {
	return g.V[y*g.W+x]
}

func gaussianWeights(size int) []float64 {
	switch size {
	case 1:
		return []float64{1}
	case 3:
		return []float64{1, 2, 1}
	case 5:
		return []float64{1, 4, 6, 4, 1}
	default:
		return []float64{1}
	}
}

func smoothChannel(pix []float64, w, h, size int) []float64 {
	if size <= 1 {
		return pix
	}
	weights := gaussianWeights(size)
	radius := len(weights) / 2
	sum := 0.0
	for _, weight := range weights {
		sum += weight
	}
	tmp := make([]float64, len(pix))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			value := 0.0
			for k := -radius; k <= radius; k++ {
				xx := x + k
				if xx < 0 {
					xx = 0
				} else if xx >= w {
					xx = w - 1
				}
				value += pix[y*w+xx] * weights[k+radius]
			}
			tmp[y*w+x] = value / sum
		}
	}
	out := make([]float64, len(pix))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			value := 0.0
			for k := -radius; k <= radius; k++ {
				yy := y + k
				if yy < 0 {
					yy = 0
				} else if yy >= h {
					yy = h - 1
				}
				value += tmp[yy*w+x] * weights[k+radius]
			}
			out[y*w+x] = value / sum
		}
	}
	return out
}

func channelValues(img *image.NRGBA) [4][]float64 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var out [4][]float64
	for c := 0; c < 4; c++ {
		out[c] = make([]float64, w*h)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			c := img.NRGBAAt(b.Min.X+x, b.Min.Y+y)
			out[0][i] = float64(c.R)
			out[1][i] = float64(c.G)
			out[2][i] = float64(c.B)
			out[3][i] = float64(c.A)
		}
	}
	return out
}

// ComputeGradient applies a deterministic Gaussian smoothing kernel followed
// by the selected color gradient operator. All channels contribute equally.
func ComputeGradient(img *image.NRGBA, params core.SegmentParams) Gradient {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	channels := channelValues(img)
	for c := range channels {
		channels[c] = smoothChannel(channels[c], w, h, params.SmoothKernel)
	}

	gx := [3][3]float64{{-1, 0, 1}, {-2, 0, 2}, {-1, 0, 1}}
	gy := [3][3]float64{{-1, -2, -1}, {0, 0, 0}, {1, 2, 1}}
	if params.GradientOperator == "prewitt" {
		gx = [3][3]float64{{-1, 0, 1}, {-1, 0, 1}, {-1, 0, 1}}
		gy = [3][3]float64{{-1, -1, -1}, {0, 0, 0}, {1, 1, 1}}
	}

	out := Gradient{W: w, H: h, V: make([]float64, w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			mag2 := 0.0
			for c := 0; c < 3; c++ {
				dx, dy := 0.0, 0.0
				for ky := -1; ky <= 1; ky++ {
					for kx := -1; kx <= 1; kx++ {
						xx := x + kx
						yy := y + ky
						if xx < 0 {
							xx = 0
						} else if xx >= w {
							xx = w - 1
						}
						if yy < 0 {
							yy = 0
						} else if yy >= h {
							yy = h - 1
						}
						value := channels[c][yy*w+xx]
						dx += value * gx[ky+1][kx+1]
						dy += value * gy[ky+1][kx+1]
					}
				}
				mag2 += dx*dx + dy*dy
			}
			out.V[y*w+x] = math.Sqrt(mag2)
		}
	}
	return out
}

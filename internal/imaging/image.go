package imaging

import (
	"image"
	"image/draw"
	"math"
)

type RGBImage struct {
	Width  int
	Height int
	R      []float64
	G      []float64
	B      []float64
}

func NewRGBImage(w, h int) *RGBImage {
	return &RGBImage{Width: w, Height: h, R: make([]float64, w*h), G: make([]float64, w*h), B: make([]float64, w*h)}
}

func (im *RGBImage) At(x, y int) (float64, float64, float64) {
	i := y*im.Width + x
	return im.R[i], im.G[i], im.B[i]
}

func ToRGB(src image.Image) *RGBImage {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	out := NewRGBImage(w, h)
	nrgba := image.NewNRGBA(b)
	draw.Draw(nrgba, b, src, b.Min, draw.Src)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			c := nrgba.NRGBAAt(b.Min.X+x, b.Min.Y+y)
			out.R[i] = float64(c.R)
			out.G[i] = float64(c.G)
			out.B[i] = float64(c.B)
		}
	}
	return out
}

func ToNRGBA(im *RGBImage) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, im.Width, im.Height))
	for i := range im.R {
		out.Pix[i*4+0] = clampByte(im.R[i])
		out.Pix[i*4+1] = clampByte(im.G[i])
		out.Pix[i*4+2] = clampByte(im.B[i])
		out.Pix[i*4+3] = 255
	}
	return out
}

func clampByte(v float64) uint8 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(math.Round(v))
}

func Luminance(im *RGBImage) []float64 {
	out := make([]float64, im.Width*im.Height)
	for i := range im.R {
		out[i] = 0.2126*im.R[i] + 0.7152*im.G[i] + 0.0722*im.B[i]
	}
	return out
}

func BoxBlur(src []float64, w, h, radius int) []float64 {
	if radius <= 0 {
		dst := make([]float64, len(src))
		copy(dst, src)
		return dst
	}
	tmp := make([]float64, len(src))
	dst := make([]float64, len(src))
	window := radius*2 + 1
	for y := 0; y < h; y++ {
		row := y * w
		var sum float64
		for x := -radius; x <= radius; x++ {
			sum += src[row+clampInt(x, 0, w-1)]
		}
		for x := 0; x < w; x++ {
			tmp[row+x] = sum / float64(window)
			x0 := clampInt(x-radius, 0, w-1)
			x1 := clampInt(x+radius+1, 0, w-1)
			sum += src[row+x1] - src[row+x0]
		}
	}
	for x := 0; x < w; x++ {
		var sum float64
		for y := -radius; y <= radius; y++ {
			sum += tmp[clampInt(y, 0, h-1)*w+x]
		}
		for y := 0; y < h; y++ {
			dst[y*w+x] = sum / float64(window)
			y0 := clampInt(y-radius, 0, h-1)
			y1 := clampInt(y+radius+1, 0, h-1)
			sum += tmp[y1*w+x] - tmp[y0*w+x]
		}
	}
	return dst
}

func GaussianKernel(radius int) []float64 {
	if radius <= 0 {
		return []float64{1}
	}
	sigma := math.Max(0.8, float64(radius))
	k := make([]float64, 2*radius+1)
	var sum float64
	for i := -radius; i <= radius; i++ {
		v := math.Exp(-float64(i*i) / (2 * sigma * sigma))
		k[i+radius] = v
		sum += v
	}
	for i := range k {
		k[i] /= sum
	}
	return k
}

func GaussianBlurChannel(src []float64, w, h, kernelSize int) []float64 {
	if kernelSize <= 1 {
		dst := make([]float64, len(src))
		copy(dst, src)
		return dst
	}
	radius := kernelSize / 2
	k := GaussianKernel(radius)
	tmp := make([]float64, len(src))
	dst := make([]float64, len(src))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sum float64
			for o := -radius; o <= radius; o++ {
				xx := clampInt(x+o, 0, w-1)
				sum += src[y*w+xx] * k[o+radius]
			}
			tmp[y*w+x] = sum
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sum float64
			for o := -radius; o <= radius; o++ {
				yy := clampInt(y+o, 0, h-1)
				sum += tmp[yy*w+x] * k[o+radius]
			}
			dst[y*w+x] = sum
		}
	}
	return dst
}

func GaussianBlurRGB(im *RGBImage, kernelSize int) *RGBImage {
	out := NewRGBImage(im.Width, im.Height)
	out.R = GaussianBlurChannel(im.R, im.Width, im.Height, kernelSize)
	out.G = GaussianBlurChannel(im.G, im.Width, im.Height, kernelSize)
	out.B = GaussianBlurChannel(im.B, im.Width, im.Height, kernelSize)
	return out
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func DownsampleAverage(src *RGBImage, factor int) *RGBImage {
	if factor <= 1 {
		out := NewRGBImage(src.Width, src.Height)
		copy(out.R, src.R)
		copy(out.G, src.G)
		copy(out.B, src.B)
		return out
	}
	w := (src.Width + factor - 1) / factor
	h := (src.Height + factor - 1) / factor
	out := NewRGBImage(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sr, sg, sb, n float64
			for yy := y * factor; yy < minInt(src.Height, (y+1)*factor); yy++ {
				for xx := x * factor; xx < minInt(src.Width, (x+1)*factor); xx++ {
					i := yy*src.Width + xx
					sr += src.R[i]
					sg += src.G[i]
					sb += src.B[i]
					n++
				}
			}
			o := y*w + x
			out.R[o] = sr / n
			out.G[o] = sg / n
			out.B[o] = sb / n
		}
	}
	return out
}

func UpsampleNearestMask(small []byte, sw, sh, w, h, factor int) []byte {
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		sy := minInt(y/factor, sh-1)
		for x := 0; x < w; x++ {
			sx := minInt(x/factor, sw-1)
			out[y*w+x] = small[sy*sw+sx]
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

package viz

import (
	"image"
	"image/color"
	"image/png"
	"io"
	"math"

	"edgerefine/internal/model"
	"edgerefine/internal/pipeline"
)

func writeGray(w io.Writer, pix []byte, width, height int) error {
	img := image.NewGray(image.Rect(0, 0, width, height))
	for i := 0; i < width*height && i < len(pix); i++ {
		img.Pix[i] = pix[i]
	}
	return png.Encode(w, img)
}

func Mask(w io.Writer, mask []byte, width, height int) error {
	return writeGray(w, mask, width, height)
}

func Heatmap(w io.Writer, values []float64, width, height int) error {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	maxV := 1.0
	for _, v := range values {
		if v > maxV {
			maxV = v
		}
	}
	for i := 0; i < width*height && i < len(values); i++ {
		t := math.Max(0, math.Min(1, values[i]/maxV))
		img.Pix[i*4+0] = byte(255 * t)
		img.Pix[i*4+1] = byte(70 + 120*(1-t))
		img.Pix[i*4+2] = byte(80 * (1 - t))
		img.Pix[i*4+3] = 255
	}
	return png.Encode(w, img)
}

func Overlay(w io.Writer, gray *model.GrayImage, mask []byte, width, height int) error {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := y*width + x
			g := gray.Pix[i]
			img.SetRGBA(x, y, color.RGBA{R: g, G: g, B: g, A: 255})
			a := mask[i]
			if a > 0 {
				old := img.RGBAAt(x, y)
				alpha := float64(a) / 255 * 0.42
				img.SetRGBA(x, y, color.RGBA{
					R: uint8(float64(old.R)*(1-alpha) + 40*alpha),
					G: uint8(float64(old.G)*(1-alpha) + 220*alpha),
					B: uint8(float64(old.B)*(1-alpha) + 255*alpha),
					A: 255,
				})
			}
		}
	}
	for _, p := range pipeline.ExtractGlobalPoints(mask, width, height) {
		x := int(math.Round(p.X))
		y := int(math.Round(p.Y))
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				xx, yy := x+dx, y+dy
				if xx >= 0 && xx < width && yy >= 0 && yy < height && (dx == 0 || dy == 0) {
					img.SetRGBA(xx, yy, color.RGBA{R: 255, G: 60, B: 40, A: 255})
				}
			}
		}
	}
	return png.Encode(w, img)
}

func Difference(w io.Writer, a, b []byte, width, height int) error {
	diff := make([]float64, width*height)
	for i := 0; i < width*height; i++ {
		var av, bv byte
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		diff[i] = math.Abs(float64(av) - float64(bv))
	}
	return Heatmap(w, diff, width, height)
}

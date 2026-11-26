package maskqa

import (
	"image"
	"image/color"
	"image/png"
	"io"
)

func WriteRasterPNG(w io.Writer, r *Raster) error {
	img := image.NewRGBA(image.Rect(0, 0, r.Width, r.Height))
	copy(img.Pix, r.RGBA)
	return png.Encode(w, img)
}

func WriteMaskPNG(w io.Writer, m *Mask, foreground color.Color) error {
	img := image.NewRGBA(image.Rect(0, 0, m.Width, m.Height))
	bg := color.RGBA{R: 15, G: 23, B: 42, A: 255}
	fg := color.RGBA{R: 226, G: 232, B: 240, A: 255}
	if foreground != nil {
		if rgba, ok := foreground.(color.RGBA); ok {
			fg = rgba
		}
	}
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			if m.At(x, y) != 0 {
				img.Set(x, y, fg)
			} else {
				img.Set(x, y, bg)
			}
		}
	}
	return png.Encode(w, img)
}

func WriteDiffPNG(w io.Writer, got, want *Mask) error {
	img := image.NewRGBA(image.Rect(0, 0, got.Width, got.Height))
	for y := 0; y < got.Height; y++ {
		for x := 0; x < got.Width; x++ {
			g := got.At(x, y) != 0
			wt := want.At(x, y) != 0
			switch {
			case g && wt:
				img.Set(x, y, color.RGBA{R: 34, G: 197, B: 94, A: 255})
			case g && !wt:
				img.Set(x, y, color.RGBA{R: 239, G: 68, B: 68, A: 255})
			case !g && wt:
				img.Set(x, y, color.RGBA{R: 245, G: 158, B: 11, A: 255})
			default:
				img.Set(x, y, color.RGBA{R: 15, G: 23, B: 42, A: 255})
			}
		}
	}
	return png.Encode(w, img)
}

package imageout

import (
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"

	"volumekernel/internal/model"
)

func WriteFramePNG(path string, frame model.Frame, scale int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return EncodeFramePNG(f, frame, scale)
}

func EncodeFramePNG(w io.Writer, frame model.Frame, scale int) error {
	if frame.Viewport.Width <= 0 || frame.Viewport.Height <= 0 {
		return model.ParamError("", "viewport", "cannot render an empty frame")
	}
	if scale <= 0 {
		scale = 1
	}
	img := image.NewRGBA(image.Rect(0, 0, frame.Viewport.Width*scale, frame.Viewport.Height*scale))
	maxScatter := 0.0
	for _, p := range frame.Pixels {
		if p.ScatteredRadiance > maxScatter {
			maxScatter = p.ScatteredRadiance
		}
	}
	if maxScatter <= 0 {
		maxScatter = 1
	}
	for _, p := range frame.Pixels {
		v := p.ScatteredRadiance / maxScatter
		v = v / (1 + v)
		v = math.Pow(v, 0.65)
		t := 1 - p.Transmittance
		if t < 0 {
			t = 0
		}
		if t > 1 {
			t = 1
		}
		c := color.RGBA{
			R: uint8(clamp255(40 + 215*v)),
			G: uint8(clamp255(20 + 170*t + 60*v)),
			B: uint8(clamp255(55 + 120*t + 30*v)),
			A: 255,
		}
		x0, y0 := p.X*scale, p.Y*scale
		for yy := 0; yy < scale; yy++ {
			for xx := 0; xx < scale; xx++ {
				img.SetRGBA(x0+xx, y0+yy, c)
			}
		}
	}
	return png.Encode(w, img)
}

func clamp255(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

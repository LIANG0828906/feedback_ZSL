package yuv

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

func CompositePNG(c *Composite) *image.RGBA {
	if c == nil {
		return image.NewRGBA(image.Rect(0, 0, 0, 0))
	}
	out := image.NewRGBA(image.Rect(0, 0, c.Width, c.Height))
	for y := 0; y < c.Height; y++ {
		for x := 0; x < c.Width; x++ {
			index := y*c.Width + x
			r, g, b := yuvToRGB(c, c.Y[index], c.U[index], c.V[index])
			out.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return out
}

func OriginalPNG(img *Image) (*image.RGBA, error) {
	composite, err := Upsample420(img.Meta, img.Y, img.U, img.V)
	if err != nil {
		return nil, err
	}
	return CompositePNG(composite), nil
}

func WritePNG(path string, rgba *image.RGBA) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return png.Encode(file, rgba)
}

func yuvToRGB(c *Composite, y, u, v uint16) (uint8, uint8, uint8) {
	maxCode := float64(uint32(1)<<uint(c.BitDepth) - 1)
	var yy, uu, vv float64
	if c.QuantRange == "video" {
		scale := math.Pow(2, float64(c.BitDepth-8))
		yy = (float64(y) - 16*scale) * 255.0 / (219.0 * scale)
		uu = (float64(u) - 128*scale) * 255.0 / (224.0 * scale)
		vv = (float64(v) - 128*scale) * 255.0 / (224.0 * scale)
	} else {
		yy = float64(y) * 255.0 / maxCode
		uu = float64(u) * 255.0 / maxCode
		vv = float64(v) * 255.0 / maxCode
	}
	cb := uu - 128.0
	cr := vv - 128.0
	r := yy + 1.402*cr
	g := yy - 0.344136*cb - 0.714136*cr
	b := yy + 1.772*cb
	return clampByte(r), clampByte(g), clampByte(b)
}

func clampByte(value float64) uint8 {
	if math.IsNaN(value) || value <= 0 {
		return 0
	}
	if value >= 255 {
		return 255
	}
	return uint8(math.Round(value))
}

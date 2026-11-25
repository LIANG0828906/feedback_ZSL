package iconcut

import (
	"image"
	"image/color"
	"image/png"
	"os"
)

func ReadPNG(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, NewError(EDecode, path, "open png: %v", err)
	}
	defer f.Close()
	im, err := png.Decode(f)
	if err != nil {
		return nil, NewError(EDecode, path, "decode png: %v", err)
	}
	b := im.Bounds()
	out := NewImage(b.Dx(), b.Dy(), 3)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := im.At(x, y).RGBA()
			out.SetPixel(x-b.Min.X, y-b.Min.Y, [3]byte{byte(r >> 8), byte(g >> 8), byte(bl >> 8)})
		}
	}
	return out, nil
}

func WritePNG(path string, im *Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	rgba := image.NewRGBA(image.Rect(0, 0, im.W, im.H))
	for y := 0; y < im.H; y++ {
		for x := 0; x < im.W; x++ {
			c := im.Pixel(x, y)
			rgba.SetRGBA(x, y, color.RGBA{R: c[0], G: c[1], B: c[2], A: 255})
		}
	}
	return png.Encode(f, rgba)
}

func ReadLabelPNG(path string) (*LabelMask, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, NewError(EDecode, path, "open label png: %v", err)
	}
	defer f.Close()
	im, err := png.Decode(f)
	if err != nil {
		return nil, NewError(EDecode, path, "decode label png: %v", err)
	}
	b := im.Bounds()
	out := NewLabelMask(b.Dx(), b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			g := color.GrayModel.Convert(im.At(x, y)).(color.Gray)
			out.Set(x-b.Min.X, y-b.Min.Y, g.Y)
		}
	}
	return out, nil
}

func WriteLabelPNG(path string, m *LabelMask) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	g := image.NewGray(image.Rect(0, 0, m.W, m.H))
	copy(g.Pix, m.Labels)
	return png.Encode(f, g)
}

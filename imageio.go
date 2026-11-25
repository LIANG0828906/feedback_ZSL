package maskqc

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

func WriteMaskPNG(path string, m *Mask) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	img := image.NewGray(image.Rect(0, 0, m.Width, m.Height))
	for i, a := range m.Alpha {
		img.Pix[i] = a
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func EncodeMaskPNG(m *Mask) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, m.Width, m.Height))
	copy(img.Pix, m.Alpha)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func EncodeImagePNG(img *Image) ([]byte, error) {
	rgba := image.NewRGBA(image.Rect(0, 0, img.Width, img.Height))
	copy(rgba.Pix, img.RGBA)
	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ReadMaskPNG(path string) (*Mask, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	out := NewMask(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, a := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			var ga byte
			if a == 0 {
				ga = 0
			} else {
				ga = byte((r + g + b) / 3 >> 8)
			}
			out.Set(x, y, ga)
		}
	}
	return out, nil
}

func WriteImagePNG(path string, img *Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	rgba := image.NewRGBA(image.Rect(0, 0, img.Width, img.Height))
	copy(rgba.Pix, img.RGBA)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, rgba)
}

func ReadImagePNG(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	out := &Image{Width: w, Height: h, RGBA: make([]byte, w*h*4)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, a := color.RGBAModel.Convert(img.At(bounds.Min.X+x, bounds.Min.Y+y)).RGBA()
			i := (y*w + x) * 4
			out.RGBA[i] = byte(r >> 8)
			out.RGBA[i+1] = byte(g >> 8)
			out.RGBA[i+2] = byte(b >> 8)
			out.RGBA[i+3] = byte(a >> 8)
		}
	}
	return out, nil
}

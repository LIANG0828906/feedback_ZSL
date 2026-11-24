package main

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
)

func writeMaskPNG(path string, mask []byte, w, h int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i, v := range mask {
		if v != 0 {
			img.Pix[i] = 255
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func writeOverlayPNG(path string, base *image.NRGBA, mask []byte, w, h int) error {
	overlay := image.NewNRGBA(base.Bounds())
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			src := base.PixOffset(x, y)
			r, g, b := base.Pix[src], base.Pix[src+1], base.Pix[src+2]
			if mask[i] != 0 {
				r = uint8((int(r) + 255) / 2)
				g = uint8(int(g) / 2)
				b = uint8(int(b) / 2)
			}
			off := overlay.PixOffset(x, y)
			overlay.Pix[off] = r
			overlay.Pix[off+1] = g
			overlay.Pix[off+2] = b
			overlay.Pix[off+3] = 220
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, overlay)
}

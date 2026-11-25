package ocrflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
)

// GrayImage is the internal integer grayscale buffer.
type GrayImage struct {
	Width  int
	Height int
	Pix    []byte
}

func NewGrayImage(w, h int) *GrayImage {
	return &GrayImage{Width: w, Height: h, Pix: make([]byte, w*h)}
}

func (g *GrayImage) Rect() Rect { return Rect{MinX: 0, MinY: 0, MaxX: g.Width, MaxY: g.Height} }

func (g *GrayImage) At(x, y int) byte {
	if x < 0 || y < 0 || x >= g.Width || y >= g.Height {
		return 0
	}
	return g.Pix[y*g.Width+x]
}

func (g *GrayImage) Set(x, y int, v byte) {
	if x < 0 || y < 0 || x >= g.Width || y >= g.Height {
		return
	}
	g.Pix[y*g.Width+x] = v
}

func (g *GrayImage) Crop(r Rect) *GrayImage {
	r = rectIntersection(r, g.Rect())
	out := NewGrayImage(r.Width(), r.Height())
	for y := r.MinY; y < r.MaxY; y++ {
		for x := r.MinX; x < r.MaxX; x++ {
			out.Pix[(y-r.MinY)*out.Width+(x-r.MinX)] = g.At(x, y)
		}
	}
	return out
}

// Mask is a binary pixel candidate mask.
type Mask struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Pix    []byte `json:"-"`
}

func NewMask(w, h int) *Mask { return &Mask{Width: w, Height: h, Pix: make([]byte, w*h)} }

func (m *Mask) At(x, y int) byte {
	if x < 0 || y < 0 || x >= m.Width || y >= m.Height {
		return 0
	}
	return m.Pix[y*m.Width+x]
}

func (m *Mask) Set(x, y int, v byte) {
	if x < 0 || y < 0 || x >= m.Width || y >= m.Height {
		return
	}
	m.Pix[y*m.Width+x] = v
}

func (m *Mask) Count() int {
	n := 0
	for _, v := range m.Pix {
		if v != 0 {
			n++
		}
	}
	return n
}

func HashRGBA(img *image.RGBA) string {
	h := sha256.New()
	_, _ = h.Write([]byte("rgba-v1\n"))
	var dims [16]byte
	dims[0] = byte(img.Rect.Dx() >> 24)
	dims[1] = byte(img.Rect.Dx() >> 16)
	dims[2] = byte(img.Rect.Dx() >> 8)
	dims[3] = byte(img.Rect.Dx())
	dims[4] = byte(img.Rect.Dy() >> 24)
	dims[5] = byte(img.Rect.Dy() >> 16)
	dims[6] = byte(img.Rect.Dy() >> 8)
	dims[7] = byte(img.Rect.Dy())
	_, _ = h.Write(dims[:8])
	_, _ = h.Write(img.Pix)
	return hex.EncodeToString(h.Sum(nil))
}

func EncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func DecodePNG(data []byte) (image.Image, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, decodeError("screenshot.png", err, "PNG decode failed: %v", err)
	}
	return img, nil
}

package core

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
)

// Image is a tightly packed, interleaved single- or three-channel raster.
// Pixel values are unsigned bytes; channels are ordered R, G, B.
type Image struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Channels int    `json:"channels"`
	Pix      []byte `json:"-"`
}

func NewImage(width, height, channels int) Image {
	return Image{Width: width, Height: height, Channels: channels, Pix: make([]byte, width*height*channels)}
}

func (im Image) PixelCount() int { return im.Width * im.Height * im.Channels }

func (im Image) At(x, y, c int) byte {
	if im.Channels == 1 {
		c = 0
	}
	return im.Pix[(y*im.Width+x)*im.Channels+c]
}

func (im Image) Set(x, y, c int, v byte) {
	if im.Channels == 1 {
		c = 0
	}
	im.Pix[(y*im.Width+x)*im.Channels+c] = v
}

func (im Image) Clone() Image {
	out := Image{Width: im.Width, Height: im.Height, Channels: im.Channels}
	out.Pix = append([]byte(nil), im.Pix...)
	return out
}

func (im Image) Validate() error {
	if im.Width <= 0 {
		return NewError(CodeMeta, "width", "image width must be positive")
	}
	if im.Height <= 0 {
		return NewError(CodeMeta, "height", "image height must be positive")
	}
	if im.Channels != 1 && im.Channels != 3 {
		return NewError(CodeMeta, "channels", "image channels must be 1 or 3")
	}
	if len(im.Pix) != im.PixelCount() {
		return NewError(CodeMeta, "pixels", fmt.Sprintf("pixel buffer length %d does not match %d", len(im.Pix), im.PixelCount()))
	}
	return nil
}

func FingerprintImage(im Image) string {
	h := sha256.New()
	var header [12]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(im.Width))
	binary.LittleEndian.PutUint32(header[4:8], uint32(im.Height))
	binary.LittleEndian.PutUint32(header[8:12], uint32(im.Channels))
	_, _ = h.Write(header[:])
	_, _ = h.Write(im.Pix)
	return hex.EncodeToString(h.Sum(nil))
}

func FingerprintBytes(width, height, channels int, pix []byte) string {
	return FingerprintImage(Image{Width: width, Height: height, Channels: channels, Pix: pix})
}

func DecodePNG(r io.Reader) (Image, error) {
	src, err := png.Decode(r)
	if err != nil {
		return Image{}, WrapError(CodeDecode, "", "PNG decode failed", err)
	}
	return FromGoImage(src), nil
}

func DecodePNGFile(path string) (Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return Image{}, WrapError(CodeDecode, "", "cannot open image", err)
	}
	defer f.Close()
	return DecodePNG(f)
}

func FromGoImage(src image.Image) Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	switch concrete := src.(type) {
	case *image.Gray:
		out := NewImage(w, h, 1)
		for y := 0; y < h; y++ {
			copy(out.Pix[y*w:(y+1)*w], concrete.Pix[y*concrete.Stride:y*concrete.Stride+w])
		}
		return out
	case *image.Gray16:
		out := NewImage(w, h, 1)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Pix[y*w+x] = uint8(concrete.Gray16At(x+b.Min.X, y+b.Min.Y).Y >> 8)
			}
		}
		return out
	}

	out := NewImage(w, h, 3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := src.At(x+b.Min.X, y+b.Min.Y).RGBA()
			i := (y*w + x) * 3
			out.Pix[i] = uint8(r >> 8)
			out.Pix[i+1] = uint8(g >> 8)
			out.Pix[i+2] = uint8(bl >> 8)
		}
	}
	return out
}

func ToGoImage(im Image) image.Image {
	if im.Channels == 1 {
		dst := image.NewGray(image.Rect(0, 0, im.Width, im.Height))
		copy(dst.Pix, im.Pix)
		return dst
	}
	dst := image.NewNRGBA(image.Rect(0, 0, im.Width, im.Height))
	for y := 0; y < im.Height; y++ {
		for x := 0; x < im.Width; x++ {
			i := (y*im.Width + x) * 3
			dst.SetNRGBA(x, y, color.NRGBA{R: im.Pix[i], G: im.Pix[i+1], B: im.Pix[i+2], A: 255})
		}
	}
	return dst
}

func EncodePNG(w io.Writer, im Image) error {
	return png.Encode(w, ToGoImage(im))
}

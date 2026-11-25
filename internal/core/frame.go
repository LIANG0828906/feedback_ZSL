package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

type Frame struct {
	Width    int
	Height   int
	Channels int
	Pix      []byte
}

func NewFrame(width, height, channels int) (*Frame, error) {
	if width <= 0 || height <= 0 || channels <= 0 {
		return nil, fmt.Errorf("invalid frame geometry %dx%dx%d", width, height, channels)
	}
	n := width * height * channels
	if n <= 0 || n > 1<<30 {
		return nil, fmt.Errorf("frame buffer too large: %d bytes", n)
	}
	return &Frame{Width: width, Height: height, Channels: channels, Pix: make([]byte, n)}, nil
}

func (f *Frame) Clone() *Frame {
	if f == nil {
		return nil
	}
	out := &Frame{Width: f.Width, Height: f.Height, Channels: f.Channels, Pix: make([]byte, len(f.Pix))}
	copy(out.Pix, f.Pix)
	return out
}

func (f *Frame) Validate() error {
	if f == nil {
		return fmt.Errorf("nil frame")
	}
	if f.Width <= 0 || f.Height <= 0 || f.Channels <= 0 {
		return fmt.Errorf("invalid geometry %dx%dx%d", f.Width, f.Height, f.Channels)
	}
	want := f.Width * f.Height * f.Channels
	if len(f.Pix) != want {
		return fmt.Errorf("pixel buffer has %d bytes, want %d", len(f.Pix), want)
	}
	return nil
}

func (f *Frame) Offset(x, y int) int {
	return (y*f.Width + x) * f.Channels
}

func (f *Frame) At(x, y int) [3]uint8 {
	var out [3]uint8
	if f == nil || x < 0 || y < 0 || x >= f.Width || y >= f.Height {
		return out
	}
	o := f.Offset(x, y)
	n := min(3, f.Channels)
	copy(out[:n], f.Pix[o:o+n])
	return out
}

func (f *Frame) Set(x, y int, c [3]uint8) {
	if f == nil || x < 0 || y < 0 || x >= f.Width || y >= f.Height {
		return
	}
	o := f.Offset(x, y)
	n := min(3, f.Channels)
	copy(f.Pix[o:o+n], c[:n])
}

func (f *Frame) Block(rect Rect) (*Frame, error) {
	if f == nil {
		return nil, fmt.Errorf("nil frame")
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if !rect.InFrame(f.Width, f.Height) {
		return nil, fmt.Errorf("block %s outside %dx%d", rect, f.Width, f.Height)
	}
	out, err := NewFrame(rect.W, rect.H, f.Channels)
	if err != nil {
		return nil, err
	}
	for y := 0; y < rect.H; y++ {
		src := f.Offset(rect.X, rect.Y+y)
		dst := out.Offset(0, y)
		copy(out.Pix[dst:dst+rect.W*f.Channels], f.Pix[src:src+rect.W*f.Channels])
	}
	return out, nil
}

func (f *Frame) SetBlock(rect Rect, src *Frame) error {
	if f == nil || src == nil {
		return fmt.Errorf("nil frame")
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if err := src.Validate(); err != nil {
		return err
	}
	if !rect.InFrame(f.Width, f.Height) {
		return fmt.Errorf("block %s outside %dx%d", rect, f.Width, f.Height)
	}
	if src.Width != rect.W || src.Height != rect.H || src.Channels != f.Channels {
		return fmt.Errorf("source frame %dx%dx%d does not match block %s/%d channels",
			src.Width, src.Height, src.Channels, rect, f.Channels)
	}
	for y := 0; y < rect.H; y++ {
		dst := f.Offset(rect.X, rect.Y+y)
		srcOff := src.Offset(0, y)
		copy(f.Pix[dst:dst+rect.W*f.Channels], src.Pix[srcOff:srcOff+rect.W*f.Channels])
	}
	return nil
}

func (f *Frame) BlockBytes(rect Rect) []byte {
	if f == nil || !rect.InFrame(f.Width, f.Height) {
		return nil
	}
	out := make([]byte, 0, rect.W*rect.H*f.Channels)
	stride := rect.W * f.Channels
	for y := 0; y < rect.H; y++ {
		o := f.Offset(rect.X, rect.Y+y)
		out = append(out, f.Pix[o:o+stride]...)
	}
	return out
}

func (f *Frame) Hash() string {
	if f == nil {
		return "nil"
	}
	var header [16]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(f.Width))
	binary.LittleEndian.PutUint32(header[4:8], uint32(f.Height))
	binary.LittleEndian.PutUint32(header[8:12], uint32(f.Channels))
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(f.Pix)))
	h := sha256.New()
	_, _ = h.Write(header[:])
	_, _ = h.Write(f.Pix)
	return hex.EncodeToString(h.Sum(nil))
}

func HashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(len(p)))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func HashFrameRegion(f *Frame, rect Rect) string {
	if f == nil {
		return "nil"
	}
	rect = rect.Clamp(f.Width, f.Height)
	if rect.Empty() {
		return "empty"
	}
	b := f.BlockBytes(rect)
	return HashParts("region", fmt.Sprintf("%d,%d,%d,%d", rect.X, rect.Y, rect.W, rect.H),
		fmt.Sprintf("%d,%d,%d", f.Width, f.Height, f.Channels), string(b))
}

func (f *Frame) EncodePNG() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	img := image.NewNRGBA(image.Rect(0, 0, f.Width, f.Height))
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			c := f.At(x, y)
			img.SetNRGBA(x, y, color.NRGBA{R: c[0], G: c[1], B: c[2], A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (f *Frame) WritePNG(path string) error {
	data, err := f.EncodePNG()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func DecodePNG(path string) (*Frame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	out, err := NewFrame(b.Dx(), b.Dy(), 3)
	if err != nil {
		return nil, err
	}
	for y := 0; y < out.Height; y++ {
		for x := 0; x < out.Width; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			out.Set(x, y, [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8)})
		}
	}
	return out, nil
}

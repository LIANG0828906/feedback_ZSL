package imagex

import (
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"

	"qkernel/internal/errs"
)

type Rect struct {
	MinX int `json:"min_x"`
	MinY int `json:"min_y"`
	MaxX int `json:"max_x"`
	MaxY int `json:"max_y"`
}

func R(x0, y0, x1, y1 int) Rect { return Rect{MinX: x0, MinY: y0, MaxX: x1, MaxY: y1} }

func (r Rect) Dx() int { return r.MaxX - r.MinX }
func (r Rect) Dy() int { return r.MaxY - r.MinY }

func (r Rect) Empty() bool { return r.MaxX <= r.MinX || r.MaxY <= r.MinY }

func (r Rect) Canonical() Rect {
	if r.MinX > r.MaxX {
		r.MinX, r.MaxX = r.MaxX, r.MinX
	}
	if r.MinY > r.MaxY {
		r.MinY, r.MaxY = r.MaxY, r.MinY
	}
	return r
}

func (r Rect) Contains(x, y int) bool {
	return x >= r.MinX && x < r.MaxX && y >= r.MinY && y < r.MaxY
}

func (r Rect) ContainsRect(o Rect) bool {
	return r.MinX <= o.MinX && r.MinY <= o.MinY && r.MaxX >= o.MaxX && r.MaxY >= o.MaxY
}

func (r Rect) Intersect(o Rect) Rect {
	x0 := maxInt(r.MinX, o.MinX)
	y0 := maxInt(r.MinY, o.MinY)
	x1 := minInt(r.MaxX, o.MaxX)
	y1 := minInt(r.MaxY, o.MaxY)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return R(x0, y0, x1, y1)
}

func (r Rect) Intersects(o Rect) bool {
	return !r.Intersect(o).Empty()
}

func (r Rect) Expand(n int, bounds Rect) Rect {
	out := R(r.MinX-n, r.MinY-n, r.MaxX+n, r.MaxY+n)
	if bounds.Empty() {
		return out
	}
	return out.Intersect(bounds)
}

func (r Rect) Offset(x, y int) Rect {
	return R(r.MinX+x, r.MinY+y, r.MaxX+x, r.MaxY+y)
}

func (r Rect) Clamp(bounds Rect) Rect { return r.Intersect(bounds) }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type Frame struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Channels int    `json:"channels"`
	Pix      []byte `json:"-"`
}

func NewFrame(w, h, ch int) *Frame {
	if w <= 0 || h <= 0 || (ch != 3 && ch != 4) {
		panic(errs.New(errs.EBuild, "invalid frame geometry %dx%d ch=%d", w, h, ch))
	}
	return &Frame{Width: w, Height: h, Channels: ch, Pix: make([]byte, w*h*ch)}
}

func (f *Frame) Bounds() Rect { return R(0, 0, f.Width, f.Height) }

func (f *Frame) Offset(x, y int) int { return (y*f.Width + x) * f.Channels }

func (f *Frame) ValidXY(x, y int) bool { return x >= 0 && x < f.Width && y >= 0 && y < f.Height }

func (f *Frame) ClampXY(x, y int) (int, int) {
	if x < 0 {
		x = 0
	} else if x >= f.Width {
		x = f.Width - 1
	}
	if y < 0 {
		y = 0
	} else if y >= f.Height {
		y = f.Height - 1
	}
	return x, y
}

func (f *Frame) Pixel(x, y int) []byte {
	i := f.Offset(x, y)
	return f.Pix[i : i+f.Channels : i+f.Channels]
}

func (f *Frame) SetRGB(x, y int, r, g, b byte) {
	if !f.ValidXY(x, y) {
		return
	}
	i := f.Offset(x, y)
	f.Pix[i] = r
	f.Pix[i+1] = g
	f.Pix[i+2] = b
	if f.Channels == 4 {
		f.Pix[i+3] = 255
	}
}

func (f *Frame) RGBAtClamped(x, y int) (byte, byte, byte) {
	x, y = f.ClampXY(x, y)
	i := f.Offset(x, y)
	return f.Pix[i], f.Pix[i+1], f.Pix[i+2]
}

func (f *Frame) Clone() *Frame {
	pix := make([]byte, len(f.Pix))
	copy(pix, f.Pix)
	return &Frame{Width: f.Width, Height: f.Height, Channels: f.Channels, Pix: pix}
}

func (f *Frame) Hash() string {
	sum := sha256.Sum256(f.Pix)
	return hex.EncodeToString(sum[:])
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (f *Frame) RectHash(r Rect) string {
	r = r.Intersect(f.Bounds())
	h := sha256.New()
	_, _ = h.Write([]byte{byte(r.MinX >> 24), byte(r.MinX >> 16), byte(r.MinX >> 8), byte(r.MinX)})
	_, _ = h.Write([]byte{byte(r.MinY >> 24), byte(r.MinY >> 16), byte(r.MinY >> 8), byte(r.MinY)})
	_, _ = h.Write([]byte{byte(r.MaxX >> 24), byte(r.MaxX >> 16), byte(r.MaxX >> 8), byte(r.MaxX)})
	_, _ = h.Write([]byte{byte(r.MaxY >> 24), byte(r.MaxY >> 16), byte(r.MaxY >> 8), byte(r.MaxY)})
	rowBytes := r.Dx() * f.Channels
	for y := r.MinY; y < r.MaxY; y++ {
		start := f.Offset(r.MinX, y)
		_, _ = h.Write(f.Pix[start : start+rowBytes])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (f *Frame) CopyRectTo(r Rect, dst []byte) {
	r = r.Intersect(f.Bounds())
	rowBytes := r.Dx() * f.Channels
	for y := r.MinY; y < r.MaxY; y++ {
		s := f.Offset(r.MinX, y)
		d := (y - r.MinY) * r.Dx() * f.Channels
		copy(dst[d:d+rowBytes], f.Pix[s:s+rowBytes])
	}
}

func (f *Frame) CopyRectFrom(r Rect, src []byte) {
	r = r.Intersect(f.Bounds())
	rowBytes := r.Dx() * f.Channels
	for y := r.MinY; y < r.MaxY; y++ {
		d := f.Offset(r.MinX, y)
		s := (y - r.MinY) * r.Dx() * f.Channels
		copy(f.Pix[d:d+rowBytes], src[s:s+rowBytes])
	}
}

func (f *Frame) ToNRGBA() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, f.Width, f.Height))
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			i := f.Offset(x, y)
			oi := img.PixOffset(x, y)
			img.Pix[oi] = f.Pix[i]
			img.Pix[oi+1] = f.Pix[i+1]
			img.Pix[oi+2] = f.Pix[i+2]
			if f.Channels == 4 {
				img.Pix[oi+3] = f.Pix[i+3]
			} else {
				img.Pix[oi+3] = 255
			}
		}
	}
	return img
}

func SavePNG(f *Frame, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errs.Wrap(errs.EIO, err, "create image directory")
	}
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return errs.Wrap(errs.EIO, err, "create png")
	}
	encErr := png.Encode(file, f.ToNRGBA())
	closeErr := file.Close()
	if encErr != nil {
		return errs.Wrap(errs.EIO, encErr, "encode png")
	}
	if closeErr != nil {
		return errs.Wrap(errs.EIO, closeErr, "close png")
	}
	if err := os.Rename(tmp, path); err != nil {
		return errs.Wrap(errs.EIO, err, "atomic rename png")
	}
	return nil
}

func LoadPNG(path string) (*Frame, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "open png")
	}
	defer file.Close()
	img, err := png.Decode(file)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "decode png")
	}
	bounds := img.Bounds()
	f := NewFrame(bounds.Dx(), bounds.Dy(), 3)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			f.SetRGB(x-bounds.Min.X, y-bounds.Min.Y, byte(r>>8), byte(g>>8), byte(bl>>8))
		}
	}
	return f, nil
}

func ByteColor(c color.Color) (byte, byte, byte) {
	r, g, b, _ := c.RGBA()
	return byte(r >> 8), byte(g >> 8), byte(b >> 8)
}

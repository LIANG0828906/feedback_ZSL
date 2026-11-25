package textureconv

import (
	"fmt"
	"sort"
)

const (
	SchemaVersion    = 1
	AlgorithmVersion = "textureconv-v1"
	DefaultTileSize  = 64

	DefaultWorkers              = 1
	DefaultBufferBytesPerThread = 8 << 20
)

// BitDepth is the native component width of an image.
type BitDepth uint8

const (
	Depth8  BitDepth = 8
	Depth16 BitDepth = 16
)

func (d BitDepth) Valid() bool { return d == Depth8 || d == Depth16 }

// Max returns the largest component value at this depth.
func (d BitDepth) Max() uint16 {
	switch d {
	case Depth8:
		return 255
	case Depth16:
		return 65535
	default:
		return 0
	}
}

// Layout is the packed source channel order.
type Layout string

const (
	LayoutRGBA Layout = "RGBA"
	LayoutBGRA Layout = "BGRA"
)

func (l Layout) Valid() bool { return l == LayoutRGBA || l == LayoutBGRA }

func (l Layout) Code() uint16 {
	if l == LayoutBGRA {
		return 2
	}
	return 1
}

func layoutFromCode(c uint16) (Layout, bool) {
	switch c {
	case 1:
		return LayoutRGBA, true
	case 2:
		return LayoutBGRA, true
	default:
		return "", false
	}
}

// Rect is a half-open image rectangle.
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H
}

func (r Rect) Intersects(o Rect) bool {
	return r.X < o.X+o.W && o.X < r.X+r.W && r.Y < o.Y+o.H && o.Y < r.Y+r.H
}

func (r Rect) Intersection(o Rect) Rect {
	x0 := max(r.X, o.X)
	y0 := max(r.Y, o.Y)
	x1 := min(r.X+r.W, o.X+o.W)
	y1 := min(r.Y+r.H, o.Y+o.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func (r Rect) Union(o Rect) Rect {
	if r.Empty() {
		return o
	}
	if o.Empty() {
		return r
	}
	x0 := min(r.X, o.X)
	y0 := min(r.Y, o.Y)
	x1 := max(r.X+r.W, o.X+o.W)
	y1 := max(r.Y+r.H, o.Y+o.H)
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func (r Rect) Cover(width, height int) Rect {
	return r.Intersection(Rect{W: width, H: height})
}

// Point identifies a pixel coordinate.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Image is a packed source image. Pixels are interleaved channels in Layout
// order. Values use the native range of BitDepth.
type Image struct {
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	BitDepth BitDepth `json:"bitDepth"`
	Layout   Layout   `json:"layout"`
	Pixels   []uint16 `json:"-"`
}

// AlphaIndex is the packed index of A for both supported layouts.
func (im *Image) AlphaIndex() int { return 3 }

func (im *Image) Validate() error {
	if im.Width <= 0 {
		return schemaErr(0, "width", "width must be positive")
	}
	if im.Height <= 0 {
		return schemaErr(0, "height", "height must be positive")
	}
	if !im.BitDepth.Valid() {
		return paramErr(0, "bitDepth", "unsupported bit depth %d", im.BitDepth)
	}
	if !im.Layout.Valid() {
		return paramErr(0, "layout", "unsupported layout %q", im.Layout)
	}
	want := im.Width * im.Height * 4
	if len(im.Pixels) != want {
		return schemaErr(0, "pixels", "got %d components, want %d", len(im.Pixels), want)
	}
	limit := im.BitDepth.Max()
	for i, v := range im.Pixels {
		if v > limit {
			x := (i / 4) % im.Width
			y := (i / 4) / im.Width
			return paramErr(0, "pixels", "value %d at (%d,%d,c%d) exceeds %d", v, x, y, i%4, limit)
		}
	}
	return nil
}

// StageImage is the result of one plan entry. Channels may be 1 through 4.
// AlphaIndex is -1 when the stage has no alpha channel.
type StageImage struct {
	Width      int              `json:"width"`
	Height     int              `json:"height"`
	BitDepth   BitDepth         `json:"bitDepth"`
	Channels   int              `json:"channels"`
	AlphaIndex int              `json:"alphaIndex"`
	Pixels     []uint16         `json:"-"`
	Dropped    []DroppedChannel `json:"dropped,omitempty"`
}

type DroppedChannel struct {
	Entry   int `json:"entry"`
	Channel int `json:"channel"`
}

func (im *StageImage) Validate() error {
	if im.Width <= 0 || im.Height <= 0 {
		return schemaErr(0, "geometry", "invalid stage dimensions %dx%d", im.Width, im.Height)
	}
	if !im.BitDepth.Valid() {
		return paramErr(0, "bitDepth", "unsupported bit depth %d", im.BitDepth)
	}
	if im.Channels < 1 || im.Channels > 4 {
		return paramErr(0, "channels", "channels must be in [1,4], got %d", im.Channels)
	}
	if im.AlphaIndex < -1 || im.AlphaIndex >= im.Channels {
		return paramErr(0, "alphaIndex", "alpha index %d is outside [0,%d)", im.AlphaIndex, im.Channels)
	}
	want := im.Width * im.Height * im.Channels
	if len(im.Pixels) != want {
		return schemaErr(0, "pixels", "got %d components, want %d", len(im.Pixels), want)
	}
	limit := im.BitDepth.Max()
	for _, v := range im.Pixels {
		if v > limit {
			return paramErr(0, "pixels", "component %d exceeds %d", v, limit)
		}
	}
	return nil
}

func (im *StageImage) Clone() *StageImage {
	cp := *im
	cp.Pixels = append([]uint16(nil), im.Pixels...)
	cp.Dropped = append([]DroppedChannel(nil), im.Dropped...)
	return &cp
}

func (im *StageImage) Pixel(x, y int) []uint16 {
	off := (y*im.Width + x) * im.Channels
	return im.Pixels[off : off+im.Channels]
}

// Quantization selects the declared direction when reducing bit depth.
type Quantization string

const (
	QuantRound Quantization = "round"
	QuantFloor Quantization = "floor"
	QuantCeil  Quantization = "ceil"
)

func (q Quantization) Valid() bool {
	return q == QuantRound || q == QuantFloor || q == QuantCeil
}

// AlphaMode declares how source alpha affects the other channels.
type AlphaMode string

const (
	AlphaPreserve    AlphaMode = "preserve"
	AlphaOpaque      AlphaMode = "opaque"
	AlphaPremultiply AlphaMode = "premultiply"
	AlphaStraighten  AlphaMode = "straighten"
)

func (a AlphaMode) Valid() bool {
	switch a {
	case AlphaPreserve, AlphaOpaque, AlphaPremultiply, AlphaStraighten:
		return true
	default:
		return false
	}
}

type FillSpec struct {
	Channel int    `json:"channel"`
	Value   uint16 `json:"value"`
}

// Entry is one ordered conversion stage. Permutation maps destination slot to
// a source channel index; -1 marks a destination slot supplied by Fill.
// Drop must be exactly the sorted complement of retained source channels.
type Entry struct {
	Seq            int          `json:"seq"`
	TargetDepth    BitDepth     `json:"targetDepth"`
	TargetChannels int          `json:"targetChannels"`
	Permutation    []int        `json:"permutation"`
	Fill           []FillSpec   `json:"fill"`
	Drop           []int        `json:"drop"`
	Quantization   Quantization `json:"quantization"`
	Alpha          AlphaMode    `json:"alpha"`
	Region         *Rect        `json:"region,omitempty"`
}

type Plan struct {
	SchemaVersion int     `json:"schemaVersion"`
	Entries       []Entry `json:"entries"`
}

func (p *Plan) Entry(seq int) (*Entry, bool) {
	for i := range p.Entries {
		if p.Entries[i].Seq == seq {
			return &p.Entries[i], true
		}
	}
	return nil, false
}

func normalizedEntry(e Entry) Entry {
	n := e
	n.Permutation = append(make([]int, 0, len(e.Permutation)), e.Permutation...)
	n.Fill = append(make([]FillSpec, 0, len(e.Fill)), e.Fill...)
	n.Drop = append(make([]int, 0, len(e.Drop)), e.Drop...)
	sort.Slice(n.Fill, func(i, j int) bool { return n.Fill[i].Channel < n.Fill[j].Channel })
	sort.Ints(n.Drop)
	if e.Region != nil {
		r := *e.Region
		n.Region = &r
	}
	return n
}

func (e Entry) String() string {
	region := "full"
	if e.Region != nil {
		region = fmt.Sprintf("%d,%d,%d,%d", e.Region.X, e.Region.Y, e.Region.W, e.Region.H)
	}
	return fmt.Sprintf("entry %d depth=%d channels=%d perm=%v fill=%v drop=%v quant=%s alpha=%s region=%s",
		e.Seq, e.TargetDepth, e.TargetChannels, e.Permutation, e.Fill, e.Drop,
		e.Quantization, e.Alpha, region)
}

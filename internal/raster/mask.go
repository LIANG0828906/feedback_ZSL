// Package raster implements deterministic scanline rasterization of stroke
// bands into grayscale masks and per-sample density statistics.
package raster

import (
	"math"

	"strokeforge/internal/model"
)

// SuperSample is the per-axis supersampling factor (16 subpixels per pixel).
const SuperSample = 4

// Mask is a grayscale coverage buffer. Values are 0..255.
type Mask struct {
	Width  int
	Height int
	Pix    []byte
}

// Frame maps font units to pixel coordinates:
//
//	px = (x - bbox.MinX + padding) * scale
//	py = (bbox.MaxY + padding - y) * scale
type Frame struct {
	BBox    model.BBox
	Padding float64
	Scale   float64
	Width   int
	Height  int
}

// NewFrame derives the pixel frame from a bbox and stroke params.
func NewFrame(b model.BBox, params model.StrokeParams) Frame {
	w := int(math.Ceil((b.MaxX-b.MinX+2*params.BBoxPadding)*params.Scale - 1e-9))
	h := int(math.Ceil((b.MaxY-b.MinY+2*params.BBoxPadding)*params.Scale - 1e-9))
	return Frame{BBox: b, Padding: params.BBoxPadding, Scale: params.Scale, Width: w, Height: h}
}

// ToPixel maps a font-unit point to a pixel-space float point.
func (f Frame) ToPixel(p model.FPoint) (float64, float64) {
	x := (p.X - f.BBox.MinX + f.Padding) * f.Scale
	y := (f.BBox.MaxY + f.Padding - p.Y) * f.Scale
	return x, y
}

// NewMask allocates an empty mask for the frame.
func NewMask(f Frame) *Mask {
	return &Mask{Width: f.Width, Height: f.Height, Pix: make([]byte, f.Width*f.Height)}
}

func (m *Mask) idx(x, y int) int { return y*m.Width + x }

// CompositeStamp stamps one convex polygon into the mask using even-odd
// scanline coverage evaluated on a SuperSample x SuperSample subpixel grid.
// The operation is order independent (max coverage), which is what makes
// parallel per-segment rendering recombine exactly.
func (m *Mask) CompositeStamp(p model.Polygon, f Frame) {
	if len(p.Points) < 3 {
		return
	}
	pixPts := make([]model.FPoint, len(p.Points))
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for i, pt := range p.Points {
		x, y := f.ToPixel(pt)
		pixPts[i] = model.FPoint{X: x, Y: y}
		if x < minX {
			minX = x
		}
		if x > maxX {
			maxX = x
		}
		if y < minY {
			minY = y
		}
		if y > maxY {
			maxY = y
		}
	}
	x0 := int(math.Floor(minX))
	y0 := int(math.Floor(minY))
	x1 := int(math.Floor(maxX))
	y1 := int(math.Floor(maxY))
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 >= m.Width {
		x1 = m.Width - 1
	}
	if y1 >= m.Height {
		y1 = m.Height - 1
	}
	for py := y0; py <= y1; py++ {
		for px := x0; px <= x1; px++ {
			hit := 0
			for sy := 0; sy < SuperSample; sy++ {
				for sx := 0; sx < SuperSample; sx++ {
					sxPos := float64(px) + (float64(sx)+0.5)/SuperSample
					syPos := float64(py) + (float64(sy)+0.5)/SuperSample
					if pointInConvex(sxPos, syPos, pixPts) {
						hit++
					}
				}
			}
			if hit == 0 {
				continue
			}
			cov := byte((hit * 255) / (SuperSample * SuperSample))
			i := m.idx(px, py)
			if cov > m.Pix[i] {
				m.Pix[i] = cov
			}
		}
	}
}

// pointInConvex performs a half-plane test against a convex CCW/CW polygon.
func pointInConvex(x, y float64, pts []model.FPoint) bool {
	n := len(pts)
	positive := 0
	negative := 0
	for i := 0; i < n; i++ {
		a := pts[i]
		b := pts[(i+1)%n]
		cross := (b.X-a.X)*(y-a.Y) - (b.Y-a.Y)*(x-a.X)
		if cross > 1e-9 {
			positive++
		} else if cross < -1e-9 {
			negative++
		}
		if positive > 0 && negative > 0 {
			return false
		}
	}
	return true
}

// BandLike exposes the nib stamps of a generated stroke band.
type BandLike interface {
	NibStamps() []model.Polygon
}

// CompositeBand stamps all nib polygons of a band into the mask.
func (m *Mask) CompositeBand(b BandLike, f Frame) {
	for _, s := range b.NibStamps() {
		m.CompositeStamp(s, f)
	}
}

// MergeMax merges another same-sized mask via per-pixel maximum.
func (m *Mask) MergeMax(other *Mask) error {
	if m.Width != other.Width || m.Height != other.Height {
		return model.NewError(model.EDiff, "mask size mismatch during merge").
			WithField("mask_width/mask_height")
	}
	for i := range m.Pix {
		if other.Pix[i] > m.Pix[i] {
			m.Pix[i] = other.Pix[i]
		}
	}
	return nil
}

// Equal reports pixel-exact equality and the first differing coordinate.
func (m *Mask) Equal(other *Mask) (bool, int, int) {
	if m.Width != other.Width || m.Height != other.Height {
		return false, 0, 0
	}
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			i := y*m.Width + x
			if m.Pix[i] != other.Pix[i] {
				return false, x, y
			}
		}
	}
	return true, 0, 0
}

// Clone returns an independent copy.
func (m *Mask) Clone() *Mask {
	pix := make([]byte, len(m.Pix))
	copy(pix, m.Pix)
	return &Mask{Width: m.Width, Height: m.Height, Pix: pix}
}

// DensityAt returns normalized coverage (0..1) at the pixel nearest p.
func (m *Mask) DensityAt(p model.FPoint, f Frame) (int, error) {
	px, py := f.ToPixel(p)
	ix := int(math.Round(px))
	iy := int(math.Round(py))
	if ix < 0 || iy < 0 || ix >= m.Width || iy >= m.Height {
		return 0, model.NewError(model.EDens, "sample point outside mask frame").
			WithField("sample_points")
	}
	return int(m.Pix[m.idx(ix, iy)]), nil
}

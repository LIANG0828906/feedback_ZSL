package maskqc

import (
	"fmt"
	"math"
)

const (
	MinBrushRadius = 1
	MaxBrushRadius = 128
)

func ValidateStrokeParameters(s *Stroke) error {
	if s == nil {
		return newError(EPara, "stroke", "stroke is nil")
	}
	if s.HardnessPermille < 0 || s.HardnessPermille > HardnessScale {
		return paraError(fmt.Sprintf("strokes.%s.hardness_permille", s.ID), s.HardnessPermille, 0, HardnessScale)
	}
	if s.WeightPermyriad < 0 || s.WeightPermyriad > WeightScale {
		return paraError(fmt.Sprintf("strokes.%s.weight_permyriad", s.ID), s.WeightPermyriad, 0, WeightScale)
	}
	if s.Radius < MinBrushRadius || s.Radius > MaxBrushRadius {
		return paraError(fmt.Sprintf("strokes.%s.radius", s.ID), s.Radius, MinBrushRadius, MaxBrushRadius)
	}
	return nil
}

// StrokeBounds returns the affected pixel bounding box, expanded by brush radius.
func StrokeBounds(s Stroke, width, height int) Rect {
	r := s.Radius
	b := Rect{
		MinX: minInt(s.Start.X, s.End.X) - r,
		MinY: minInt(s.Start.Y, s.End.Y) - r,
		MaxX: maxInt(s.Start.X, s.End.X) + r + 1,
		MaxY: maxInt(s.Start.Y, s.End.Y) + r + 1,
	}
	if width > 0 && height > 0 {
		b = b.Intersect(Rect{0, 0, width, height})
	}
	return b
}

// RasterizeStrokeRect renders coverage in global coordinates into rect-sized
// single-channel alpha. All geometry is integer and deterministic.
func RasterizeStrokeRect(s *Stroke, rect Rect) (*Mask, error) {
	if err := ValidateStrokeParameters(s); err != nil {
		return nil, err
	}
	if rect.Empty() {
		return NewMask(rect.Width(), rect.Height()), nil
	}
	out := NewMask(rect.Width(), rect.Height())
	bounds := StrokeBounds(*s, 0, 0).Intersect(rect)
	if bounds.Empty() {
		return out, nil
	}

	dx := s.End.X - s.Start.X
	dy := s.End.Y - s.Start.Y
	len2 := dx*dx + dy*dy
	r := s.Radius
	inner := r * s.HardnessPermille / HardnessScale

	for y := bounds.MinY; y < bounds.MaxY; y++ {
		for x := bounds.MinX; x < bounds.MaxX; x++ {
			dist2 := segmentDistanceSquared(x, y, s.Start.X, s.Start.Y, dx, dy, len2)
			if dist2 > int64(r*r) {
				continue
			}
			d := intSquareRootFloor(dist2)
			var coverage int
			switch {
			case d <= inner:
				coverage = 255
			case inner >= r:
				coverage = 255
			default:
				den := r - inner
				coverage = 255 * (r - d) / den
				if coverage < 0 {
					coverage = 0
				}
				if coverage > 255 {
					coverage = 255
				}
			}
			alpha := coverage * s.WeightPermyriad / WeightScale
			idx := (y-rect.MinY)*rect.Width() + (x - rect.MinX)
			out.Alpha[idx] = byte(alpha)
		}
	}
	return out, nil
}

func segmentDistanceSquared(px, py, x1, y1, dx, dy, len2 int) int64 {
	wx := px - x1
	wy := py - y1
	if len2 == 0 {
		return int64(wx)*int64(wx) + int64(wy)*int64(wy)
	}
	dot := wx*dx + wy*dy
	switch {
	case dot <= 0:
		return int64(wx)*int64(wx) + int64(wy)*int64(wy)
	case dot >= len2:
		ex := px - (x1 + dx)
		ey := py - (y1 + dy)
		return int64(ex)*int64(ex) + int64(ey)*int64(ey)
	default:
		cross := wx*dy - wy*dx
		// Floor of cross^2 / len2 is sufficient for the declared raster algorithm.
		return int64(cross) * int64(cross) / int64(len2)
	}
}

func intSquareRootFloor(v int64) int {
	if v <= 0 {
		return 0
	}
	r := int(math.Sqrt(float64(v)))
	for int64((r+1)*(r+1)) <= v {
		r++
	}
	for int64(r*r) > v {
		r--
	}
	return r
}

// AlphaOver performs deterministic 8-bit source-over compositing.
func AlphaOver(dst, src byte) byte {
	if src == 0 {
		return dst
	}
	if src == 255 {
		return src
	}
	return byte(int(src) + int(dst)*(255-int(src))/255)
}

func ComposeAlpha(dst []byte, components ...[]byte) error {
	n := len(dst)
	for _, comp := range components {
		if len(comp) != n {
			return newError(EPara, "components", "component length %d != destination %d", len(comp), n)
		}
		for i, src := range comp {
			dst[i] = AlphaOver(dst[i], src)
		}
	}
	return nil
}

// ComposeTile renders ordered strokes over a candidate tile.
func ComposeTile(icon *Icon, tile Rect, strokes []*Stroke) (*Mask, []string, error) {
	candidate, err := CropMask(icon.Candidate, tile)
	if err != nil {
		return nil, nil, err
	}
	participants := make([]string, 0, len(strokes))
	for _, stroke := range strokes {
		bounds := StrokeBounds(*stroke, icon.Width, icon.Height)
		if bounds.Intersect(tile).Empty() {
			continue
		}
		comp, err := RasterizeStrokeRect(stroke, tile)
		if err != nil {
			return nil, nil, err
		}
		if err := ComposeAlpha(candidate.Alpha, comp.Alpha); err != nil {
			return nil, nil, err
		}
		participants = append(participants, stroke.ID)
	}
	return candidate, participants, nil
}

func CropMask(m *Mask, r Rect) (*Mask, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	full := Rect{0, 0, m.Width, m.Height}
	cr := r.Intersect(full)
	if cr.Empty() {
		return NewMask(r.Width(), r.Height()), nil
	}
	out := NewMask(r.Width(), r.Height())
	for y := cr.MinY; y < cr.MaxY; y++ {
		for x := cr.MinX; x < cr.MaxX; x++ {
			out.Alpha[(y-r.MinY)*r.Width()+(x-r.MinX)] = m.Alpha[y*m.Width+x]
		}
	}
	return out, nil
}

func TileRect(icon *Icon, tx, ty int) Rect {
	return Rect{
		MinX: tx * icon.TileWidth,
		MinY: ty * icon.TileHeight,
		MaxX: (tx + 1) * icon.TileWidth,
		MaxY: (ty + 1) * icon.TileHeight,
	}.Intersect(Rect{0, 0, icon.Width, icon.Height})
}

package maskqa

import (
	"math"
)

// Component is one stroke's coverage on one tile. Alpha uses basis points 0..10000.
type Component struct {
	StrokeIndex int      `json:"stroke_index"`
	TileID      int      `json:"tile_id"`
	Rect        Rect     `json:"rect"`
	Alpha       []uint16 `json:"-"`
	ContentHash string   `json:"content_hash"`
}

func brushRadius(p *Project) float64 {
	return float64(p.BrushWidthPx) / 2.0
}

// StrokeBounds returns the pixel rectangle that can receive nonzero coverage.
func StrokeBounds(p *Project, s Stroke) Rect {
	r := brushRadius(p)
	pad := int(math.Ceil(r)) + 1
	x0 := minInt(s.Start.X, s.End.X) - pad
	y0 := minInt(s.Start.Y, s.End.Y) - pad
	x1 := maxInt(s.Start.X, s.End.X) + pad + 1
	y1 := maxInt(s.Start.Y, s.End.Y) + pad + 1
	return Rect{X0: x0, Y0: y0, X1: x1, Y1: y1}.Intersect(p.Rect())
}

func segmentGeometry(px, py int, s Stroke) (distance, along float64) {
	ax := float64(s.Start.X)
	ay := float64(s.Start.Y)
	dx := float64(s.End.X - s.Start.X)
	dy := float64(s.End.Y - s.Start.Y)
	wx := float64(px) - ax
	wy := float64(py) - ay
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = (wx*dx + wy*dy) / l2
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
	}
	cx := ax + t*dx
	cy := ay + t*dy
	return math.Hypot(float64(px)-cx, float64(py)-cy), t
}

// GeometryCoverageBPS returns brush-shape coverage before pressure/weight multiplication.
func GeometryCoverageBPS(px, py int, s Stroke, brushWidth int) uint16 {
	radius := float64(brushWidth) / 2.0
	dist, _ := segmentGeometry(px, py, s)
	if dist > radius {
		return 0
	}
	hardness := float64(clampInt(s.HardnessPermille, 0, WeightPermilleScale)) / float64(WeightPermilleScale)
	core := radius * hardness
	if dist <= core || radius-core < 1e-9 {
		return WeightBasisScale
	}
	v := (radius - dist) / (radius - core) * WeightBasisScale
	if v < 0 {
		v = 0
	}
	if v > WeightBasisScale {
		v = WeightBasisScale
	}
	return uint16(math.Floor(v))
}

func weightCoverageAlpha(weightBPS int, geom uint16) uint16 {
	if geom == 0 || weightBPS <= 0 {
		return 0
	}
	// Ceiling multiplication keeps any analytically supported pixel nonzero.
	v := (uint64(weightBPS)*uint64(geom) + WeightBasisScale - 1) / WeightBasisScale
	if v > WeightBasisScale {
		v = WeightBasisScale
	}
	if v == 0 {
		v = 1
	}
	return uint16(v)
}

// RasterStrokeTile deterministically rasterizes one stroke into a tile buffer.
func RasterStrokeTile(p *Project, s Stroke, tile Tile) (*Component, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	if s.HardnessPermille < 0 || s.HardnessPermille > WeightPermilleScale {
		e := paramError("hardness_permille", "0..1000", "hardness %d out of range", s.HardnessPermille)
		e.SourceVersion = p.SourceVersion
		return nil, e
	}
	if s.WeightBPS < 1 || s.WeightBPS > WeightBasisScale {
		e := paramError("weight", "1..10000 basis points", "weight %d out of range", s.WeightBPS)
		e.SourceVersion = p.SourceVersion
		return nil, e
	}
	n := tile.Rect.Dx() * tile.Rect.Dy()
	alpha := make([]uint16, n)
	i := 0
	for y := tile.Rect.Y0; y < tile.Rect.Y1; y++ {
		for x := tile.Rect.X0; x < tile.Rect.X1; x++ {
			geom := GeometryCoverageBPS(x, y, s, p.BrushWidthPx)
			alpha[i] = weightCoverageAlpha(s.WeightBPS, geom)
			i++
		}
	}
	return &Component{StrokeIndex: s.Index, TileID: tile.ID, Rect: tile.Rect, Alpha: alpha, ContentHash: HashComponentAlpha(alpha)}, nil
}

func HashComponentAlpha(alpha []uint16) string {
	b := make([]byte, len(alpha)*2)
	for i, v := range alpha {
		b[i*2] = byte(v >> 8)
		b[i*2+1] = byte(v)
	}
	return HashBytes(b)
}

// CompositeOver performs integer source-over alpha composition in basis-point scale.
func CompositeOver(dst, src uint16) uint16 {
	if src == 0 {
		return dst
	}
	if src >= WeightBasisScale {
		return WeightBasisScale
	}
	rest := uint32(dst) * uint32(WeightBasisScale-src) / WeightBasisScale
	out := uint32(src) + rest
	if out > WeightBasisScale {
		out = WeightBasisScale
	}
	return uint16(out)
}

// BuildAnalyticReferenceMask directly evaluates the analytic stroke footprint.
// It is used by the deterministic generator to construct truth, never by recovery/report code.
func BuildAnalyticReferenceMask(p *Project) *Mask {
	m := NewMask(p.Width, p.Height)
	for y := 0; y < p.Height; y++ {
		for x := 0; x < p.Width; x++ {
			for _, s := range p.Strokes {
				if GeometryCoverageBPS(x, y, s, p.BrushWidthPx) > 0 {
					m.Set(x, y, true)
					break
				}
			}
		}
	}
	return m
}

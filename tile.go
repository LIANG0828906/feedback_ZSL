package iconcut

import (
	"fmt"
	"math"
)

type Tile struct {
	Index        int     `json:"index"`
	TX           int     `json:"tx"`
	TY           int     `json:"ty"`
	Core         Rect    `json:"core"`
	Band         Rect    `json:"band"`
	GrowRect     Rect    `json:"grow_rect"`
	SourceRect   Rect    `json:"source_rect"`
	Polygon      []Point `json:"polygon"`
	RequiredBand int     `json:"required_band"`
}

func BandWidthForOrder(order int) int {
	return 2*order + 1
}

func BuildTiles(w, h, tileW, tileH, band, order int, m *Mapper) ([]Tile, error) {
	if tileW <= 0 || tileH <= 0 {
		return nil, NewError(EMeta, "declared.tile_width", "tile dimensions must be positive")
	}
	if w%tileW != 0 {
		return nil, NewError(EMeta, "declared.tile_width", "tile_width must divide corrected width %d", w)
	}
	if h%tileH != 0 {
		return nil, NewError(EMeta, "declared.tile_height", "tile_height must divide corrected height %d", h)
	}
	nx := w / tileW
	ny := h / tileH
	required := BandWidthForOrder(order)
	tiles := make([]Tile, 0, nx*ny)
	idx := 0
	for ty := 0; ty < ny; ty++ {
		for tx := 0; tx < nx; tx++ {
			core := Rect{X: tx * tileW, Y: ty * tileH, W: tileW, H: tileH}
			br := core.Expand(band, w, h)
			tile := Tile{
				Index: idx, TX: tx, TY: ty, Core: core, Band: br, GrowRect: br,
				RequiredBand: required,
				Polygon: []Point{
					m.MapPoint(Point{X: float64(br.X), Y: float64(br.Y)}),
					m.MapPoint(Point{X: float64(br.MaxX()), Y: float64(br.Y)}),
					m.MapPoint(Point{X: float64(br.MaxX()), Y: float64(br.MaxY())}),
					m.MapPoint(Point{X: float64(br.X), Y: float64(br.MaxY())}),
				},
			}
			tile.SourceRect = sourceRectForBand(m, br, order)
			tiles = append(tiles, tile)
			idx++
		}
	}
	return tiles, nil
}

func sourceRectForBand(m *Mapper, band Rect, order int) Rect {
	entries := m.BuildMap(band)
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, e := range entries {
		x := float64(e.SXQ) / float64(m.Scale)
		y := float64(e.SYQ) / float64(m.Scale)
		minX = math.Min(minX, x)
		minY = math.Min(minY, y)
		maxX = math.Max(maxX, x)
		maxY = math.Max(maxY, y)
	}
	x0 := int(math.Floor(minX))
	y0 := int(math.Floor(minY))
	x1 := int(math.Ceil(maxX))
	y1 := int(math.Ceil(maxY))
	pad := interpolationRadius(order) + 1
	x0 -= pad
	y0 -= pad
	x1 += pad
	y1 += pad
	srcW := m.G.Source.W
	srcH := m.G.Source.H
	x0 = clampInt(x0, 0, srcW)
	y0 = clampInt(y0, 0, srcH)
	x1 = clampInt(x1, 0, srcW)
	y1 = clampInt(y1, 0, srcH)
	if x1 <= x0 {
		x1 = minInt(srcW, x0+1)
	}
	if y1 <= y0 {
		y1 = minInt(srcH, y0+1)
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func requiredSourceRange(sx, sy float64, order int) (int, int, int, int) {
	switch order {
	case 0:
		x := int(math.Floor(sx + 0.5))
		y := int(math.Floor(sy + 0.5))
		return x, x, y, y
	case 1:
		x0 := int(math.Floor(sx))
		y0 := int(math.Floor(sy))
		return x0, x0 + 1, y0, y0 + 1
	default:
		x0 := int(math.Floor(sx))
		y0 := int(math.Floor(sy))
		return x0 - 1, x0 + 2, y0 - 1, y0 + 2
	}
}

func (t Tile) bandError(band int, reason string, extra map[string]any) *Error {
	d := map[string]any{"tile_index": t.Index, "tx": t.TX, "ty": t.TY, "band_px": band, "required_band": t.RequiredBand}
	for k, v := range extra {
		d[k] = v
	}
	return WithDetails(NewError(EBand, fmt.Sprintf("tiles[%d].band_px", t.Index), "%s for tile (%d,%d)", reason, t.TX, t.TY), d)
}

func CheckTileBand(t Tile, m *Mapper, order, band int) error {
	if band < t.RequiredBand {
		return t.bandError(band, "declared overlap band is smaller than interpolation support", map[string]any{
			"condition": fmt.Sprintf("band_px >= %d", t.RequiredBand),
		})
	}
	entries := m.BuildMap(t.Core)
	sr := t.SourceRect
	for _, e := range entries {
		sx := float64(e.SXQ) / float64(m.Scale)
		sy := float64(e.SYQ) / float64(m.Scale)
		x0, x1, y0, y1 := requiredSourceRange(sx, sy, order)
		if x0 >= 0 && x0 < m.G.Source.W && (x0 < sr.X || x0 >= sr.MaxX()) {
			return t.bandError(band, "required source sample is outside mapped source polygon", map[string]any{
				"pixel": IPoint{X: e.X, Y: e.Y}, "missing": IPoint{X: x0, Y: y0}, "source_rect": sr,
			})
		}
		if x1 >= 0 && x1 < m.G.Source.W && (x1 < sr.X || x1 >= sr.MaxX()) {
			return t.bandError(band, "required source sample is outside mapped source polygon", map[string]any{
				"pixel": IPoint{X: e.X, Y: e.Y}, "missing": IPoint{X: x1, Y: y1}, "source_rect": sr,
			})
		}
		if y0 >= 0 && y0 < m.G.Source.H && (y0 < sr.Y || y0 >= sr.MaxY()) {
			return t.bandError(band, "required source sample is outside mapped source polygon", map[string]any{
				"pixel": IPoint{X: e.X, Y: e.Y}, "missing": IPoint{X: x0, Y: y0}, "source_rect": sr,
			})
		}
		if y1 >= 0 && y1 < m.G.Source.H && (y1 < sr.Y || y1 >= sr.MaxY()) {
			return t.bandError(band, "required source sample is outside mapped source polygon", map[string]any{
				"pixel": IPoint{X: e.X, Y: e.Y}, "missing": IPoint{X: x1, Y: y1}, "source_rect": sr,
			})
		}
	}
	return nil
}

func subImage(src *Image, r Rect) *Image {
	r = r.Intersect(Rect{W: src.W, H: src.H})
	out := NewImage(r.W, r.H, 3)
	for y := 0; y < r.H; y++ {
		srcStart := ((r.Y+y)*src.W + r.X) * 3
		dstStart := y * r.W * 3
		copy(out.Pix[dstStart:dstStart+r.W*3], src.Pix[srcStart:srcStart+r.W*3])
	}
	return out
}

func CorrectTile(src *Image, m *Mapper, t Tile, order, band int) (*Image, []MapEntry, error) {
	if err := CheckTileBand(t, m, order, band); err != nil {
		return nil, nil, err
	}
	local := subImage(src, t.SourceRect)
	rs, err := NewResampler(local, m, order)
	if err != nil {
		return nil, nil, err
	}
	entries := m.BuildMap(t.Core)
	out := NewImage(t.Core.W, t.Core.H, 3)
	for _, e := range entries {
		sx := float64(e.SXQ)/float64(m.Scale) - float64(t.SourceRect.X)
		sy := float64(e.SYQ)/float64(m.Scale) - float64(t.SourceRect.Y)
		c := rs.Sample(sx, sy)
		lx := e.X - t.Core.X
		ly := e.Y - t.Core.Y
		out.SetPixel(lx, ly, c)
	}
	return out, entries, nil
}

func TilePixelsWithinBuffer(t Tile) int {
	return t.Band.W*t.Band.H + t.SourceRect.W*t.SourceRect.H + t.Core.W*t.Core.H*4
}

package maskqc

import "fmt"

type Component struct {
	StrokeID    string `json:"strokeId"`
	TileID      string `json:"tileId"`
	CacheKey    string `json:"cacheKey"`
	ContentHash string `json:"contentHash"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Op          string `json:"op"`
	Alpha       []byte `json:"alpha"`
}

type Participant struct {
	StrokeID string `json:"strokeId"`
	Seq      int    `json:"seq"`
	Priority int    `json:"priority"`
	OrderKey string `json:"orderKey"`
	Reason   string `json:"reason"`
}

type TileOrderDecision struct {
	TileID       string        `json:"tileId"`
	OrderVersion string        `json:"orderVersion"`
	Rule         string        `json:"rule"`
	Participants []Participant `json:"participants"`
}

func ValidateStroke(s Stroke) error {
	field := fmt.Sprintf("strokes[%s].hardnessPermille", s.ID)
	if s.HardnessPermille < 1 || s.HardnessPermille > 1000 {
		return parameterError(field, 1, 1000, "hardnessPermille must be within declared range")
	}
	weightField := fmt.Sprintf("strokes[%s].weightBP", s.ID)
	if !ContainsInt(AllowedWeightsBP, s.WeightBP) {
		return &QCError{
			Code:       CodeParameter,
			Field:      weightField,
			Path:       "$." + weightField,
			Message:    "weightBP is not in the declared allowed scale set",
			LegalRange: []int{AllowedWeightsBP[0], AllowedWeightsBP[len(AllowedWeightsBP)-1]},
			Details:    map[string]any{"allowed": append([]int(nil), AllowedWeightsBP...)},
		}
	}
	radiusField := fmt.Sprintf("strokes[%s].radius", s.ID)
	if s.Radius < 1 || s.Radius > 64 {
		return parameterError(radiusField, 1, 64, "radius must be within declared pixel range and buffer limit")
	}
	if s.Seq <= 0 {
		return parameterError(fmt.Sprintf("strokes[%s].seq", s.ID), 1, 1<<30, "seq must be positive")
	}
	if s.Op == "" {
		s.Op = StrokeOpOver
	}
	if s.Op != StrokeOpOver && s.Op != StrokeOpErase {
		return parameterError(fmt.Sprintf("strokes[%s].op", s.ID), 0, 1, "op must be over or erase")
	}
	return nil
}

func sourceAlpha(weightBP int) byte {
	v := (int64(weightBP)*255 + int64(BasisTenThousand)/2) / BasisTenThousand
	if v > 255 {
		return 255
	}
	return byte(v)
}

// coverageAt returns 0..255 source coverage. All geometry is integer and
// uses squared distances, so repeated calls are bit-for-bit deterministic.
func coverageAt(x, y int, s Stroke) byte {
	r := int64(s.Radius)
	if r <= 0 {
		r = 1
	}
	r2 := r * r
	hardness := int64(s.HardnessPermille)
	coreR := r * hardness / 1000
	coreR2 := coreR * coreR

	ax, ay := int64(s.Start.X), int64(s.Start.Y)
	bx, by := int64(s.End.X), int64(s.End.Y)
	px, py := int64(x), int64(y)
	dx, dy := bx-ax, by-ay
	vx, vy := px-ax, py-ay
	l2 := dx*dx + dy*dy

	var insideCore, insideOuter bool
	var featherNum, featherDen int64

	if l2 == 0 {
		d2 := vx*vx + vy*vy
		insideCore = d2 <= coreR2
		insideOuter = d2 <= r2
		if !insideCore && insideOuter && r2 > coreR2 {
			featherNum = r2 - d2
			featherDen = r2 - coreR2
		}
	} else {
		dot := vx*dx + vy*dy
		switch {
		case dot <= 0:
			d2 := vx*vx + vy*vy
			insideCore = d2 <= coreR2
			insideOuter = d2 <= r2
			if !insideCore && insideOuter && r2 > coreR2 {
				featherNum = r2 - d2
				featherDen = r2 - coreR2
			}
		case dot >= l2:
			ex, ey := px-bx, py-by
			d2 := ex*ex + ey*ey
			insideCore = d2 <= coreR2
			insideOuter = d2 <= r2
			if !insideCore && insideOuter && r2 > coreR2 {
				featherNum = r2 - d2
				featherDen = r2 - coreR2
			}
		default:
			cross := dx*vy - dy*vx
			cross2 := cross * cross
			insideCore = cross2 <= coreR2*l2
			insideOuter = cross2 <= r2*l2
			if !insideCore && insideOuter && r2 > coreR2 {
				featherNum = r2*l2 - cross2
				featherDen = (r2 - coreR2) * l2
			}
		}
	}

	if !insideOuter {
		return 0
	}
	wa := int64(sourceAlpha(s.WeightBP))
	if insideCore || featherDen <= 0 {
		return byte(wa)
	}
	if featherNum <= 0 {
		return 0
	}
	a := wa * featherNum / featherDen
	if a > wa {
		a = wa
	}
	return byte(a)
}

func RasterizeStrokeTile(s Stroke, tile Tile) (*Component, error) {
	if err := ValidateStroke(s); err != nil {
		return nil, err
	}
	op := s.Op
	if op == "" {
		op = StrokeOpOver
	}
	width := tile.Rect.Width()
	height := tile.Rect.Height()
	alpha := make([]byte, width*height)
	for y := tile.Rect.MinY; y <= tile.Rect.MaxY; y++ {
		row := (y - tile.Rect.MinY) * width
		for x := tile.Rect.MinX; x <= tile.Rect.MaxX; x++ {
			a := coverageAt(x, y, s)
			if a != 0 {
				alpha[row+(x-tile.Rect.MinX)] = a
			}
		}
	}
	return &Component{
		StrokeID:    s.ID,
		TileID:      tile.ID,
		Width:       width,
		Height:      height,
		Op:          op,
		Alpha:       alpha,
		ContentHash: HashBytes(alpha),
	}, nil
}

func AlphaOver(dst, src byte) byte {
	if src == 0 {
		return dst
	}
	if src == 255 {
		return 255
	}
	rest := (int(dst)*int(255-src) + 127) / 255
	out := int(src) + rest
	if out > 255 {
		return 255
	}
	return byte(out)
}

func ApplyComponent(m *Mask, tile Tile, c *Component) {
	if m == nil || c == nil {
		return
	}
	width := tile.Rect.Width()
	for yy := 0; yy < c.Height; yy++ {
		y := tile.Rect.MinY + yy
		for xx := 0; xx < c.Width; xx++ {
			x := tile.Rect.MinX + xx
			if !m.InBounds(x, y) {
				continue
			}
			i := yy*width + xx
			if i >= len(c.Alpha) {
				continue
			}
			mi := m.index(x, y)
			if c.Op == StrokeOpErase {
				m.Alpha[mi] = AlphaErase(m.Alpha[mi], c.Alpha[i])
			} else {
				m.Alpha[mi] = AlphaOver(m.Alpha[mi], c.Alpha[i])
			}
		}
	}
}

func AlphaErase(dst, src byte) byte {
	if src == 0 {
		return dst
	}
	if src == 255 {
		return 0
	}
	return byte((int(dst)*(255-int(src)) + 127) / 255)
}

func AdjudicateTile(strokes []Stroke, tile Tile) TileOrderDecision {
	ordered := OrderedStrokes(strokes)
	decision := TileOrderDecision{
		TileID:       tile.ID,
		OrderVersion: OrderVersion,
		Rule:         "priority ascending, then seq ascending; only strokes whose declared bounds intersect tile",
	}
	for _, s := range ordered {
		bounds := s.Bounds()
		if _, ok := bounds.Intersect(tile.Rect); !ok {
			continue
		}
		decision.Participants = append(decision.Participants, Participant{
			StrokeID: s.ID,
			Seq:      s.Seq,
			Priority: s.Priority,
			OrderKey: fmt.Sprintf("p%06d/s%06d", s.Priority, s.Seq),
			Reason:   fmt.Sprintf("stroke bounds %+v intersects tile %+v", bounds, tile.Rect),
		})
	}
	return decision
}

// ComposeStrokesFull is a simple sequential reference implementation used by
// tests and the no-scheduler full-recompute path.
func ComposeStrokesFull(width, height int, base *Mask, strokes []Stroke) (*Mask, []TileOrderDecision, error) {
	var out *Mask
	if base != nil {
		if base.Width != width || base.Height != height {
			return nil, nil, schemaError("mask", "candidate mask dimensions do not match project")
		}
		out = base.Clone()
	} else {
		out = NewMask(width, height)
	}
	tiles := Tiles(width, height, DefaultTileSize)
	decisions := make([]TileOrderDecision, 0, len(tiles))
	for _, tile := range tiles {
		decisions = append(decisions, AdjudicateTile(strokes, tile))
	}
	for _, s := range OrderedStrokes(strokes) {
		if err := ValidateStroke(s); err != nil {
			return nil, nil, err
		}
		bounds := s.Bounds()
		for _, tile := range tiles {
			if _, ok := bounds.Intersect(tile.Rect); !ok {
				continue
			}
			comp, err := RasterizeStrokeTile(s, tile)
			if err != nil {
				return nil, nil, err
			}
			ApplyComponent(out, tile, comp)
		}
	}
	return out, decisions, nil
}

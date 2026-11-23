// Package skeleton flattens validated closed contours and records the contour
// index, source curve index and cumulative arc-length parameter of every point.
package skeleton

import (
	"fmt"

	"glyphbrush/internal/geom"
	"glyphbrush/internal/model"
)

const DefaultTolerance = 0.2

type Point struct {
	GlyphID      string     `json:"glyphId"`
	ContourIndex int        `json:"contourIndex"`
	CurveIndex   int        `json:"curveIndex"`
	ArcLength    float64    `json:"arcLength"`
	Position     geom.Point `json:"position"`
}

type Run struct {
	CurveIndex int     `json:"curveIndex"`
	StartArc   float64 `json:"startArc"`
	EndArc     float64 `json:"endArc"`
	Points     []Point `json:"points"`
}

type Contour struct {
	GlyphID      string      `json:"glyphId"`
	ContourIndex int         `json:"contourIndex"`
	Closed       bool        `json:"closed"`
	Points       []Point     `json:"points"`
	Runs         []Run       `json:"runs"`
	Length       float64     `json:"length"`
	Area         float64     `json:"area"`
	Bounds       geom.Bounds `json:"bounds"`
}

// Polygon returns a polygon without a repeated closing point.
func (c Contour) Polygon() []geom.Point {
	if len(c.Points) == 0 {
		return nil
	}
	out := make([]geom.Point, 0, len(c.Points))
	for _, p := range c.Points {
		out = append(out, p.Position)
	}
	if len(out) > 1 && almostSame(out[0], out[len(out)-1]) {
		out = out[:len(out)-1]
	}
	return out
}

func almostSame(a, b geom.Point) bool {
	return a.Distance(b) < 1e-7
}

// Build flattens every contour of g. Validation must have happened before this
// function is called; the checks here guard against degenerate input.
func Build(g model.Glyph, tolerance float64) ([]Contour, error) {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	out := make([]Contour, 0, len(g.Contours))
	for _, c := range g.Contours {
		if len(c.Segments) == 0 {
			return nil, fmt.Errorf("contour %d has no segments", c.Index)
		}
		sc := Contour{GlyphID: g.ID, ContourIndex: c.Index, Closed: c.Closed}
		bounds := geom.EmptyBounds()
		var arc float64
		for si, seg := range c.Segments {
			p0 := seg.Start.F()
			startPt := Point{GlyphID: g.ID, ContourIndex: c.Index, CurveIndex: si, ArcLength: arc, Position: p0}
			if si == 0 {
				sc.Points = append(sc.Points, startPt)
				bounds.AddPoint(p0)
			}
			sc.Runs = append(sc.Runs, Run{CurveIndex: si, StartArc: arc, Points: []Point{startPt}})
			flat, err := model.FlattenSegment(seg, tolerance)
			if err != nil {
				return nil, fmt.Errorf("contour %d segment %d: %w", c.Index, si, err)
			}
			run := &sc.Runs[si]
			prev := p0
			for _, p := range flat {
				arc += p.Distance(prev)
				sp := Point{
					GlyphID:      g.ID,
					ContourIndex: c.Index,
					CurveIndex:   si,
					ArcLength:    arc,
					Position:     p,
				}
				sc.Points = append(sc.Points, sp)
				run.Points = append(run.Points, sp)
				bounds.AddPoint(p)
				prev = p
			}
			run.EndArc = arc
		}
		sc.Length = arc
		sc.Bounds = bounds
		sc.Area = geom.Area(sc.Polygon())
		out = append(out, sc)
	}
	return out, nil
}

// ValidateClosed verifies start/end continuity at the polygon level.
func ValidateClosed(c Contour) error {
	if !c.Closed {
		return fmt.Errorf("contour is not marked closed")
	}
	if len(c.Points) < 4 {
		return fmt.Errorf("contour has fewer than four flattened points")
	}
	if !almostSame(c.Points[0].Position, c.Points[len(c.Points)-1].Position) {
		return fmt.Errorf("flattened contour does not close")
	}
	if c.Length <= geom.Epsilon {
		return fmt.Errorf("contour has zero arc length")
	}
	return nil
}

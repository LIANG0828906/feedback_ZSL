// Package model defines the deterministic glyph sample and truth formats.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"glyphbrush/internal/geom"
)

const (
	SampleSchema = "glyphbrush.samples/v1"
	TruthSchema  = "glyphbrush.truth/v1"
)

type SegmentType string

const (
	Quadratic SegmentType = "quadratic"
	Cubic     SegmentType = "cubic"
)

// Point is a quantized integer glyph coordinate.
type Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

func (p Point) F() geom.Point { return geom.Point{X: float64(p.X), Y: float64(p.Y)} }

type Bounds struct {
	MinX int64 `json:"minX"`
	MinY int64 `json:"minY"`
	MaxX int64 `json:"maxX"`
	MaxY int64 `json:"maxY"`
}

func (b Bounds) F() geom.Bounds {
	return geom.Bounds{MinX: float64(b.MinX), MinY: float64(b.MinY), MaxX: float64(b.MaxX), MaxY: float64(b.MaxY)}
}

type CoordinateRange struct {
	MinX int64 `json:"minX"`
	MinY int64 `json:"minY"`
	MaxX int64 `json:"maxX"`
	MaxY int64 `json:"maxY"`
}

func (r CoordinateRange) Contains(p Point) bool {
	return p.X >= r.MinX && p.X <= r.MaxX && p.Y >= r.MinY && p.Y <= r.MaxY
}

type DensityRange struct {
	MinPPM int `json:"minPpm"`
	MaxPPM int `json:"maxPpm"`
}

type Segment struct {
	Type    SegmentType `json:"type"`
	Start   Point       `json:"start"`
	Control []Point     `json:"control"`
	End     Point       `json:"end"`
}

type Contour struct {
	Index    int       `json:"index"`
	Closed   bool      `json:"closed"`
	Segments []Segment `json:"segments"`
}

type Glyph struct {
	ID           string    `json:"id"`
	Baseline     int64     `json:"baseline"`
	AdvanceWidth int64     `json:"advanceWidth"`
	Bounds       Bounds    `json:"bounds"`
	Contours     []Contour `json:"contours"`
}

func (g Glyph) Hash() string {
	raw, _ := json.Marshal(g)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type Document struct {
	SchemaVersion   string          `json:"schemaVersion"`
	Generator       string          `json:"generator"`
	Seed            int64           `json:"seed"`
	Quantization    int64           `json:"quantization"`
	UnitsPerEm      int64           `json:"unitsPerEm"`
	CoordinateRange CoordinateRange `json:"coordinateRange"`
	DensityRange    DensityRange    `json:"densityRange"`
	Glyphs          []Glyph         `json:"glyphs"`
}

func (d Document) Glyph(id string) (Glyph, bool) {
	for _, g := range d.Glyphs {
		if g.ID == id {
			return g, true
		}
	}
	return Glyph{}, false
}

type CurveSample struct {
	ContourIndex int     `json:"contourIndex"`
	SegmentIndex int     `json:"segmentIndex"`
	T            float64 `json:"t"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
}

type ContourTruth struct {
	Index int     `json:"index"`
	Area  float64 `json:"area"`
}

type GlyphTruth struct {
	ID          string         `json:"id"`
	Bounds      Bounds         `json:"bounds"`
	ClosedArea  float64        `json:"closedArea"`
	Contours    []ContourTruth `json:"contours"`
	Samples     []CurveSample  `json:"samples"`
	ContentHash string         `json:"contentHash"`
}

type TruthDocument struct {
	SchemaVersion string       `json:"schemaVersion"`
	Generator     string       `json:"generator"`
	Seed          int64        `json:"seed"`
	Glyphs        []GlyphTruth `json:"glyphs"`
}

func (d TruthDocument) Glyph(id string) (GlyphTruth, bool) {
	for _, g := range d.Glyphs {
		if g.ID == id {
			return g, true
		}
	}
	return GlyphTruth{}, false
}

// SegmentCurve evaluates a source segment at t.
func SegmentCurve(s Segment, t float64) (geom.Point, error) {
	p0, pN := s.Start.F(), s.End.F()
	switch s.Type {
	case Quadratic:
		if len(s.Control) != 1 {
			return geom.Point{}, fmt.Errorf("quadratic requires exactly one control point")
		}
		p1 := s.Control[0].F()
		u := 1 - t
		return geom.Point{
			X: u*u*p0.X + 2*u*t*p1.X + t*t*pN.X,
			Y: u*u*p0.Y + 2*u*t*p1.Y + t*t*pN.Y,
		}, nil
	case Cubic:
		if len(s.Control) != 2 {
			return geom.Point{}, fmt.Errorf("cubic requires exactly two control points")
		}
		p1, p2 := s.Control[0].F(), s.Control[1].F()
		u := 1 - t
		return geom.Point{
			X: u*u*u*p0.X + 3*u*u*t*p1.X + 3*u*t*t*p2.X + t*t*t*pN.X,
			Y: u*u*u*p0.Y + 3*u*u*t*p1.Y + 3*u*t*t*p2.Y + t*t*t*pN.Y,
		}, nil
	default:
		return geom.Point{}, fmt.Errorf("unsupported curve type %q", s.Type)
	}
}

// FlattenSegment returns points after the start point, including the end.
func FlattenSegment(s Segment, tolerance float64) ([]geom.Point, error) {
	p0, pN := s.Start.F(), s.End.F()
	switch s.Type {
	case Quadratic:
		if len(s.Control) != 1 {
			return nil, fmt.Errorf("quadratic requires exactly one control point")
		}
		return geom.FlattenQuad(p0, s.Control[0].F(), pN, tolerance), nil
	case Cubic:
		if len(s.Control) != 2 {
			return nil, fmt.Errorf("cubic requires exactly two control points")
		}
		return geom.FlattenCubic(p0, s.Control[0].F(), s.Control[1].F(), pN, tolerance), nil
	default:
		return nil, fmt.Errorf("unsupported curve type %q", s.Type)
	}
}

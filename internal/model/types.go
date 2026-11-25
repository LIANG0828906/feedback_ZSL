package model

import "fmt"

// AlgorithmVersion is bumped whenever rendering semantics change. Checkpoints
// carrying a different version are discarded wholesale on recovery.
const AlgorithmVersion = "stroke-forge/1.0.0"

// SchemaVersion of the deterministic sample document.
const SchemaVersion = 1

// Point uses fixed-point integer font units (1/64 em by default).
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// FPoint is a flattened, sub-pixel point in the same font-unit space.
type FPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Polygon is a simple (possibly convex) polygon in font units.
type Polygon struct {
	Points []FPoint `json:"points"`
}

const (
	SegQuadratic = "quadratic"
	SegCubic     = "cubic"
)

// Segment is a single Bezier span of a closed contour.
type Segment struct {
	ID  string `json:"id"`
	Typ string `json:"type"`
	P0  Point  `json:"p0"`
	C1  Point  `json:"c1,omitempty"`
	C2  Point  `json:"c2,omitempty"`
	P1  Point  `json:"p1"`
}

// Contour is an ordered, closed chain of Bezier segments.
type Contour struct {
	ID       string    `json:"id"`
	Closed   bool      `json:"closed"`
	Segments []Segment `json:"segments"`
}

// StrokeSegment partitions one contour by arc-length ratio. Upstream lists
// stroke segment IDs (within the same glyph) that must be committed first.
type StrokeSegment struct {
	ID         int     `json:"id"`
	ContourID  string  `json:"contour_id"`
	StartRatio float64 `json:"start_ratio"`
	EndRatio   float64 `json:"end_ratio"`
	Upstream   []int   `json:"upstream"`
}

// Glyph bundles contours with their stroke-segment partition.
type Glyph struct {
	ID             string          `json:"id"`
	Contours       []Contour       `json:"contours"`
	StrokeSegments []StrokeSegment `json:"stroke_segments"`
}

// Quantization declares the allowed fixed-point coordinate envelope.
type Quantization struct {
	Min  int    `json:"min"`
	Max  int    `json:"max"`
	Step int    `json:"step"`
	Unit string `json:"unit"`
}

// StrokeParams is the versioned parameter snapshot consumed by rendering.
type StrokeParams struct {
	Width       float64 `json:"width"`
	Join        string  `json:"join"`
	Cap         string  `json:"cap"`
	Spacing     float64 `json:"spacing"`
	Tolerance   float64 `json:"tolerance"`
	Scale       float64 `json:"scale"`
	BBoxPadding float64 `json:"bbox_padding"`
}

// BBox in font units (may be fractional because of flattened control points).
type BBox struct {
	MinX float64 `json:"min_x"`
	MinY float64 `json:"min_y"`
	MaxX float64 `json:"max_x"`
	MaxY float64 `json:"max_y"`
}

// SamplePointTruth is one density sample located on the skeleton.
type SamplePointTruth struct {
	StrokeSegmentID int    `json:"stroke_segment_id"`
	Point           FPoint `json:"point"`
	Density         int    `json:"density"`
}

// GlyphTruth is the recorded analytical truth for one glyph.
type GlyphTruth struct {
	GlyphID       string             `json:"glyph_id"`
	SamplePoints  []SamplePointTruth `json:"sample_points"`
	ClosedArea    float64            `json:"closed_area"`
	BBox          BBox               `json:"bbox"`
	MaskWidth     int                `json:"mask_width"`
	MaskHeight    int                `json:"mask_height"`
	StrokeDensity int                `json:"stroke_density"`
}

// Truth is the top-level truth block.
type Truth struct {
	Glyphs []GlyphTruth `json:"glyphs"`
}

// Document is the full deterministic sample.
type Document struct {
	SchemaVersion int          `json:"schema_version"`
	Seed          int64        `json:"seed"`
	Quantization  Quantization `json:"quantization"`
	StrokeParams  StrokeParams `json:"stroke_params"`
	Glyphs        []Glyph      `json:"glyphs"`
	Truth         Truth        `json:"truth"`
}

// CodedError carries a stable machine code plus a human message and a
// locate-able glyph/segment pair.
type CodedError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	GlyphID   string `json:"glyph_id,omitempty"`
	SegmentID int    `json:"segment_id,omitempty"`
	Field     string `json:"field,omitempty"`
}

func (e *CodedError) Error() string {
	loc := e.GlyphID
	if loc != "" {
		loc = fmt.Sprintf("%s#%d", e.GlyphID, e.SegmentID)
	}
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (field=%s, loc=%s)", e.Code, e.Message, e.Field, loc)
	}
	return fmt.Sprintf("%s: %s (loc=%s)", e.Code, e.Message, loc)
}

// Stable error codes required by the specification.
const (
	EDecode = "E_DECODE"
	ESchema = "E_SCHEMA"
	EPara   = "E_PARA"
	EBound  = "E_BOUND"
	EDens   = "E_DENS"
	EDiff   = "E_DIFF"
	EState  = "E_STATE"
	ESelf   = "E_SELF"
	EZlen   = "E_ZLEN"
	EStage  = "E_STAGE"
)

// NewError builds a CodedError.
func NewError(code, msg string) *CodedError {
	return &CodedError{Code: code, Message: msg, SegmentID: -1}
}

// WithLocation attaches glyph/segment coordinates.
func (e *CodedError) WithLocation(glyph string, seg int) *CodedError {
	e.GlyphID = glyph
	e.SegmentID = seg
	return e
}

// WithField names the offending schema/parameter field.
func (e *CodedError) WithField(field string) *CodedError {
	e.Field = field
	return e
}

// AttachLocation enriches any error with glyph/segment location when it is a
// CodedError; other errors are wrapped into E_STAGE.
func AttachLocation(err error, glyph string, seg int) error {
	if err == nil {
		return nil
	}
	if ce, ok := err.(*CodedError); ok {
		return ce.WithLocation(glyph, seg)
	}
	return NewError(EStage, err.Error()).WithLocation(glyph, seg)
}

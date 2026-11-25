package maskqa

import (
	"fmt"
	"image"
)

const (
	SchemaV1 = "v1"
	SchemaV2 = "v2"
	SchemaV3 = "v3"

	WeightPermilleScale = 1000
	WeightBasisScale    = 10000

	DefaultSeed             int64 = 20261126
	AlgorithmVersion              = "maskqa-algo-2026q4-1"
	ParameterVersion              = "raster-coverage-v1"
	DefaultTileSize               = 32
	DefaultCutAlphaBPS            = 1
	DefaultHardnessPermille       = 800

	RoundingFloor   = "floor_to_zero"
	RoundingNearest = "round_half_up"
)

// Point is an integer pixel coordinate. Coordinates use image convention: X right, Y down.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Rect is a half-open integer rectangle.
type Rect struct {
	X0 int `json:"x0"`
	Y0 int `json:"y0"`
	X1 int `json:"x1"`
	Y1 int `json:"y1"`
}

func (r Rect) Dx() int     { return r.X1 - r.X0 }
func (r Rect) Dy() int     { return r.Y1 - r.Y0 }
func (r Rect) Empty() bool { return r.X1 <= r.X0 || r.Y1 <= r.Y0 }

func (r Rect) Contains(p Point) bool {
	return p.X >= r.X0 && p.X < r.X1 && p.Y >= r.Y0 && p.Y < r.Y1
}

func (r Rect) Intersect(o Rect) Rect {
	if r.X0 < o.X0 {
		r.X0 = o.X0
	}
	if r.Y0 < o.Y0 {
		r.Y0 = o.Y0
	}
	if r.X1 > o.X1 {
		r.X1 = o.X1
	}
	if r.Y1 > o.Y1 {
		r.Y1 = o.Y1
	}
	if r.Empty() {
		return Rect{}
	}
	return r
}

// Mask is a deterministic binary foreground mask.
type Mask struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Pix    []byte `json:"pix"`
}

func NewMask(width, height int) *Mask {
	return &Mask{Width: width, Height: height, Pix: make([]byte, width*height)}
}

func MaskFromImage(m image.Image) *Mask {
	b := m.Bounds()
	out := NewMask(b.Dx(), b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := m.At(x, y).RGBA()
			_ = r
			_ = g
			_ = bl
			if a>>8 > 0 {
				out.Set(x-b.Min.X, y-b.Min.Y, a>>8 > 127)
			}
		}
	}
	return out
}

func (m *Mask) index(x, y int) int { return y*m.Width + x }

func (m *Mask) InBounds(x, y int) bool {
	return x >= 0 && y >= 0 && x < m.Width && y < m.Height
}

func (m *Mask) Get(x, y int) byte {
	if !m.InBounds(x, y) {
		return 0
	}
	return m.Pix[m.index(x, y)]
}

func (m *Mask) Set(x, y int, foreground bool) {
	if !m.InBounds(x, y) {
		return
	}
	if foreground {
		m.Pix[m.index(x, y)] = 1
	} else {
		m.Pix[m.index(x, y)] = 0
	}
}

func (m *Mask) Clone() *Mask {
	pix := make([]byte, len(m.Pix))
	copy(pix, m.Pix)
	return &Mask{Width: m.Width, Height: m.Height, Pix: pix}
}

func (m *Mask) Equal(o *Mask) bool {
	if m == nil || o == nil || m.Width != o.Width || m.Height != o.Height || len(m.Pix) != len(o.Pix) {
		return false
	}
	for i := range m.Pix {
		if m.Pix[i] != o.Pix[i] {
			return false
		}
	}
	return true
}

func (m *Mask) Rect() Rect { return Rect{X1: m.Width, Y1: m.Height} }

// CoordinateEncoding records how a source schema expressed coordinates.
type CoordinateEncoding struct {
	Mode          string `json:"mode"` // absolute_px or stroke_index_offset
	IndexStepX    int    `json:"index_step_x,omitempty"`
	IndexStepY    int    `json:"index_step_y,omitempty"`
	OffsetQuantum int    `json:"offset_quantum,omitempty"`
}

// Stroke is the schema-independent construction operation.
type Stroke struct {
	Index             int     `json:"index"`
	ID                string  `json:"id"`
	Priority          int     `json:"priority"`
	Start             Point   `json:"start"`
	End               Point   `json:"end"`
	WeightBPS         int     `json:"weight_bps"`
	HardnessPermille  int     `json:"hardness_permille"`
	HardnessPresent   bool    `json:"hardness_present"`
	DirectionCategory string  `json:"direction_category"`
	DeclaredAngleDeg  float64 `json:"declared_angle_deg"`
}

// Project is the canonical in-memory icon engineering package.
type Project struct {
	SourceVersion  string             `json:"source_version"`
	ProjectID      string             `json:"project_id"`
	Seed           int64              `json:"seed"`
	Width          int                `json:"width"`
	Height         int                `json:"height"`
	BrushWidthPx   int                `json:"brush_width_px"`
	CutAlphaBPS    int                `json:"cut_alpha_bps"`
	RoundingMode   string             `json:"rounding_mode"`
	CoordEncoding  CoordinateEncoding `json:"coord_encoding"`
	WeightScale    string             `json:"weight_scale"`
	DirectionTable string             `json:"direction_table"`
	ImagePNG       []byte             `json:"image_png,omitempty"`
	Candidate      *Mask              `json:"candidate"`
	Strokes        []Stroke           `json:"strokes"`
	Residuals      []Truncation       `json:"residuals,omitempty"`
	ResidualDigest string             `json:"residual_digest,omitempty"`
}

func (p *Project) Rect() Rect { return Rect{X1: p.Width, Y1: p.Height} }

// Tile is a rectangular work partition.
type Tile struct {
	ID   int  `json:"id"`
	Rect Rect `json:"rect"`
}

func BuildTiles(width, height, tileSize int) []Tile {
	if tileSize <= 0 {
		tileSize = DefaultTileSize
	}
	var tiles []Tile
	id := 0
	for y0 := 0; y0 < height; y0 += tileSize {
		for x0 := 0; x0 < width; x0 += tileSize {
			r := Rect{X0: x0, Y0: y0, X1: minInt(x0+tileSize, width), Y1: minInt(y0+tileSize, height)}
			tiles = append(tiles, Tile{ID: id, Rect: r})
			id++
		}
	}
	return tiles
}

func TileAt(tiles []Tile, x, y int) (Tile, bool) {
	for _, t := range tiles {
		if t.Rect.Contains(Point{X: x, Y: y}) {
			return t, true
		}
	}
	return Tile{}, false
}

// DomainMetric is both the analytic truth record and a measured topology report row.
type DomainMetric struct {
	DomainID    int     `json:"domain_id"`
	Label       string  `json:"label"`
	Area        int     `json:"area"`
	CentroidX   float64 `json:"centroid_x"`
	CentroidY   float64 `json:"centroid_y"`
	MinX        int     `json:"min_x"`
	MinY        int     `json:"min_y"`
	MaxX        int     `json:"max_x"`
	MaxY        int     `json:"max_y"`
	Holes       int     `json:"holes"`
	RingWidthPx float64 `json:"ring_width_px"`
}

// BoundarySegment is one analytically declared boundary/direction observation unit.
type BoundarySegment struct {
	SegmentID        string  `json:"segment_id"`
	DomainID         int     `json:"domain_id"`
	StrokeIndex      int     `json:"stroke_index"`
	Side             string  `json:"side"`
	Probe            Point   `json:"probe"`
	ExpectedCategory string  `json:"expected_category"`
	ExpectedAngleDeg float64 `json:"expected_angle_deg"`
}

// TruthManifest is the only accepted precision oracle. It is constructed by the generator.
type TruthManifest struct {
	ProjectID string            `json:"project_id"`
	Seed      int64             `json:"seed"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Mask      *Mask             `json:"mask"`
	Domains   []DomainMetric    `json:"domains"`
	Segments  []BoundarySegment `json:"segments"`
}

func (t *TruthManifest) Domain(id int) (DomainMetric, bool) {
	for _, d := range t.Domains {
		if d.DomainID == id {
			return d, true
		}
	}
	return DomainMetric{}, false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func formatField(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return fmt.Sprintf("%s.%s", prefix, name)
}

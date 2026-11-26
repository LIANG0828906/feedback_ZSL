package maskqa

import (
	"fmt"
	"image"
)

const (
	SchemaV1 = "maskqa.project/v1"
	SchemaV2 = "maskqa.project/v2"
	SchemaV3 = "maskqa.project/v3"

	DefaultTileSize = 16

	// DefaultHardnessPermyriad is used when an old schema has no hardness.
	DefaultHardnessPermyriad = 6500
	// DefaultDirection is used when an old schema has no direction encoding.
	DefaultDirection DirectionClass = "unknown"
)

// DirectionClass is the broad travel/topology direction category.
type DirectionClass string

const (
	DirUnknown    DirectionClass = "unknown"
	DirHorizontal DirectionClass = "horizontal"
	DirVertical   DirectionClass = "vertical"
	DirDownRight  DirectionClass = "diag_down_right"
	DirDownLeft   DirectionClass = "diag_down_left"
	DirClosedRing DirectionClass = "closed_ring"
)

// Point is an integer pixel coordinate.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Rect is a half-open integer pixel rectangle.
type Rect struct {
	MinX int `json:"min_x"`
	MinY int `json:"min_y"`
	MaxX int `json:"max_x"`
	MaxY int `json:"max_y"`
}

func (r Rect) Width() int  { return r.MaxX - r.MinX }
func (r Rect) Height() int { return r.MaxY - r.MinY }

func (r Rect) Contains(x, y int) bool {
	return x >= r.MinX && x < r.MaxX && y >= r.MinY && y < r.MaxY
}

func (r Rect) Intersect(other Rect) Rect {
	if r.MinX < other.MinX {
		r.MinX = other.MinX
	}
	if r.MinY < other.MinY {
		r.MinY = other.MinY
	}
	if r.MaxX > other.MaxX {
		r.MaxX = other.MaxX
	}
	if r.MaxY > other.MaxY {
		r.MaxY = other.MaxY
	}
	if r.MinX > r.MaxX {
		r.MaxX = r.MinX
	}
	if r.MinY > r.MaxY {
		r.MaxY = r.MinY
	}
	return r
}

func (r Rect) Empty() bool { return r.MaxX <= r.MinX || r.MaxY <= r.MinY }

// Stroke is the schema-independent construction record.
type Stroke struct {
	ID                string         `json:"id"`
	Seq               int            `json:"seq"`
	Group             string         `json:"group"`
	Priority          int            `json:"priority"`
	Start             Point          `json:"start"`
	End               Point          `json:"end"`
	WidthPx           int            `json:"width_px"`
	HardnessPermyriad int            `json:"hardness_permyriad"`
	WeightPermyriad   int            `json:"weight_permyriad"`
	Direction         DirectionClass `json:"direction"`
	DirectionCode     int            `json:"direction_code,omitempty"`
	HasHardness       bool           `json:"has_hardness"`
	HasDirection      bool           `json:"has_direction"`
}

// DomainTruth is generator-owned ground truth for one foreground component.
type DomainTruth struct {
	DomainNo       int            `json:"domain_no"`
	CentroidXMilli int            `json:"centroid_x_milli"`
	CentroidYMilli int            `json:"centroid_y_milli"`
	Area           int            `json:"area"`
	MinX           int            `json:"min_x"`
	MinY           int            `json:"min_y"`
	MaxX           int            `json:"max_x"`
	MaxY           int            `json:"max_y"`
	Holes          int            `json:"holes"`
	BandWidthMilli int            `json:"band_width_milli"`
	Direction      DirectionClass `json:"direction"`
	DirectionCode  int            `json:"direction_code"`
	StrokeIDs      []string       `json:"stroke_ids"`
}

// Tolerances declares QC comparison windows.
type Tolerances struct {
	AreaRelPermyriad        int `json:"area_rel_permyriad"`
	CentroidMilliPx         int `json:"centroid_milli_px"`
	BBoxPx                  int `json:"bbox_px"`
	HoleCount               int `json:"hole_count"`
	BandWidthMilliPx        int `json:"band_width_milli_px"`
	DirectionScorePermyriad int `json:"direction_score_permyriad"`
}

func DefaultTolerances() Tolerances {
	return Tolerances{
		AreaRelPermyriad:        1200,
		CentroidMilliPx:         1800,
		BBoxPx:                  2,
		HoleCount:               0,
		BandWidthMilliPx:        3000,
		DirectionScorePermyriad: 6000,
	}
}

// FieldTrace records a source-field to internal-field conversion.
type FieldTrace struct {
	SourceVersion string `json:"source_version"`
	Path          string `json:"path"`
	SourceField   string `json:"source_field"`
	TargetField   string `json:"target_field"`
	Rule          string `json:"rule"`
	OldValue      any    `json:"old_value,omitempty"`
	NewValue      any    `json:"new_value,omitempty"`
	DefaultValue  any    `json:"default_value,omitempty"`
	Note          string `json:"note,omitempty"`
}

// Project is the unified in-memory engineering model.
type Project struct {
	ID            string             `json:"id"`
	Seed          int64              `json:"seed"`
	Width         int                `json:"width"`
	Height        int                `json:"height"`
	TileSize      int                `json:"tile_size"`
	Icon          *image.RGBA        `json:"-"`
	Candidate     *image.Gray        `json:"-"`
	Reference     *image.Gray        `json:"-"`
	Strokes       []Stroke           `json:"strokes"`
	Truth         []DomainTruth      `json:"truth"`
	Tolerances    Tolerances         `json:"tolerances"`
	SourceVersion string             `json:"source_version"`
	Traces        []FieldTrace       `json:"traces,omitempty"`
	Conversions   []TruncationRecord `json:"conversions,omitempty"`
}

func (p *Project) tileSize() int {
	if p != nil && p.TileSize > 0 {
		return p.TileSize
	}
	return DefaultTileSize
}

// TileCoord identifies a chunk.
type TileCoord struct {
	TX int `json:"tx"`
	TY int `json:"ty"`
}

func (c TileCoord) String() string { return fmt.Sprintf("%d,%d", c.TX, c.TY) }

// Tile is a pixel rectangle aligned to the chunk grid.
type Tile struct {
	Coord TileCoord `json:"coord"`
	Rect
	PixelCount int `json:"pixel_count"`
}

// Tiles returns all deterministic row-major chunks.
func Tiles(width, height, size int) []Tile {
	if size <= 0 {
		size = DefaultTileSize
	}
	var tiles []Tile
	for ty, y := 0, 0; y < height; ty, y = ty+1, y+size {
		y1 := y + size
		if y1 > height {
			y1 = height
		}
		for tx, x := 0, 0; x < width; tx, x = tx+1, x+size {
			x1 := x + size
			if x1 > width {
				x1 = width
			}
			tiles = append(tiles, Tile{
				Coord:      TileCoord{TX: tx, TY: ty},
				Rect:       Rect{MinX: x, MinY: y, MaxX: x1, MaxY: y1},
				PixelCount: (x1 - x) * (y1 - y),
			})
		}
	}
	return tiles
}

// StrokeBounds returns the affected pixel rectangle including the brush radius.
func StrokeBounds(stroke Stroke) Rect {
	radius := stroke.WidthPx/2 + 2
	minX := minInt(stroke.Start.X, stroke.End.X) - radius
	maxX := maxInt(stroke.Start.X, stroke.End.X) + radius + 1
	minY := minInt(stroke.Start.Y, stroke.End.Y) - radius
	maxY := maxInt(stroke.Start.Y, stroke.End.Y) + radius + 1
	return Rect{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
}

// TilesForRect returns row-major chunks intersecting rect.
func TilesForRect(rect Rect, width, height, size int) []TileCoord {
	all := Tiles(width, height, size)
	var out []TileCoord
	for _, tile := range all {
		if !tile.Rect.Intersect(rect).Empty() {
			out = append(out, tile.Coord)
		}
	}
	return out
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

// ValidateParameters checks declared brush/weight ranges.
func (s Stroke) ValidateParameters(fieldPrefix string) error {
	if s.HardnessPermyriad < 0 || s.HardnessPermyriad > 10000 {
		return paraError(fieldPrefix+".hardness_permyriad", 0, 10000,
			"hardness %d outside declared interval", s.HardnessPermyriad)
	}
	if s.WeightPermyriad < 0 || s.WeightPermyriad > 10000 {
		return paraError(fieldPrefix+".weight_permyriad", 0, 10000,
			"weight %d outside declared permyriad interval", s.WeightPermyriad)
	}
	if s.WidthPx <= 0 || s.WidthPx > 256 {
		return paraError(fieldPrefix+".width_px", 1, 256,
			"brush width %d outside declared interval", s.WidthPx)
	}
	return nil
}

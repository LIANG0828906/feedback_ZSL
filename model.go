package maskqc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	SchemaV1 = "icon-project/v1"
	SchemaV2 = "icon-project/v2"
	SchemaV3 = "icon-project/v3"

	ConversionParamsVersion = "int-conv-v1"
	OrderRulesVersion       = "priority-seq-v1"
	AlgorithmVersion        = "raster-topology-v1"
	CacheVersion            = "cache-key-v1"

	// WeightScale is the internal exact scale: 10000 = 100%.
	WeightScale = 10000
	// HardnessScale is internal permille hardness.
	HardnessScale = 1000
)

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Rect struct {
	MinX int `json:"min_x"`
	MinY int `json:"min_y"`
	MaxX int `json:"max_x"` // exclusive
	MaxY int `json:"max_y"` // exclusive
}

func (r Rect) Width() int  { return r.MaxX - r.MinX }
func (r Rect) Height() int { return r.MaxY - r.MinY }
func (r Rect) Contains(p Point) bool {
	return p.X >= r.MinX && p.X < r.MaxX && p.Y >= r.MinY && p.Y < r.MaxY
}
func (r Rect) Intersect(o Rect) Rect {
	r.MinX = maxInt(r.MinX, o.MinX)
	r.MinY = maxInt(r.MinY, o.MinY)
	r.MaxX = minInt(r.MaxX, o.MaxX)
	r.MaxY = minInt(r.MaxY, o.MaxY)
	if r.MaxX < r.MinX {
		r.MaxX = r.MinX
	}
	if r.MaxY < r.MinY {
		r.MaxY = r.MinY
	}
	return r
}
func (r Rect) Empty() bool { return r.MaxX <= r.MinX || r.MaxY <= r.MinY }

type DirectionClass string

const (
	DirAlong   DirectionClass = "along"
	DirCross   DirectionClass = "cross"
	DirMixed   DirectionClass = "mixed"
	DirUnknown DirectionClass = "unknown"
)

// Stroke is the schema-independent construction record.
type Stroke struct {
	ID       string `json:"id"`
	Seq      int    `json:"seq"`
	Priority int    `json:"priority"`
	Group    string `json:"group,omitempty"`

	Start Point `json:"start"`
	End   Point `json:"end"`

	Radius            int            `json:"radius"`
	HardnessPermille  int            `json:"hardness_permille"`
	WeightPermyriad   int            `json:"weight_permyriad"`
	DeclaredDirection DirectionClass `json:"declared_direction"`
	CompositionOrder  int            `json:"composition_order"`

	SourceSchema string `json:"source_schema"`
}

func (s Stroke) ContentHash() string {
	b, _ := json.Marshal(struct {
		Start, End Point
		Seq        int
		Priority   int
		Group      string
		Radius     int
		Hardness   int
		Weight     int
		Dir        DirectionClass
		Order      int
	}{s.Start, s.End, s.Seq, s.Priority, s.Group, s.Radius, s.HardnessPermille, s.WeightPermyriad, s.DeclaredDirection, s.CompositionOrder})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

type Mask struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Alpha  []byte `json:"alpha"`
}

func NewMask(w, h int) *Mask {
	return &Mask{Width: w, Height: h, Alpha: make([]byte, w*h)}
}

func (m *Mask) Validate() error {
	if m.Width <= 0 || m.Height <= 0 {
		return newError(EPara, "mask", "mask dimensions must be positive")
	}
	if len(m.Alpha) != m.Width*m.Height {
		return newError(EPara, "mask.alpha", "length %d does not match %d*%d", len(m.Alpha), m.Width, m.Height)
	}
	return nil
}

func (m *Mask) Clone() *Mask {
	a := make([]byte, len(m.Alpha))
	copy(a, m.Alpha)
	return &Mask{Width: m.Width, Height: m.Height, Alpha: a}
}

func (m *Mask) At(x, y int) byte { return m.Alpha[y*m.Width+x] }

func (m *Mask) Hash() string {
	sum := sha256.Sum256(m.Alpha)
	return hex.EncodeToString(sum[:])
}

type TileTruth struct {
	ID     string `json:"id"`
	IconID string `json:"icon_id"`
	Rect   Rect   `json:"rect"`
	Hash   string `json:"hash"`
}

type Icon struct {
	ID         string      `json:"id"`
	Width      int         `json:"width"`
	Height     int         `json:"height"`
	TileWidth  int         `json:"tile_width"`
	TileHeight int         `json:"tile_height"`
	Candidate  *Mask       `json:"-"`
	Truth      *Mask       `json:"-"`
	TruthTiles []TileTruth `json:"truth_tiles"`
	Strokes    []*Stroke   `json:"strokes"`
}

func (ic *Icon) TileIDs() []string {
	cols := ic.Width / ic.TileWidth
	rows := ic.Height / ic.TileHeight
	out := make([]string, 0, cols*rows)
	for ty := 0; ty < rows; ty++ {
		for tx := 0; tx < cols; tx++ {
			out = append(out, TileID(ic.ID, tx, ty))
		}
	}
	return out
}

func TileID(iconID string, tx, ty int) string {
	return fmt.Sprintf("%s:t%02d_%02d", iconID, tx, ty)
}

func TileCoords(tileID string) (tx, ty int, ok bool) {
	var a, b int
	// suffix is tXX_YY
	for i := len(tileID) - 1; i >= 0; i-- {
		if tileID[i] == '_' {
			if _, err := fmt.Sscanf(tileID[i+1:], "%02d", &b); err != nil {
				return 0, 0, false
			}
			if i >= 3 && tileID[i-3] == 't' {
				if _, err := fmt.Sscanf(tileID[i-2:i], "%02d", &a); err != nil {
					return 0, 0, false
				}
				return a, b, true
			}
		}
	}
	_ = a
	return 0, 0, false
}

type Project struct {
	Schema       string       `json:"schema"`
	Seed         int64        `json:"seed"`
	Name         string       `json:"name"`
	Icons        []*Icon      `json:"icons"`
	SourceRaw    []byte       `json:"-"`
	ConvertTrace []FieldTrace `json:"convert_trace,omitempty"`
}

func (p *Project) Icon(id string) *Icon {
	for _, ic := range p.Icons {
		if ic.ID == id {
			return ic
		}
	}
	return nil
}

func (p *Project) SortedStrokes(iconID string) []*Stroke {
	ic := p.Icon(iconID)
	if ic == nil {
		return nil
	}
	out := make([]*Stroke, len(ic.Strokes))
	copy(out, ic.Strokes)
	SortStrokes(out)
	return out
}

// SortStrokes defines the only legal synthesis order: process priority first,
// then pen sequence, then stable ID for deterministic tie-breaking.
func SortStrokes(strokes []*Stroke) {
	sort.SliceStable(strokes, func(i, j int) bool {
		a, b := strokes[i], strokes[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		return a.ID < b.ID
	})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

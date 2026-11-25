package maskqc

import "sort"

const (
	AlgorithmVersion = "raster-int-v1/alpha-over-v1/topo-4-v1/dir-normal-v1"
	OrderVersion     = "priority-asc+seq-asc-v1"
	ParamVersion     = "weight-bp10000-hardness-pm1000-radius-v1"
	CacheVersion     = "cache-key-v1"

	BasisPermille       = 1000
	BasisTenThousand    = 10000
	DefaultTileSize     = 8
	ForegroundThreshold = 128
)

var AllowedWeightsBP = []int{1000, 2500, 5000, 7500, 10000}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type BBox struct {
	MinX int `json:"minX"`
	MinY int `json:"minY"`
	MaxX int `json:"maxX"`
	MaxY int `json:"maxY"`
}

func (b BBox) Width() int  { return b.MaxX - b.MinX + 1 }
func (b BBox) Height() int { return b.MaxY - b.MinY + 1 }

func (b BBox) Contains(p Point) bool {
	return p.X >= b.MinX && p.X <= b.MaxX && p.Y >= b.MinY && p.Y <= b.MaxY
}

func (b BBox) Intersect(other BBox) (BBox, bool) {
	out := BBox{
		MinX: maxInt(b.MinX, other.MinX),
		MinY: maxInt(b.MinY, other.MinY),
		MaxX: minInt(b.MaxX, other.MaxX),
		MaxY: minInt(b.MaxY, other.MaxY),
	}
	return out, out.MinX <= out.MaxX && out.MinY <= out.MaxY
}

func (b BBox) Union(other BBox) BBox {
	return BBox{
		MinX: minInt(b.MinX, other.MinX),
		MinY: minInt(b.MinY, other.MinY),
		MaxX: maxInt(b.MaxX, other.MaxX),
		MaxY: maxInt(b.MaxY, other.MaxY),
	}
}

type Image struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	RGBA   []byte `json:"-"`
}

type Mask struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Alpha  []byte `json:"alpha"`
}

func NewMask(width, height int) *Mask {
	return &Mask{Width: width, Height: height, Alpha: make([]byte, width*height)}
}

func (m *Mask) index(x, y int) int { return y*m.Width + x }

func (m *Mask) InBounds(x, y int) bool {
	return m != nil && x >= 0 && y >= 0 && x < m.Width && y < m.Height
}

func (m *Mask) Get(x, y int) byte {
	if !m.InBounds(x, y) {
		return 0
	}
	return m.Alpha[m.index(x, y)]
}

func (m *Mask) Set(x, y int, v byte) {
	if m.InBounds(x, y) {
		m.Alpha[m.index(x, y)] = v
	}
}

func (m *Mask) Clone() *Mask {
	if m == nil {
		return nil
	}
	cp := NewMask(m.Width, m.Height)
	copy(cp.Alpha, m.Alpha)
	return cp
}

func (m *Mask) Binary(threshold byte) *Mask {
	out := NewMask(m.Width, m.Height)
	for i, v := range m.Alpha {
		if v >= threshold {
			out.Alpha[i] = 255
		}
	}
	return out
}

func (m *Mask) EqualAlpha(other *Mask) bool {
	if m == nil || other == nil || m.Width != other.Width || m.Height != other.Height {
		return false
	}
	return bytesEqual(m.Alpha, other.Alpha)
}

func (m *Mask) EqualBinary(other *Mask, threshold byte) bool {
	if m == nil || other == nil || m.Width != other.Width || m.Height != other.Height {
		return false
	}
	for i := range m.Alpha {
		a := m.Alpha[i] >= threshold
		b := other.Alpha[i] >= threshold
		if a != b {
			return false
		}
	}
	return true
}

type DirectionClass string

const (
	DirForward DirectionClass = "forward"
	DirReverse DirectionClass = "reverse"
	DirLeft    DirectionClass = "perp_left"
	DirRight   DirectionClass = "perp_right"
	DirUnknown DirectionClass = "unknown"
)

type Stroke struct {
	ID                string         `json:"id"`
	Seq               int            `json:"seq"`
	Priority          int            `json:"priority"`
	Op                string         `json:"op"`
	Role              string         `json:"role"`
	Start             Point          `json:"start"`
	End               Point          `json:"end"`
	WeightBP          int            `json:"weightBP"`
	HardnessPermille  int            `json:"hardnessPermille"`
	Radius            int            `json:"radius"`
	DeclaredDirection DirectionClass `json:"declaredDirection,omitempty"`
}

const (
	StrokeOpOver  = "over"
	StrokeOpErase = "erase"
	RolePaint     = "paint"
	RoleRepair    = "repair"
)

func (s Stroke) Bounds() BBox {
	r := s.Radius
	return BBox{
		MinX: minInt(s.Start.X, s.End.X) - r,
		MinY: minInt(s.Start.Y, s.End.Y) - r,
		MaxX: maxInt(s.Start.X, s.End.X) + r,
		MaxY: maxInt(s.Start.Y, s.End.Y) + r,
	}
}

// OrderedStrokes returns the unique composition order. Lower Priority values
// are construction processes executed first; Seq is the stable tie breaker.
func OrderedStrokes(strokes []Stroke) []Stroke {
	out := append([]Stroke(nil), strokes...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

type DomainTruth struct {
	DomainID        int   `json:"domainId"`
	CentroidX1000   int64 `json:"centroidX1000"`
	CentroidY1000   int64 `json:"centroidY1000"`
	Area            int   `json:"area"`
	BBox            BBox  `json:"bbox"`
	Holes           int   `json:"holes"`
	BandWidthPixels int   `json:"bandWidthPixels"`
	HoleTolerance   int   `json:"holeTolerance"`
	BandTolerancePM int   `json:"bandTolerancePermille"`
}

type SegmentTruth struct {
	SegmentID  string         `json:"segmentId"`
	DomainID   int            `json:"domainId"`
	StrokeID   string         `json:"strokeId"`
	Class      DirectionClass `json:"class"`
	NormalX    int            `json:"normalX"`
	NormalY    int            `json:"normalY"`
	PointCount int            `json:"pointCount"`
	BBox       BBox           `json:"bbox"`
}

type Truth struct {
	Reference *Mask          `json:"reference"`
	Domains   []DomainTruth  `json:"domains"`
	Segments  []SegmentTruth `json:"segments"`
}

type Project struct {
	SchemaVersion string            `json:"schemaVersion"`
	ID            string            `json:"id"`
	Seed          int64             `json:"seed"`
	Width         int               `json:"width"`
	Height        int               `json:"height"`
	Image         *Image            `json:"image,omitempty"`
	Candidate     *Mask             `json:"candidate"`
	DefectMasks   map[string]*Mask  `json:"defectMasks,omitempty"`
	Strokes       []Stroke          `json:"strokes"`
	Truth         Truth             `json:"truth"`
	ConvertReport *ConversionReport `json:"convertReport,omitempty"`
	AssetDir      string            `json:"-"`
}

type Tile struct {
	ID     string `json:"id"`
	Row    int    `json:"row"`
	Col    int    `json:"col"`
	Rect   BBox   `json:"rect"`
	Pixels int    `json:"pixels"`
}

func Tiles(width, height, size int) []Tile {
	if size <= 0 {
		size = DefaultTileSize
	}
	var tiles []Tile
	row := 0
	for y0 := 0; y0 < height; y0 += size {
		col := 0
		for x0 := 0; x0 < width; x0 += size {
			r := BBox{MinX: x0, MinY: y0, MaxX: minInt(x0+size-1, width-1), MaxY: minInt(y0+size-1, height-1)}
			tiles = append(tiles, Tile{
				ID:     tileID(row, col),
				Row:    row,
				Col:    col,
				Rect:   r,
				Pixels: r.Width() * r.Height(),
			})
			col++
		}
		row++
	}
	return tiles
}

func tileID(row, col int) string {
	return "r" + pad2(row) + "c" + pad2(col)
}

func pad2(v int) string {
	if v < 10 {
		return "0" + itoa(v)
	}
	return itoa(v)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
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

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

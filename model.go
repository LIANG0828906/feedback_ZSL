package maskqc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	CodeSchema = "E_SCHEMA"
	CodeLossy  = "E_LOSSY"
	CodeOVF    = "E_OVF"
	CodePara   = "E_PARA"
	CodeTop    = "E_TOP"
	CodeState  = "E_STATE"
	CodeSize   = "E_SIZE"
	CodeRange  = "E_RANGE"
	CodeBlock  = "E_BLOCK"
)

const (
	OrderVersion     = "priority-seq-v1"
	ConvertVersion   = "int-convert-v1"
	AlgorithmVersion = "raster-topo-v1"
)

// BizError is the stable error envelope used by domain code and the workbench.
type BizError struct {
	Code     string     `json:"code"`
	Message  string     `json:"message"`
	Path     string     `json:"path,omitempty"`
	ObjectID string     `json:"object_id,omitempty"`
	Field    string     `json:"field,omitempty"`
	ChunkID  int        `json:"chunk_id,omitempty"`
	StrokeID string     `json:"stroke_id,omitempty"`
	Min      int64      `json:"min,omitempty"`
	Max      int64      `json:"max,omitempty"`
	A        int64      `json:"operand_a,omitempty"`
	B        int64      `json:"operand_b,omitempty"`
	Want     string     `json:"want,omitempty"`
	Got      string     `json:"got,omitempty"`
	Details  []Mismatch `json:"details,omitempty"`
	Cause    string     `json:"cause,omitempty"`
}

type Mismatch struct {
	Domain int    `json:"domain"`
	Item   string `json:"item"`
	Want   string `json:"want"`
	Got    string `json:"got"`
}

func (e *BizError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := fmt.Sprintf("%s: %s", e.Code, e.Message)
	if e.Path != "" {
		msg += " path=" + e.Path
	}
	if e.ObjectID != "" {
		msg += " object=" + e.ObjectID
	}
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.ChunkID >= 0 {
		msg += fmt.Sprintf(" chunk=%d", e.ChunkID)
	}
	if e.StrokeID != "" {
		msg += " stroke=" + e.StrokeID
	}
	if len(e.Details) > 0 {
		msg += fmt.Sprintf(" mismatches=%d", len(e.Details))
	}
	return msg
}

func (e *BizError) Unwrap() error { return nil }

func NewError(code, message string) *BizError {
	return &BizError{Code: code, Message: message, ChunkID: -1}
}

func AsBizError(err error) (*BizError, bool) {
	if err == nil {
		return nil, false
	}
	var be *BizError
	if ok := errorAs(err, &be); ok {
		return be, true
	}
	return nil, false
}

func errorAs(err error, target **BizError) bool {
	for err != nil {
		if be, ok := err.(*BizError); ok {
			*target = be
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func IsCode(err error, code string) bool {
	be, ok := AsBizError(err)
	return ok && be.Code == code
}

// Point uses integer pixel-center coordinates.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Stroke struct {
	ID             string   `json:"id"`
	Seq            int      `json:"seq"`
	Priority       int      `json:"priority"`
	Group          string   `json:"group,omitempty"`
	Start          Point    `json:"start"`
	End            Point    `json:"end"`
	Width          int      `json:"width"`
	HardnessPct    int      `json:"hardness_pct"`
	WeightMyriad   int      `json:"weight_myriad"`
	DirectionClass DirClass `json:"direction_class"`
}

type StrokeGroup struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	StrokeIDs []string `json:"stroke_ids"`
}

type DirClass string

const (
	DirTangent      DirClass = "tangent"
	DirNormalAlign  DirClass = "normal_aligned"
	DirNormalOppose DirClass = "normal_opposed"
	DirDiagonal     DirClass = "diagonal"
	DirMixed        DirClass = "mixed"
)

type TraceEntry struct {
	Seq           int    `json:"seq"`
	Stage         string `json:"stage"`
	Path          string `json:"path"`
	ObjectID      string `json:"object_id,omitempty"`
	Field         string `json:"field,omitempty"`
	OldValue      string `json:"old_value,omitempty"`
	NewValue      string `json:"new_value,omitempty"`
	SourceDefault string `json:"source_default,omitempty"`
	Rule          string `json:"rule"`
	TruncatedAt   string `json:"truncated_at,omitempty"`
	Remainder     int64  `json:"remainder,omitempty"`
	OperandA      int64  `json:"operand_a,omitempty"`
	OperandB      int64  `json:"operand_b,omitempty"`
}

type LossyItem struct {
	Seq         int    `json:"seq"`
	Code        string `json:"code"`
	Path        string `json:"path"`
	ObjectID    string `json:"object_id,omitempty"`
	Kind        string `json:"kind"`
	Original    string `json:"original"`
	Disposition string `json:"disposition"`
	SidecarKey  string `json:"sidecar_key,omitempty"`
}

type GrayMask struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Pix    []byte `json:"pix"`
}

func NewGrayMask(width, height int) *GrayMask {
	return &GrayMask{Width: width, Height: height, Pix: make([]byte, width*height)}
}

func (m *GrayMask) Clone() *GrayMask {
	if m == nil {
		return nil
	}
	pix := make([]byte, len(m.Pix))
	copy(pix, m.Pix)
	return &GrayMask{Width: m.Width, Height: m.Height, Pix: pix}
}

func (m *GrayMask) InBounds(x, y int) bool {
	return m != nil && x >= 0 && y >= 0 && x < m.Width && y < m.Height
}

func (m *GrayMask) At(x, y int) byte {
	if !m.InBounds(x, y) {
		return 0
	}
	return m.Pix[y*m.Width+x]
}

func (m *GrayMask) Set(x, y int, v byte) {
	if m.InBounds(x, y) {
		m.Pix[y*m.Width+x] = v
	}
}

func (m *GrayMask) Hash() string {
	sum := sha256.Sum256(m.Pix)
	return hex.EncodeToString(sum[:])
}

func (m *GrayMask) Equals(other *GrayMask) bool {
	if m == nil || other == nil {
		return m == other
	}
	if m.Width != other.Width || m.Height != other.Height || len(m.Pix) != len(other.Pix) {
		return false
	}
	for i := range m.Pix {
		if m.Pix[i] != other.Pix[i] {
			return false
		}
	}
	return true
}

type RGBAMask struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Pix    []byte `json:"pix"`
}

func NewRGBAMask(width, height int) *RGBAMask {
	return &RGBAMask{Width: width, Height: height, Pix: make([]byte, width*height*4)}
}

func (img *RGBAMask) SetRGBA(x, y int, r, g, b, a byte) {
	if img == nil || x < 0 || y < 0 || x >= img.Width || y >= img.Height {
		return
	}
	i := (y*img.Width + x) * 4
	// Source-over for deterministic icon construction.
	ar := int(a)
	img.Pix[i+0] = byte((int(r)*ar + int(img.Pix[i+0])*(255-ar)) / 255)
	img.Pix[i+1] = byte((int(g)*ar + int(img.Pix[i+1])*(255-ar)) / 255)
	img.Pix[i+2] = byte((int(b)*ar + int(img.Pix[i+2])*(255-ar)) / 255)
	img.Pix[i+3] = 255
}

type BBox struct {
	MinX int `json:"min_x"`
	MinY int `json:"min_y"`
	MaxX int `json:"max_x"`
	MaxY int `json:"max_y"`
}

func (b BBox) Width() int  { return b.MaxX - b.MinX + 1 }
func (b BBox) Height() int { return b.MaxY - b.MinY + 1 }

type BoundarySegment struct {
	DomainID int      `json:"domain_id"`
	Index    int      `json:"index"`
	Points   []Point  `json:"points"`
	Normal   Point    `json:"normal"`
	Tangent  Point    `json:"tangent"`
	StrokeID string   `json:"stroke_id,omitempty"`
	Category DirClass `json:"category"`
}

type DomainTruth struct {
	DomainID      int               `json:"domain_id"`
	Area          int               `json:"area"`
	CentroidX1000 int64             `json:"centroid_x1000"`
	CentroidY1000 int64             `json:"centroid_y1000"`
	BBox          BBox              `json:"bbox"`
	Holes         int               `json:"holes"`
	RingWidth1000 int64             `json:"ring_width_1000"`
	Boundary      []BoundarySegment `json:"boundary,omitempty"`
}

type Truth struct {
	Mask    *GrayMask     `json:"mask"`
	Domains []DomainTruth `json:"domains"`
}

type Project struct {
	ID            string        `json:"id"`
	SourceVersion string        `json:"source_version"`
	SchemaVersion string        `json:"schema_version"`
	Width         int           `json:"width"`
	Height        int           `json:"height"`
	TileWidth     int           `json:"tile_width"`
	TileHeight    int           `json:"tile_height"`
	Seed          int64         `json:"seed"`
	Icon          *RGBAMask     `json:"icon,omitempty"`
	Candidate     *GrayMask     `json:"candidate,omitempty"`
	Truth         *Truth        `json:"truth,omitempty"`
	Strokes       []Stroke      `json:"strokes"`
	Groups        []StrokeGroup `json:"groups,omitempty"`
	Trace         []TraceEntry  `json:"trace,omitempty"`
	Lossy         []LossyItem   `json:"lossy,omitempty"`
}

func (p *Project) SortedStrokes() []Stroke {
	out := append([]Stroke(nil), p.Strokes...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

func (p *Project) StrokeByID(id string) (Stroke, bool) {
	for _, s := range p.Strokes {
		if s.ID == id {
			return s, true
		}
	}
	return Stroke{}, false
}

func (p *Project) AppendTrace(stage, path string, entry TraceEntry) {
	entry.Seq = len(p.Trace) + 1
	entry.Stage = stage
	if entry.Path == "" {
		entry.Path = path
	}
	p.Trace = append(p.Trace, entry)
}

func (p *Project) AppendLossy(path string, item LossyItem) {
	item.Seq = len(p.Lossy) + 1
	item.Code = CodeLossy
	if item.Path == "" {
		item.Path = path
	}
	p.Lossy = append(p.Lossy, item)
}

func stableJSONHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

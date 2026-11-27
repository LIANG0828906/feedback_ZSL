package terrainbake

import (
	"fmt"
	"sort"
	"strings"
)

type SchemaVersion int

const (
	SchemaV1 SchemaVersion = 1
	SchemaV2 SchemaVersion = 2
	SchemaV3 SchemaVersion = 3
)

func (v SchemaVersion) String() string {
	switch v {
	case SchemaV1:
		return "v1"
	case SchemaV2:
		return "v2"
	case SchemaV3:
		return "v3"
	default:
		return fmt.Sprintf("v%d", int(v))
	}
}

func IsKnownSchemaVersion(v SchemaVersion) bool {
	return v >= SchemaV1 && v <= SchemaV3
}

type Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type Size struct {
	W int64 `json:"w"`
	H int64 `json:"h"`
}

// Rect is always half-open internally: [MinX,MaxX) x [MinY,MaxY).
type Rect struct {
	MinX int64 `json:"min_x"`
	MinY int64 `json:"min_y"`
	MaxX int64 `json:"max_x"`
	MaxY int64 `json:"max_y"`
}

func (r Rect) Width() int64  { return r.MaxX - r.MinX }
func (r Rect) Height() int64 { return r.MaxY - r.MinY }

func (r Rect) Contains(p Point) bool {
	return p.X >= r.MinX && p.X < r.MaxX && p.Y >= r.MinY && p.Y < r.MaxY
}

func (r Rect) Overlaps(other Rect) bool {
	return r.MinX < other.MaxX && other.MinX < r.MaxX &&
		r.MinY < other.MaxY && other.MinY < r.MaxY
}

type Budget struct {
	MaxVertices  int `json:"max_vertices"`
	MaxTriangles int `json:"max_triangles"`
}

type BlockSpec struct {
	ID                 string   `json:"id"`
	Origin             Point    `json:"origin"`
	Size               Size     `json:"size"`
	Rect               Rect     `json:"rect"`
	Budget             Budget   `json:"budget"`
	ToleranceUM        int64    `json:"tolerance_um"`
	ToleranceScale     string   `json:"tolerance_scale"`
	ToleranceRounding  string   `json:"tolerance_rounding"`
	Neighbors          []string `json:"neighbors"`
	DeclaredNeighbors  bool     `json:"declared_neighbors"`
	SourceCoverageRule string   `json:"source_coverage_rule"`
}

type SeamAxis string

const (
	AxisVertical   SeamAxis = "vertical"
	AxisHorizontal SeamAxis = "horizontal"
)

type SeamSpec struct {
	ID            string   `json:"id"`
	A             string   `json:"block_a"`
	B             string   `json:"block_b"`
	Axis          SeamAxis `json:"axis"`
	Coordinate    int64    `json:"coordinate"`
	Start         int64    `json:"start"`
	End           int64    `json:"end"`
	SampleSpacing int64    `json:"sample_spacing"`
	AngleLimitDeg float64  `json:"angle_limit_deg"`
}

func (s SeamSpec) EdgeKey() string {
	a, b := s.A, s.B
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

type CameraSample struct {
	TimeMS   int64   `json:"time_ms"`
	Position Point   `json:"position"`
	YawDeg   float64 `json:"yaw_deg"`
	PitchDeg float64 `json:"pitch_deg"`
	FOVDeg   float64 `json:"fov_deg"`
	Near     float64 `json:"near"`
	Far      float64 `json:"far"`
	Focus    string  `json:"focus_block"`
}

type World struct {
	Rect Rect   `json:"rect"`
	Seed uint64 `json:"seed"`
	Name string `json:"name"`
}

type ConversionTrace struct {
	SourceVersion SchemaVersion `json:"source_version"`
	Path          string        `json:"path"`
	Operation     string        `json:"operation"`
	From          string        `json:"from,omitempty"`
	To            string        `json:"to,omitempty"`
	Rounding      string        `json:"rounding,omitempty"`
	Residual      int64         `json:"residual"`
	Truncation    string        `json:"truncation,omitempty"`
	Note          string        `json:"note,omitempty"`
}

type TruthSample struct {
	BlockID  string `json:"block_id"`
	X        int64  `json:"x"`
	Y        int64  `json:"y"`
	HeightMM int64  `json:"height_mm"`
}

type SeamTruth struct {
	SeamID    string   `json:"seam_id"`
	Axis      SeamAxis `json:"axis"`
	Start     int64    `json:"start"`
	End       int64    `json:"end"`
	Points    []int64  `json:"points"`
	HeightsMM []int64  `json:"heights_mm"`
}

type FractureTruth struct {
	ID          string  `json:"id"`
	SeamID      string  `json:"seam_id"`
	Kind        string  `json:"kind"`
	At          int64   `json:"at"`
	ToleranceUM int64   `json:"tolerance_um"`
	AngleLimit  float64 `json:"angle_limit_deg"`
	Expected    string  `json:"expected_attribution"`
}

type Truth struct {
	Samples      []TruthSample   `json:"samples"`
	MaxTriangles map[string]int  `json:"max_triangles"`
	Seams        []SeamTruth     `json:"seams"`
	Fractures    []FractureTruth `json:"fractures"`
}

type Project struct {
	SourceVersion SchemaVersion     `json:"source_version"`
	World         World             `json:"world"`
	Blocks        []BlockSpec       `json:"blocks"`
	Seams         []SeamSpec        `json:"seams"`
	Camera        []CameraSample    `json:"camera_track"`
	Truth         Truth             `json:"truth"`
	Traces        []ConversionTrace `json:"conversion_traces"`
	GeneratedBy   string            `json:"generated_by"`
}

func (p *Project) BlockByID(id string) (*BlockSpec, bool) {
	for i := range p.Blocks {
		if p.Blocks[i].ID == id {
			return &p.Blocks[i], true
		}
	}
	return nil, false
}

func (p *Project) SeamByID(id string) (*SeamSpec, bool) {
	for i := range p.Seams {
		if p.Seams[i].ID == id {
			return &p.Seams[i], true
		}
	}
	return nil, false
}

func (p *Project) Clone() *Project {
	out := *p
	out.Blocks = append([]BlockSpec(nil), p.Blocks...)
	for i := range out.Blocks {
		out.Blocks[i].Neighbors = append([]string(nil), p.Blocks[i].Neighbors...)
	}
	out.Seams = append([]SeamSpec(nil), p.Seams...)
	out.Camera = append([]CameraSample(nil), p.Camera...)
	out.Traces = append([]ConversionTrace(nil), p.Traces...)
	out.Truth.Samples = append([]TruthSample(nil), p.Truth.Samples...)
	out.Truth.Seams = append([]SeamTruth(nil), p.Truth.Seams...)
	for i := range out.Truth.Seams {
		out.Truth.Seams[i].Points = append([]int64(nil), p.Truth.Seams[i].Points...)
		out.Truth.Seams[i].HeightsMM = append([]int64(nil), p.Truth.Seams[i].HeightsMM...)
	}
	out.Truth.Fractures = append([]FractureTruth(nil), p.Truth.Fractures...)
	out.Truth.MaxTriangles = make(map[string]int, len(p.Truth.MaxTriangles))
	for k, v := range p.Truth.MaxTriangles {
		out.Truth.MaxTriangles[k] = v
	}
	return &out
}

func (p *Project) AdjacentBlocks(id string) []string {
	seen := map[string]bool{}
	for _, s := range p.Seams {
		switch id {
		case s.A:
			seen[s.B] = true
		case s.B:
			seen[s.A] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (p *Project) BlockOrder() []string {
	ids := make([]string, 0, len(p.Blocks))
	for _, b := range p.Blocks {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	return ids
}

func (p *Project) ValidateTopology() error {
	blockIDs := map[string]bool{}
	for i := range p.Blocks {
		b := p.Blocks[i]
		if b.ID == "" {
			return errSchema(fmt.Sprintf("blocks[%d].id", i), "block id is required")
		}
		if blockIDs[b.ID] {
			return errSchema(fmt.Sprintf("blocks[%d].id", i), "duplicate block id "+b.ID)
		}
		blockIDs[b.ID] = true
		if b.Rect.Width() <= 0 || b.Rect.Height() <= 0 {
			return &Error{
				Code:    CodeSchema,
				Path:    fmt.Sprintf("blocks[%d].rect", i),
				Field:   "rect",
				BlockID: b.ID,
				Message: "non-positive coverage",
			}
		}
		if b.Budget.MaxVertices <= 0 || b.Budget.MaxTriangles <= 0 {
			return errPara("budget", "max_vertices>0,max_triangles>0", "block budget must be positive")
		}
		if b.ToleranceUM <= 0 {
			return errPara("chord_tolerance", ">0", "chord tolerance must be positive")
		}
	}
	for i := 0; i < len(p.Blocks); i++ {
		for j := i + 1; j < len(p.Blocks); j++ {
			if p.Blocks[i].Rect.Overlaps(p.Blocks[j].Rect) {
				return errOver(
					fmt.Sprintf("blocks[%d].rect", j),
					p.Blocks[i].ID,
					p.Blocks[j].ID,
					"block coverage overlaps after boundary normalization",
				)
			}
		}
	}
	for _, b := range p.Blocks {
		for _, n := range b.Neighbors {
			if _, ok := p.BlockByID(n); !ok {
				return errAdj(
					"blocks."+b.ID+".neighbors",
					"neighbor",
					"adjacency references unknown block "+n,
				)
			}
		}
	}
	seamIDs := map[string]bool{}
	for i := range p.Seams {
		s := p.Seams[i]
		if seamIDs[s.ID] {
			return errSchema(fmt.Sprintf("seams[%d].id", i), "duplicate seam id "+s.ID)
		}
		seamIDs[s.ID] = true
		if _, ok := p.BlockByID(s.A); !ok {
			return errAdj(fmt.Sprintf("seams[%d].block_a", i), "block_a", "unknown block "+s.A)
		}
		if _, ok := p.BlockByID(s.B); !ok {
			return errAdj(fmt.Sprintf("seams[%d].block_b", i), "block_b", "unknown block "+s.B)
		}
		if s.SampleSpacing <= 0 {
			return errPara("seams."+s.ID+".sample_spacing", ">=1", "seam sample spacing must be positive")
		}
		if s.End <= s.Start {
			return &Error{
				Code:     CodeAttr,
				Boundary: s.ID,
				BlockA:   s.A,
				BlockB:   s.B,
				Message:  "seam has empty shared range",
			}
		}
	}
	return nil
}

func RectFromOriginSize(o Point, s Size) Rect {
	return Rect{MinX: o.X, MinY: o.Y, MaxX: o.X + s.W, MaxY: o.Y + s.H}
}

func EdgeID(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "S:" + a + "-" + b
}

func SortedUniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func normalizeScaleName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "permille", "per_mille", "‰", "thousandth":
		return "permille"
	case "bp", "basis_points", "per_10000", "万分比", "basis_point":
		return "basis_points"
	default:
		return s
	}
}

package maskqc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
)

const (
	SchemaV1 = "icon.project/v1"
	SchemaV2 = "icon.project/v2"
	SchemaV3 = "icon.project/v3"

	AlgorithmVersion = "raster-int-v1.2.0"
	// OrderRulesVersion changes only when precedence rules themselves change.
	OrderRulesVersion = "priority-then-seq-v1"
)

// Point is an integer pixel coordinate.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Rect is a half-open integer rectangle.
type Rect struct {
	MinX int `json:"min_x"`
	MinY int `json:"min_y"`
	MaxX int `json:"max_x"`
	MaxY int `json:"max_y"`
}

func (r Rect) Width() int  { return r.MaxX - r.MinX }
func (r Rect) Height() int { return r.MaxY - r.MinY }
func (r Rect) Empty() bool { return r.MaxX <= r.MinX || r.MaxY <= r.MinY }

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
	if r.Empty() {
		return Rect{}
	}
	return r
}

func rectFromImage(r image.Rectangle) Rect {
	return Rect{MinX: r.Min.X, MinY: r.Min.Y, MaxX: r.Max.X, MaxY: r.Max.Y}
}

// Stroke is the schema-independent construction operation.
type Stroke struct {
	Seq              int    `json:"seq"`
	OrderPriority    int    `json:"order_priority"`
	Start            Point  `json:"start"`
	End              Point  `json:"end"`
	WeightPermille   int    `json:"weight_permille"`
	RadiusTick       int    `json:"radius_tick"`
	HardnessPermille int    `json:"hardness_permille"`
	WeightRemainder  int64  `json:"weight_remainder,omitempty"`
	ResidualDigest   string `json:"residual_digest,omitempty"`
}

// TransformRecord is one auditable schema/unit conversion.
type TransformRecord struct {
	Path          string `json:"path"`
	Field         string `json:"field"`
	Op            string `json:"op"`
	SourceVersion string `json:"source_version,omitempty"`
	TargetVersion string `json:"target_version,omitempty"`
	Old           any    `json:"old,omitempty"`
	New           any    `json:"new,omitempty"`
	DefaultValue  any    `json:"default_value,omitempty"`
	Rounding      string `json:"rounding,omitempty"`
	Remainder     int64  `json:"remainder,omitempty"`
	TruncatedAt   string `json:"truncated_at,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// CoverageComponent is a stroke's isolated alpha contribution.
type CoverageComponent struct {
	StrokeSeq   int    `json:"stroke_seq"`
	Bounds      Rect   `json:"bounds"`
	Alpha       []byte `json:"alpha"`
	ContentHash string `json:"content_hash"`
}

// DomainMetric contains both truth and measured topology fields.
type DomainMetric struct {
	DomainID   int     `json:"domain_id"`
	Area       int     `json:"area"`
	CentroidX  float64 `json:"centroid_x"`
	CentroidY  float64 `json:"centroid_y"`
	Bounds     Rect    `json:"bounds"`
	Holes      int     `json:"holes"`
	BandWidth  float64 `json:"band_width"`
	AngleDeg   float64 `json:"angle_deg"`
	PixelCount int     `json:"pixel_count"`
}

// Truth is generated only by the deterministic construction generator.
type Truth struct {
	Mask             *image.Gray                `json:"-"`
	Domains          []DomainMetric             `json:"domains"`
	StrokeComponents map[int]*CoverageComponent `json:"-"`
	ReferenceHash    string                     `json:"reference_hash"`
	ComponentHashes  map[int]string             `json:"component_hashes"`
}

// Project is the internal unified engineering model.
type Project struct {
	ID              string            `json:"id"`
	Seed            int64             `json:"seed"`
	Width           int               `json:"width"`
	Height          int               `json:"height"`
	SourceVersion   string            `json:"source_version"`
	Image           *image.RGBA       `json:"-"`
	Candidate       *image.Gray       `json:"-"`
	Strokes         []Stroke          `json:"strokes"`
	ChannelCoverage []int             `json:"channel_coverage"`
	Truth           *Truth            `json:"truth,omitempty"`
	Trail           []TransformRecord `json:"trail,omitempty"`
}

// ChunkCoord identifies a tile by tile indices.
type ChunkCoord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func (c ChunkCoord) String() string {
	return "(" + itoa(c.X) + "," + itoa(c.Y) + ")"
}

// ChunkResult is a deterministic tile output and its cache identity.
type ChunkResult struct {
	Coord        ChunkCoord `json:"coord"`
	Bounds       Rect       `json:"bounds"`
	Alpha        []byte     `json:"alpha,omitempty"`
	InputHash    string     `json:"input_hash"`
	ComponentKey string     `json:"component_key,omitempty"`
	OrderKey     string     `json:"order_key"`
	ParamsKey    string     `json:"params_key"`
	UpstreamKeys []string   `json:"upstream_keys"`
	CacheKey     string     `json:"cache_key"`
	OutputHash   string     `json:"output_hash"`
}

// GraphNode is a dependency graph vertex.
type GraphNode struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Label    string         `json:"label"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// GraphEdge is a directed dependency relation.
type GraphEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Reason    string `json:"reason"`
	StrokeSeq int    `json:"stroke_seq,omitempty"`
}

// Graph explicitly records stroke/order/topology/chunk dependencies.
type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// WaitRecord explains bounded scheduler blocking.
type WaitRecord struct {
	Chunk       ChunkCoord `json:"chunk"`
	Reason      string     `json:"reason"`
	WaitMillis  int64      `json:"wait_millis"`
	Parallelism int        `json:"parallelism"`
	QueueBudget int        `json:"queue_budget"`
}

// InvalidationItem lists one affected tile.
type InvalidationItem struct {
	Chunk      ChunkCoord `json:"chunk"`
	Reason     string     `json:"reason"`
	StrokeSeqs []int      `json:"stroke_seqs"`
	Fields     []string   `json:"fields"`
}

// InvalidationReport is the precise dependency propagation result.
type InvalidationReport struct {
	InvalidatedChunks []InvalidationItem `json:"invalidated_chunks"`
	ReusedChunks      []ChunkCoord       `json:"reused_chunks"`
	InvalidatedTopo   []int              `json:"invalidated_topology_domains"`
	WaitRecords       []WaitRecord       `json:"wait_records,omitempty"`
}

// EditKind enumerates supported deterministic edits.
type EditKind string

const (
	EditMoveStart EditKind = "move_start"
	EditMoveEnd   EditKind = "move_end"
	EditWeight    EditKind = "weight"
	EditPriority  EditKind = "priority"
	EditHardness  EditKind = "hardness"
	EditRadius    EditKind = "radius"
)

// Edit is one construction mutation.
type Edit struct {
	Kind      EditKind `json:"kind"`
	StrokeSeq int      `json:"stroke_seq"`
	Point     Point    `json:"point,omitempty"`
	Value     int      `json:"value,omitempty"`
}

func hashBytes(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write(part)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HashBytes exposes the stable SHA-256 framing used by domain content hashes.
func HashBytes(parts ...[]byte) string {
	return hashBytes(parts...)
}

func hashJSON(prefix string, v any) string {
	b, _ := json.Marshal(v)
	return hashBytes([]byte(prefix), b)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

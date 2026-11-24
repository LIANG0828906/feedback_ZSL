package terrain

const (
	FormatVersion    = "block-minimal-terrain/v1"
	AlgorithmVersion = "adaptive-longedge-delaunay-v1"
	DefaultParamVer  = "sampling-params-v1"
	StoreSchemaVer   = "checkpoint-v1"
	SideNorth        = "north"
	SideSouth        = "south"
	SideEast         = "east"
	SideWest         = "west"
	StatusClean      = "clean"
	StatusDirty      = "dirty"
	StatusFailed     = "failed"
	StatusRecovered  = "recovered"
	StatusComputing  = "computing"
	ReuseCache       = "persisted_cache_reused"
	ReuseRebuild     = "rebuild_required"
	ReuseRecovered   = "recovered_checkpoint"
	ReasonHeight     = "height_changed"
	ReasonBudget     = "sample_budget_changed"
	ReasonTolerance  = "error_tolerance_changed"
	ReasonBoundary   = "boundary_realign"
	ReasonVersion    = "version_mismatch"
	ReasonCorrupt    = "checkpoint_corrupt"
)

type GridPoint struct {
	X int `json:"grid_x"`
	Y int `json:"grid_y"`
}

type Grid struct {
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	CellSize float64   `json:"cell_size"`
	Heights  []float64 `json:"heights"`
}

type Block struct {
	BlockNo        int         `json:"block_no"`
	ID             string      `json:"id"`
	Feature        string      `json:"feature"`
	Grid           Grid        `json:"grid"`
	OriginX        float64     `json:"origin_x"`
	OriginZ        float64     `json:"origin_z"`
	CoverageXZ     [4]float64  `json:"coverage_xz"`
	SampleBudget   int         `json:"sample_budget"`
	ErrorTolerance float64     `json:"error_tolerance"`
	RequiredPoints []GridPoint `json:"required_points,omitempty"`
}

type SharedBoundary struct {
	ID           string `json:"id"`
	BlockA       int    `json:"block_a"`
	BlockB       int    `json:"block_b"`
	SideA        string `json:"side_a"`
	SideB        string `json:"side_b"`
	ConstraintID string `json:"constraint_id"`
}

type AlignmentConstraint struct {
	ID                   string  `json:"id"`
	BlockA               int     `json:"block_a"`
	BlockB               int     `json:"block_b"`
	SideA                string  `json:"side_a"`
	SideB                string  `json:"side_b"`
	SampleColumnA        []int   `json:"sample_column_a"`
	SampleColumnB        []int   `json:"sample_column_b"`
	MaxFoldAngleDeg      float64 `json:"max_fold_angle_deg"`
	AlignmentBufferCells float64 `json:"alignment_buffer_cells"`
}

type VertexTruth struct {
	BlockNo        int     `json:"block_no"`
	GridX          int     `json:"grid_x"`
	GridY          int     `json:"grid_y"`
	ExpectedHeight float64 `json:"expected_height"`
	Tolerance      float64 `json:"tolerance"`
}

type BlockErrorTruth struct {
	BlockNo                int     `json:"block_no"`
	DeclaredMaxHeightError float64 `json:"declared_max_height_error"`
}

type BoundaryTruth struct {
	BoundaryID          string  `json:"boundary_id"`
	MaxAllowedFoldAngle float64 `json:"max_allowed_fold_angle_deg"`
}

type Truth struct {
	VertexChecks     []VertexTruth     `json:"vertex_checks"`
	BlockErrorBounds []BlockErrorTruth `json:"block_error_bounds"`
	BoundaryChecks   []BoundaryTruth   `json:"boundary_checks"`
	FaultCases       []FaultCase       `json:"fault_cases"`
}

type Project struct {
	SchemaVersion        string                `json:"schema_version"`
	ProjectID            string                `json:"project_id"`
	Seed                 int64                 `json:"seed"`
	AlgorithmVersion     string                `json:"algorithm_version"`
	ParameterVersion     string                `json:"parameter_version"`
	Blocks               []Block               `json:"blocks"`
	SharedBoundaries     []SharedBoundary      `json:"shared_boundaries"`
	AlignmentConstraints []AlignmentConstraint `json:"alignment_constraints"`
	Truth                Truth                 `json:"truth"`
}

type Vertex struct {
	Index int     `json:"index"`
	GridX int     `json:"grid_x"`
	GridY int     `json:"grid_y"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Z     float64 `json:"z"`
}

type Triangle struct {
	Index    int    `json:"index"`
	Vertices [3]int `json:"vertices"`
}

type TriangleError struct {
	TriangleIndex int       `json:"triangle_index"`
	MaxAbsError   float64   `json:"max_abs_error"`
	At            GridPoint `json:"at"`
	SampledPoints int       `json:"sampled_points"`
}

type Mesh struct {
	BlockNo        int             `json:"block_no"`
	AlgorithmVer   string          `json:"algorithm_version"`
	ParameterVer   string          `json:"parameter_version"`
	Vertices       []Vertex        `json:"vertices"`
	Triangles      []Triangle      `json:"triangles"`
	TriangleErrors []TriangleError `json:"triangle_errors"`
	MaxHeightError float64         `json:"max_height_error"`
	Budget         int             `json:"sample_budget"`
	Tolerance      float64         `json:"error_tolerance"`
}

type Dependency struct {
	BoundaryID string `json:"boundary_id"`
	BlockNo    int    `json:"block_no"`
	DependsOn  int    `json:"depends_on"`
	Direction  string `json:"direction"`
}

type BoundaryPoint struct {
	T     float64 `json:"t"`
	GridX int     `json:"grid_x,omitempty"`
	GridY int     `json:"grid_y,omitempty"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Z     float64 `json:"z"`
}

type BoundarySide struct {
	BlockNo        int             `json:"block_no"`
	Side           string          `json:"side"`
	Points         []BoundaryPoint `json:"points"`
	Tangents       [][3]float64    `json:"tangents,omitempty"`
	SegmentNormals [][3]float64    `json:"segment_normals,omitempty"`
	MaxHeightError float64         `json:"max_height_error,omitempty"`
}

type FaultCase struct {
	ID               string       `json:"id"`
	BoundaryID       string       `json:"boundary_id"`
	BlockA           int          `json:"block_a"`
	BlockB           int          `json:"block_b"`
	Left             BoundarySide `json:"left"`
	Right            BoundarySide `json:"right"`
	FoldLimitDeg     float64      `json:"fold_limit_deg"`
	HeightTolerance  float64      `json:"height_tolerance"`
	HeightToleranceA float64      `json:"height_tolerance_a,omitempty"`
	HeightToleranceB float64      `json:"height_tolerance_b,omitempty"`
	AlignmentBuffer  float64      `json:"alignment_buffer_world"`
	ExpectedCategory string       `json:"expected_category"`
	ExpectedCode     string       `json:"expected_code,omitempty"`
}

type AngleLocation struct {
	T            float64    `json:"t"`
	BlockA       int        `json:"block_a"`
	BlockB       int        `json:"block_b"`
	At           [3]float64 `json:"at_xyz"`
	HeightErrorA float64    `json:"height_error_a,omitempty"`
	HeightErrorB float64    `json:"height_error_b,omitempty"`
	PairResidual float64    `json:"pair_residual,omitempty"`
}

type AttributionReport struct {
	CaseID           string         `json:"case_id,omitempty"`
	BoundaryID       string         `json:"boundary_id"`
	BlockA           int            `json:"block_a"`
	BlockB           int            `json:"block_b"`
	Category         string         `json:"category"`
	ErrorCode        string         `json:"error_code,omitempty"`
	Aligned          bool           `json:"aligned"`
	MaxFoldAngleDeg  float64        `json:"max_fold_angle_deg"`
	LimitDeg         float64        `json:"limit_deg"`
	MaxAngleLocation *AngleLocation `json:"max_angle_location,omitempty"`
	MaxHeightErrorA  float64        `json:"max_height_error_a"`
	MaxHeightErrorB  float64        `json:"max_height_error_b"`
	MaxPairResidual  float64        `json:"max_pair_residual"`
	OrderMismatch    bool           `json:"order_mismatch"`
	Evidence         []string       `json:"evidence"`
	CheckedAt        string         `json:"checked_at,omitempty"`
}

type BoundaryReport struct {
	AttributionReport
	VertexCountA int `json:"vertex_count_a"`
	VertexCountB int `json:"vertex_count_b"`
}

type InvalidationItem struct {
	BlockNo    int      `json:"block_no"`
	Invalid    bool     `json:"invalid"`
	Reason     string   `json:"reason"`
	Reuse      string   `json:"reuse_status"`
	Boundaries []string `json:"boundaries,omitempty"`
}

type QueueEvent struct {
	BlockNo  int    `json:"block_no"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	WaitMS   int64  `json:"wait_ms,omitempty"`
	AtMS     int64  `json:"at_ms,omitempty"`
	Vertices int    `json:"vertices,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
}

type BlockRunStat struct {
	BlockNo     int     `json:"block_no"`
	Status      string  `json:"status"`
	CacheHit    bool    `json:"cache_hit"`
	BuildMS     int64   `json:"build_ms"`
	CommitMS    int64   `json:"commit_ms,omitempty"`
	Vertices    int     `json:"vertices"`
	Triangles   int     `json:"triangles"`
	MaxError    float64 `json:"max_error"`
	ContentHash string  `json:"content_hash,omitempty"`
	Failure     string  `json:"failure,omitempty"`
	QueueReason string  `json:"queue_reason,omitempty"`
}

type CommitRecord struct {
	Seq         int    `json:"seq"`
	BlockNo     int    `json:"block_no"`
	Hash        string `json:"content_hash"`
	CommittedAt string `json:"committed_at"`
}

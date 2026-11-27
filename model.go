package grid

import "fmt"

const (
	DefaultSeed      int64 = 20261127
	PositionScale    int64 = 1000 // one internal coordinate unit is 1/1000 mm (micrometre)
	NormalScale      int64 = 1000
	CoefficientScale int64 = 10000 // basis points
	PermilleScale    int64 = 1000
	TickMilliseconds int64 = 10
	AlgorithmVersion       = "fixed-sweep-v1"
	// PixelSize is both width and height of one square tile in pixels.
	PixelSize = 32
	// CoverageSamples equals PixelSize*PixelSize; coverage is counted in pixels.
	CoverageSamples = PixelSize * PixelSize
	// DefaultCoverageCeilingPermille is the declared per-tile coverage cap.
	DefaultCoverageCeilingPermille = 250
)

// Vec2 is a fixed-point vector. World coordinates are micrometres; velocities
// are micrometres/second; normals use NormalScale as their unit length.
type Vec2 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

func (v Vec2) Add(o Vec2) Vec2 { return Vec2{X: v.X + o.X, Y: v.Y + o.Y} }
func (v Vec2) Sub(o Vec2) Vec2 { return Vec2{X: v.X - o.X, Y: v.Y - o.Y} }

// Point is an alias used to make endpoint intent explicit.
type Point = Vec2

type Obstacle struct {
	ID             string `json:"id"`
	Seq            int    `json:"seq"`
	A              Point  `json:"a"`
	B              Point  `json:"b"`
	Normal         Vec2   `json:"normal"`
	Restitution    int64  `json:"restitutionBasisPoints"`
	MaterialVer    int    `json:"materialVersion"`
	Penetrable     bool   `json:"penetrable"`
	EndpointVer    int    `json:"endpointVersion"`
	NormalVer      int    `json:"normalVersion"`
	RestitutionVer int    `json:"restitutionVersion"`
}

type Launch struct {
	ID          string `json:"id"`
	Origin      Point  `json:"origin"`
	Velocity    Vec2   `json:"velocity"`
	BounceLimit int    `json:"bounceLimit"`
	FrameLimit  int    `json:"frameLimit"`
}

type FrameRequest struct {
	RequestID string `json:"requestId"`
	Frame     int    `json:"frame"`
	LaunchID  string `json:"launchId"`
}

type TruthCheckpoint struct {
	Frame      int    `json:"frame"`
	ObstacleID string `json:"obstacleId"`
	X          int64  `json:"x"`
	Y          int64  `json:"y"`
	BounceSeq  int    `json:"bounceSeq"`
}

type Truth struct {
	RequestID         string            `json:"requestId"`
	LaunchID          string            `json:"launchId"`
	FirstObstacleID   string            `json:"firstObstacleId,omitempty"`
	BounceCount       int               `json:"bounceCount"`
	Escaped           bool              `json:"escaped"`
	TerminalReason    string            `json:"terminalReason"`
	Checkpoints       []TruthCheckpoint `json:"checkpoints"`
	ExpectedErrorCode ErrorCode         `json:"expectedErrorCode,omitempty"`
	ExpectedBounceSeq int               `json:"expectedBounceSeq,omitempty"`
}

type Level struct {
	SchemaVersion int            `json:"schemaVersion"`
	Seed          int64          `json:"seed"`
	Code          string         `json:"code"`
	Gravity       Vec2           `json:"gravity"` // micrometres/s^2
	StepMS        int64          `json:"stepMs"`
	Bounds        [2]Point       `json:"bounds"`
	Obstacles     []Obstacle     `json:"obstacles"`
	Launches      []Launch       `json:"launches"`
	Requests      []FrameRequest `json:"requests"`
	Truths        []Truth        `json:"truths,omitempty"`
}

// FieldTrace records one deterministic integer or schema conversion action.
type FieldTrace struct {
	Kind         string `json:"kind"`
	ObjectID     string `json:"objectId,omitempty"`
	Field        string `json:"field"`
	Position     string `json:"position,omitempty"`
	OldValue     string `json:"oldValue,omitempty"`
	NewValue     string `json:"newValue,omitempty"`
	Source       string `json:"source,omitempty"`
	Rounding     string `json:"rounding,omitempty"`
	Numerator    int64  `json:"numerator,omitempty"`
	Denominator  int64  `json:"denominator,omitempty"`
	Remainder    int64  `json:"remainder,omitempty"`
	Disposition  string `json:"disposition,omitempty"`
	SemanticLoss bool   `json:"semanticLoss,omitempty"`
}

type ConversionReport struct {
	SourceSchema int          `json:"sourceSchema,omitempty"`
	TargetSchema int          `json:"targetSchema"`
	Fills        []FieldTrace `json:"fills"`
	Losses       []FieldTrace `json:"losses"`
	Truncations  []FieldTrace `json:"truncations"`
}

func (r ConversionReport) Counts() (fills, losses, truncations int) {
	return len(r.Fills), len(r.Losses), len(r.Truncations)
}

func (id TileID) String() string { return fmt.Sprintf("%d,%d", id.Col, id.Row) }

type TileID struct {
	Col int `json:"col"`
	Row int `json:"row"`
}

func tileIDPtr(col, row int) *TileID { return &TileID{Col: col, Row: row} }

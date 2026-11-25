package kernel

const (
	SchemaV1 = 1
	SchemaV2 = 2
	SchemaV3 = 3

	DefaultWidthUnits  int64 = 1280
	DefaultHeightUnits int64 = 720
	DefaultMaxFrames         = 24
)

type SemanticVersions struct {
	Endpoints    int64 `json:"endpoints"`
	Normals      int64 `json:"normals"`
	Restitutions int64 `json:"restitutions"`
	Gravity      int64 `json:"gravity"`
	TimeStep     int64 `json:"time_step"`
	Materials    int64 `json:"materials"`
}

func (v SemanticVersions) Clone() SemanticVersions { return v }

type Scene struct {
	SchemaVersion    int                 `json:"schema_version"`
	LevelID          string              `json:"level_id"`
	Seed             int64               `json:"seed"`
	Width            int64               `json:"width"`
	Height           int64               `json:"height"`
	MSPerTick        int64               `json:"ms_per_tick"`
	StepTicks        int64               `json:"step_ticks"`
	StepMS           int64               `json:"step_ms"`
	Gravity          Vec2                `json:"gravity"`
	Segments         []Segment           `json:"segments"`
	Launchers        []Launcher          `json:"launchers"`
	Requests         []PredictionRequest `json:"requests"`
	Versions         SemanticVersions    `json:"versions"`
	StructureVersion int64               `json:"structure_version"`
	Prepared         bool                `json:"prepared,omitempty"`
}

type Segment struct {
	ID               string `json:"id"`
	MaterialID       string `json:"material_id"`
	MaterialVersion  int64  `json:"material_version"`
	A                Vec2   `json:"a"`
	B                Vec2   `json:"b"`
	NormalSide       string `json:"normal_side"`
	Normal           Vec2   `json:"normal"`
	Restitution      int64  `json:"restitution"`
	Penetrable       bool   `json:"penetrable"`
	PenetrationDepth int64  `json:"penetration_depth"`
}

type Launcher struct {
	ID       string `json:"id"`
	Position Vec2   `json:"position"`
	Velocity Vec2   `json:"velocity"`
}

type PredictionRequest struct {
	ID           string `json:"id"`
	LogicalFrame int64  `json:"logical_frame"`
	LauncherID   string `json:"launcher_id"`
	MaxFrames    int    `json:"max_frames"`
}

type Truth struct {
	LevelID       string             `json:"level_id"`
	Seed          int64              `json:"seed"`
	PositionScale int64              `json:"position_scale"`
	CoeffScale    int64              `json:"coeff_scale"`
	Samples       []TruthSample      `json:"samples"`
	Bounces       []TruthBounce      `json:"bounces"`
	Attributions  []TruthAttribution `json:"attributions"`
}

type TruthSample struct {
	RequestID    string `json:"request_id"`
	LauncherID   string `json:"launcher_id"`
	LogicalFrame int64  `json:"logical_frame"`
	LocalFrame   int    `json:"local_frame"`
	X            int64  `json:"x"`
	Y            int64  `json:"y"`
}

type TruthBounce struct {
	RequestID string `json:"request_id"`
	Index     int    `json:"index"`
	Frame     int    `json:"frame"`
	SegmentID string `json:"segment_id"`
	X         int64  `json:"x"`
	Y         int64  `json:"y"`
}

type TruthAttribution struct {
	RequestID            string   `json:"request_id"`
	LauncherID           string   `json:"launcher_id"`
	FirstHitSegmentID    string   `json:"first_hit_segment_id"`
	BounceCount          int      `json:"bounce_count"`
	HitSegmentIDs        []string `json:"hit_segment_ids"`
	Escaped              bool     `json:"escaped"`
	TerminalLogicalFrame int64    `json:"terminal_logical_frame"`
	EndX                 int64    `json:"end_x"`
	EndY                 int64    `json:"end_y"`
}

type Quantization struct {
	Frame               int   `json:"frame"`
	TruncatedPosition   Vec2  `json:"truncated_position"`
	GravityRemainder    Vec2  `json:"gravity_remainder"`
	MoveRemainder       Vec2  `json:"move_remainder"`
	MoveDenominator     int64 `json:"move_denominator"`
	HitRemainder        Vec2  `json:"hit_remainder"`
	HitDenominator      int64 `json:"hit_denominator"`
	ReflectionRemainder Vec2  `json:"reflection_remainder"`
}

type Sample struct {
	RequestID    string       `json:"request_id"`
	LauncherID   string       `json:"launcher_id"`
	LogicalFrame int64        `json:"logical_frame"`
	LocalFrame   int          `json:"local_frame"`
	Position     Vec2         `json:"position"`
	Quant        Quantization `json:"quantization"`
}

type Bounce struct {
	RequestID      string `json:"request_id"`
	Index          int    `json:"index"`
	Frame          int    `json:"frame"`
	SegmentID      string `json:"segment_id"`
	Position       Vec2   `json:"position"`
	Normal         Vec2   `json:"normal"`
	VelocityBefore Vec2   `json:"velocity_before"`
	VelocityAfter  Vec2   `json:"velocity_after"`
	Restitution    int64  `json:"restitution"`
	Penetrated     bool   `json:"penetrated"`
}

type Attribution struct {
	RequestID         string   `json:"request_id"`
	LauncherID        string   `json:"launcher_id"`
	FirstHitSegmentID string   `json:"first_hit_segment_id"`
	BounceCount       int      `json:"bounce_count"`
	HitSegmentIDs     []string `json:"hit_segment_ids"`
	Escaped           bool     `json:"escaped"`
	TerminalFrame     int      `json:"terminal_frame"`
	End               Vec2     `json:"end"`
	Outcome           string   `json:"outcome"`
}

type Trajectory struct {
	RequestID   string      `json:"request_id"`
	LauncherID  string      `json:"launcher_id"`
	Samples     []Sample    `json:"samples"`
	Bounces     []Bounce    `json:"bounces"`
	Attribution Attribution `json:"attribution"`
}

type PredictionReport struct {
	Trajectories []Trajectory `json:"trajectories"`
}

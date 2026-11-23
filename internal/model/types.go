package model

const (
	SchemaVersion      = "1.0"
	GeneratorVersion   = "generator-v1"
	AlgorithmVersion   = "retarget-v1.0.0"
	DefaultAlgorithmID = "deterministic-iterative-projection"
)

type Skeleton struct {
	ID     string  `json:"id"`
	Joints []Joint `json:"joints"`
}

type Joint struct {
	Name        string  `json:"name"`
	Parent      string  `json:"parent"`
	Offset      Vec3    `json:"offset"`
	MaxAngleDeg float64 `json:"max_angle_deg"`
	EndEffector bool    `json:"end_effector,omitempty"`
}

type MapRule struct {
	Source   string   `json:"source"`
	Targets  []string `json:"targets"`
	Strategy string   `json:"strategy"`
}

type FallbackRule struct {
	Target   string `json:"target"`
	Source   string `json:"source"`
	Strategy string `json:"strategy"`
	Reason   string `json:"reason"`
}

type TargetSkeleton struct {
	ID            string            `json:"id"`
	Joints        []Joint           `json:"joints"`
	Mapping       []MapRule         `json:"mapping"`
	Fallbacks     []FallbackRule    `json:"fallbacks,omitempty"`
	ContactJoints map[string]string `json:"contact_joints"`
}

func (t TargetSkeleton) Skeleton() Skeleton {
	return Skeleton{ID: t.ID, Joints: t.Joints}
}

type ContactInterval struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type Frame struct {
	Index           int             `json:"index"`
	RootTranslation Vec3            `json:"root_translation"`
	Rotations       []Quat          `json:"rotations"`
	Contacts        map[string]bool `json:"contacts"`
}

type KeyframeTruth struct {
	Frame int             `json:"frame"`
	World map[string]Vec3 `json:"world"`
}

type Tolerances struct {
	WorldPosition      float64 `json:"world_position"`
	BoneLength         float64 `json:"bone_length"`
	AngleDeg           float64 `json:"angle_deg"`
	Reach              float64 `json:"reach"`
	ContactDrift       float64 `json:"contact_drift"`
	ContactFrameSlack  int     `json:"contact_frame_slack"`
	Root               float64 `json:"root"`
	ScaleContactSlack  int     `json:"scale_contact_slack"`
	ScaleRoot          float64 `json:"scale_root"`
	ResidualNormalized float64 `json:"residual_normalized"`
}

type Truth struct {
	FrameCount       int                          `json:"frame_count"`
	JointCount       int                          `json:"joint_count"`
	Keyframes        []KeyframeTruth              `json:"keyframes"`
	BoneLengths      map[string]float64           `json:"bone_lengths"`
	ContactIntervals map[string][]ContactInterval `json:"contact_intervals"`
	RootStart        Vec3                         `json:"root_start"`
	RootEnd          Vec3                         `json:"root_end"`
	Tolerances       Tolerances                   `json:"tolerances"`
}

type Clip struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	Seed          int64    `json:"seed"`
	FPS           float64  `json:"fps"`
	Skeleton      Skeleton `json:"skeleton"`
	Frames        []Frame  `json:"frames"`
	Truth         Truth    `json:"truth"`
}

type Budget struct {
	MaxExactFrames  int `json:"max_exact_frames"`
	MaxExactJoints  int `json:"max_exact_joints"`
	MaxExactWork    int `json:"max_exact_work"`
	MaxSolveFrames  int `json:"max_solve_frames"`
	MaxSmoothWindow int `json:"max_smooth_window"`
	MinSmoothWindow int `json:"min_smooth_window"`
}

type Config struct {
	Mode              string  `json:"mode"`
	Preview           bool    `json:"preview"`
	Budget            Budget  `json:"budget"`
	SmoothWindow      int     `json:"smooth_window"`
	MaxIterations     int     `json:"max_iterations"`
	AngleToleranceDeg float64 `json:"angle_tolerance_deg"`
	ReachTolerance    float64 `json:"reach_tolerance"`
	GroundY           float64 `json:"ground_y"`
	ContactDamping    float64 `json:"contact_damping"`
}

func DefaultConfig() Config {
	return Config{
		Mode: "auto",
		Budget: Budget{
			MaxExactFrames:  260,
			MaxExactJoints:  34,
			MaxExactWork:    6000,
			MaxSolveFrames:  96,
			MaxSmoothWindow: 11,
			MinSmoothWindow: 3,
		},
		SmoothWindow:      9,
		MaxIterations:     24,
		AngleToleranceDeg: 0.25,
		ReachTolerance:    0.005,
		GroundY:           0,
		ContactDamping:    1,
	}
}

type Plan struct {
	RequestedMode      string   `json:"requested_mode"`
	Mode               string   `json:"mode"`
	Strategy           string   `json:"strategy"`
	Reason             []string `json:"reason"`
	Stride             int      `json:"stride"`
	SolveFrames        int      `json:"solve_frames"`
	SolveFrameIndices  []int    `json:"solve_frame_indices"`
	FrameCount         int      `json:"frame_count"`
	JointCount         int      `json:"joint_count"`
	WorkUnits          int      `json:"work_units"`
	BudgetWork         int      `json:"budget_work"`
	SmoothWindow       int      `json:"smooth_window"`
	ShortenedSmoothing bool     `json:"shortened_smoothing"`
	Interpolation      string   `json:"interpolation"`
}

type MappedJoint struct {
	Target   string  `json:"target"`
	Source   string  `json:"source"`
	Strategy string  `json:"strategy"`
	Scale    float64 `json:"scale"`
	Align    Quat    `json:"rest_alignment"`
	Reason   string  `json:"reason,omitempty"`
}

type UnmappedSource struct {
	Joint  string `json:"joint"`
	Reason string `json:"reason"`
}

type MappingTrace struct {
	Rules           []MappedJoint    `json:"rules"`
	UnmappedSources []UnmappedSource `json:"unmapped_sources"`
	RootScale       float64          `json:"root_scale"`
	SourceHeight    float64          `json:"source_height"`
	TargetHeight    float64          `json:"target_height"`
}

type Decision struct {
	Frame      int     `json:"frame"`
	Constraint string  `json:"constraint"`
	Action     string  `json:"action"`
	Reason     string  `json:"reason"`
	Before     float64 `json:"before,omitempty"`
	After      float64 `json:"after,omitempty"`
	Joint      string  `json:"joint,omitempty"`
}

type FrameStats struct {
	Iterations        int     `json:"iterations"`
	AngleViolationDeg float64 `json:"angle_violation_deg"`
	ReachViolation    float64 `json:"reach_violation"`
	ContactDrift      float64 `json:"contact_drift"`
	Residual          float64 `json:"residual"`
	Interpolated      bool    `json:"interpolated"`
}

type FrameResult struct {
	Frame           int        `json:"frame"`
	Rotations       []Quat     `json:"rotations"`
	RootTranslation Vec3       `json:"root_translation"`
	Contacts        []string   `json:"contacts"`
	Stats           FrameStats `json:"stats"`
	Decisions       []Decision `json:"decisions"`
	State           string     `json:"state"`
}

type Summary struct {
	MaxResidual       float64 `json:"max_residual"`
	MaxAngleViolation float64 `json:"max_angle_violation_deg"`
	MaxReachViolation float64 `json:"max_reach_violation"`
	MaxContactDrift   float64 `json:"max_contact_drift"`
	DecisionCount     int     `json:"decision_count"`
	FailedFrames      []int   `json:"failed_frames"`
}

type Animation struct {
	ClipID           string                       `json:"clip_id"`
	TargetID         string                       `json:"target_id"`
	AlgorithmVersion string                       `json:"algorithm_version"`
	ParameterVersion string                       `json:"parameter_version"`
	Plan             Plan                         `json:"plan"`
	Mapping          MappingTrace                 `json:"mapping"`
	Frames           []FrameResult                `json:"frames"`
	ContactFrames    []int                        `json:"contact_frames"`
	ContactSpans     map[string][]ContactInterval `json:"contact_spans"`
	RootStart        Vec3                         `json:"root_start"`
	RootEnd          Vec3                         `json:"root_end"`
	Summary          Summary                      `json:"summary"`
	ResumedFrames    int                          `json:"resumed_frames"`
	SnapshotAction   string                       `json:"snapshot_action"`
	SnapshotReason   string                       `json:"snapshot_reason,omitempty"`
}

type ManifestEntry struct {
	ID       string            `json:"id"`
	Path     string            `json:"path"`
	Kind     string            `json:"kind"`
	SHA256   string            `json:"sha256"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type Manifest struct {
	GeneratorVersion string          `json:"generator_version"`
	SchemaVersion    string          `json:"schema_version"`
	Seed             int64           `json:"seed"`
	Files            []ManifestEntry `json:"files"`
}

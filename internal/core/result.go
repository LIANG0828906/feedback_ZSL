package core

type ContactDrift struct {
	Frame    int     `json:"frame"`
	Foot     string  `json:"foot"`
	Joint    string  `json:"joint"`
	Anchor   Vec3    `json:"anchor"`
	Position Vec3    `json:"position"`
	Drift    float64 `json:"drift"`
}

type FrameContact struct {
	Active             []string        `json:"active"`
	Anchors            map[string]Vec3 `json:"anchors"`
	Drifts             []ContactDrift  `json:"drifts"`
	MaxDrift           float64         `json:"max_drift"`
	Correction         Vec3            `json:"correction"`
	SmoothedCorrection Vec3            `json:"smoothed_correction"`
}

type JointDiagnostic struct {
	Joint              string  `json:"joint"`
	Parent             string  `json:"parent"`
	Depth              int     `json:"depth"`
	SourceJoint        string  `json:"source_joint,omitempty"`
	MappingStrategy    string  `json:"mapping_strategy,omitempty"`
	Iterations         int     `json:"iterations"`
	QuantStep          float64 `json:"quant_step"`
	QuantResidual      float64 `json:"quant_residual"`
	QuantBound         float64 `json:"quant_bound"`
	ScaleRatio         float64 `json:"scale_ratio"`
	ScaleResidual      float64 `json:"scale_residual"`
	ScaleBound         float64 `json:"scale_bound"`
	FKObservedError    float64 `json:"fk_observed_error"`
	FKBound            float64 `json:"fk_bound"`
	AngleResidual      float64 `json:"angle_residual_radians"`
	ReachResidual      float64 `json:"reach_residual"`
	ProjectionResidual float64 `json:"projection_residual"`
	ProjectionBound    float64 `json:"projection_bound"`
	TotalBound         float64 `json:"total_bound"`
}

type FrameStats struct {
	Iterations        int     `json:"iterations"`
	MaxAngleViolation float64 `json:"max_angle_violation"`
	MaxReachViolation float64 `json:"max_reach_violation"`
	MaxViolation      float64 `json:"max_violation"`
	Converged         bool    `json:"converged"`
	MaxContactDrift   float64 `json:"max_contact_drift"`
	ContactViolations int     `json:"contact_violations"`
	Depth             int     `json:"depth"`
	JointCount        int     `json:"joint_count"`
}

type FrameResult struct {
	ClipID         string            `json:"clip_id"`
	TargetID       string            `json:"target_id"`
	Frame          int               `json:"frame"`
	State          string            `json:"state"`
	Rotations      []Quat            `json:"rotations"`
	Root           Vec3              `json:"root"`
	WorldPositions []Vec3            `json:"world_positions,omitempty"`
	Contacts       FrameContact      `json:"contacts"`
	Stats          FrameStats        `json:"stats"`
	Diagnostics    []JointDiagnostic `json:"diagnostics"`
	ContentHash    string            `json:"content_hash"`
	ParamsVersion  string            `json:"params_version"`
	AlgoVersion    string            `json:"algo_version"`
}

type DiffMismatch struct {
	Frame int    `json:"frame"`
	Joint string `json:"joint,omitempty"`
	Field string `json:"field"`
	A     any    `json:"a,omitempty"`
	B     any    `json:"b,omitempty"`
}

type DiffReport struct {
	OK             bool          `json:"ok"`
	PathA          string        `json:"path_a"`
	PathB          string        `json:"path_b"`
	FirstMismatch  *DiffMismatch `json:"first_mismatch,omitempty"`
	MismatchCount  int           `json:"mismatch_count"`
	ComparedFrames int           `json:"compared_frames"`
}

type PathSummary struct {
	Path            string      `json:"path"`
	Frames          int         `json:"frames"`
	Committed       int         `json:"committed"`
	DBPath          string      `json:"db_path,omitempty"`
	FirstHash       string      `json:"first_hash,omitempty"`
	LastHash        string      `json:"last_hash,omitempty"`
	MaxViolation    float64     `json:"max_violation"`
	MaxContactDrift float64     `json:"max_contact_drift"`
	Comparison      *DiffReport `json:"comparison,omitempty"`
}

type RunReport struct {
	OK              bool          `json:"ok"`
	ClipID          string        `json:"clip_id"`
	TargetID        string        `json:"target_id"`
	DBPath          string        `json:"db_path"`
	Parameters      Parameters    `json:"parameters"`
	ParamsVersion   string        `json:"params_version"`
	AlgoVersion     string        `json:"algo_version"`
	Mapping         *Mapping      `json:"mapping"`
	Frames          []FrameResult `json:"frames"`
	Paths           []PathSummary `json:"paths"`
	ThreeWay        *DiffReport   `json:"three_way,omitempty"`
	Truth           TruthReport   `json:"truth"`
	ContactCheck    ContactCheck  `json:"contact_check"`
	Summary         ReportSummary `json:"summary"`
	VerificationLog []string      `json:"verification_log"`
}

type ContactCheck struct {
	OK                 bool                  `json:"ok"`
	MaxDrift           float64               `json:"max_drift"`
	DriftLimit         float64               `json:"drift_limit"`
	FrameTolerance     int                   `json:"frame_tolerance"`
	ExpectedIntervals  map[string][]Interval `json:"expected_intervals"`
	ObservedIntervals  map[string][]Interval `json:"observed_intervals"`
	IntervalFrameDiffs map[string]int        `json:"interval_frame_diffs"`
	DriftViolations    []ContactDrift        `json:"drift_violations"`
}

type ReportSummary struct {
	Frames          int     `json:"frames"`
	Joints          int     `json:"joints"`
	MaxIterations   int     `json:"max_iterations"`
	MaxViolation    float64 `json:"max_violation"`
	MaxContactDrift float64 `json:"max_contact_drift"`
	MaxTotalBound   float64 `json:"max_total_bound"`
	UnmappedSource  int     `json:"unmapped_source"`
	DegradedSource  int     `json:"degraded_source"`
	ConsistencyOK   bool    `json:"consistency_ok"`
	RecoveryOK      bool    `json:"recovery_ok"`
}

type FrameContent struct {
	Frame       int               `json:"frame"`
	Rotations   []Quat            `json:"rotations"`
	Root        Vec3              `json:"root"`
	Contacts    FrameContact      `json:"contacts"`
	Stats       FrameStats        `json:"stats"`
	Diagnostics []JointDiagnostic `json:"diagnostics"`
}

func (r *FrameResult) ComputeHash() string {
	return CanonicalHash(FrameContent{
		Frame:       r.Frame,
		Rotations:   r.Rotations,
		Root:        r.Root,
		Contacts:    r.Contacts,
		Stats:       r.Stats,
		Diagnostics: r.Diagnostics,
	})
}

package model

import (
	"encoding/json"
	"fmt"
)

// ErrorCode is the stable machine-readable validation/execution code.
type ErrorCode string

const (
	ESchema ErrorCode = "E_SCHEMA"
	EDim    ErrorCode = "E_DIM"
	ERef    ErrorCode = "E_REF"
	EPara   ErrorCode = "E_PARA"
	EVis    ErrorCode = "E_VIS"
	ELoad   ErrorCode = "E_LOAD"
	EEst    ErrorCode = "E_EST"
	EState  ErrorCode = "E_STATE"
	EHash   ErrorCode = "E_HASH"
)

// Error carries a stable code plus the field and values needed by diagnostics.
type Error struct {
	Code    ErrorCode      `json:"code"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message"`
	Allowed string         `json:"allowed,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s %s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code ErrorCode, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func ParameterError(field, allowed, format string, args ...any) *Error {
	return &Error{Code: EPara, Field: field, Allowed: allowed, Message: fmt.Sprintf(format, args...)}
}

// Vec3 is a deterministic fixed-order three-component vector.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Rect2 is an axis-aligned coverage rectangle in terrain XZ space.
type Rect2 struct {
	MinX float64 `json:"min_x"`
	MinZ float64 `json:"min_z"`
	MaxX float64 `json:"max_x"`
	MaxZ float64 `json:"max_z"`
}

// QuantParameters contains all declared inputs that may change culling precision.
type QuantParameters struct {
	ShadowResolution  int     `json:"shadow_resolution"`
	LightAzimuthDeg   float64 `json:"light_azimuth_deg"`
	LightElevationDeg float64 `json:"light_elevation_deg"`
	CameraDepthRes    int     `json:"camera_depth_resolution"`
	SampleSpacing     float64 `json:"sample_spacing"`
	FixedPointStep    float64 `json:"fixed_point_step"`
	ViewportWidth     int     `json:"viewport_width"`
	ViewportHeight    int     `json:"viewport_height"`
	MaxWorkers        int     `json:"max_workers"`
	DepthBufferLimit  int     `json:"depth_buffer_limit"`
}

// Budgets declares streaming and state-machine service levels.
type Budgets struct {
	LoadSlots              int   `json:"load_slots"`
	MemoryBytes            int64 `json:"memory_bytes"`
	MaxResidencyDelay      int   `json:"max_residency_delay_samples"`
	MaxParallelTasks       int   `json:"max_parallel_tasks"`
	DepthBuffersPerTask    int   `json:"depth_buffer_entries_per_thread"`
	RecomputeOnParamChange bool  `json:"recompute_on_param_change"`
}

type Terrain struct {
	WidthCells  int       `json:"width_cells"`
	DepthCells  int       `json:"depth_cells"`
	CellSize    float64   `json:"cell_size"`
	HeightScale float64   `json:"height_scale"`
	Heights     []float64 `json:"heights"`
}

type Chunk struct {
	ID           int     `json:"id"`
	Label        string  `json:"label"`
	Rect         Rect2   `json:"coverage"`
	Complexity   int     `json:"geometry_complexity"`
	LoadCost     int     `json:"load_cost"`
	MemoryBytes  int64   `json:"memory_bytes"`
	DistanceBias float64 `json:"distance_bias,omitempty"`
	OccluderRefs []int   `json:"occluder_refs"`
	SamplePoints []Vec3  `json:"sample_points"`
}

type Occluder struct {
	ID     int    `json:"id"`
	Label  string `json:"label"`
	Bounds Box3   `json:"bounds"`
}

type Box3 struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

type CameraSample struct {
	Index    int     `json:"index"`
	TimeMs   int64   `json:"logical_time_ms"`
	Position Vec3    `json:"position"`
	Target   Vec3    `json:"target"`
	FovDeg   float64 `json:"fov_deg"`
	Near     float64 `json:"near"`
	Far      float64 `json:"far"`
	PoseKey  string  `json:"pose_key,omitempty"`
}

type ProjectionTruth struct {
	SampleIndex int     `json:"sample_index"`
	ChunkID     int     `json:"chunk_id"`
	PointIndex  int     `json:"point_index"`
	ScreenX     float64 `json:"screen_x"`
	ScreenY     float64 `json:"screen_y"`
	ViewDepth   float64 `json:"view_depth"`
}

type ChunkTruth struct {
	ChunkID             int     `json:"chunk_id"`
	Verdict             Verdict `json:"verdict"`
	ReferenceOccluderID int     `json:"reference_occluder_id"`
	ExactMargin         float64 `json:"exact_margin"`
}

type SampleTruth struct {
	SampleIndex  int           `json:"sample_index"`
	TimeMs       int64         `json:"logical_time_ms"`
	Chunks       []ChunkTruth  `json:"chunks"`
	VisibleIDs   []int         `json:"visible_ids"`
	OccludedIDs  []int         `json:"occluded_ids"`
	UncertainIDs []int         `json:"uncertain_ids"`
	ResidentIDs  []int         `json:"resident_ids"`
	LoadEvents   []LoadEvent   `json:"load_events"`
	UnloadEvents []UnloadEvent `json:"unload_events"`
}

type Truth struct {
	GeneratorSeed int64             `json:"generator_seed"`
	Projection    []ProjectionTruth `json:"screen_projections"`
	Samples       []SampleTruth     `json:"samples"`
}

type Project struct {
	SchemaVersion string          `json:"schema_version"`
	Seed          int64           `json:"seed"`
	GeneratedAt   string          `json:"generated_at"`
	Parameters    QuantParameters `json:"parameters"`
	Budgets       Budgets         `json:"budgets"`
	Terrain       Terrain         `json:"terrain"`
	Occluders     []Occluder      `json:"occluders"`
	Chunks        []Chunk         `json:"chunks"`
	CameraTrack   []CameraSample  `json:"camera_track"`
	Truth         Truth           `json:"truth"`
}

type Verdict string

const (
	VerdictVisible   Verdict = "visible"
	VerdictOccluded  Verdict = "occluded"
	VerdictUncertain Verdict = "uncertain"
)

type LoadEvent struct {
	SampleIndex int    `json:"sample_index"`
	ChunkID     int    `json:"chunk_id"`
	Reason      string `json:"reason"`
}

type UnloadEvent struct {
	SampleIndex int    `json:"sample_index"`
	ChunkID     int    `json:"chunk_id"`
	Reason      string `json:"reason"`
}

// ChunkVerdict is the per-chunk diagnostic result at one sample.
type ChunkVerdict struct {
	ChunkID             int        `json:"chunk_id"`
	Coarse              string     `json:"coarse"`
	Fine                Verdict    `json:"fine"`
	Final               Verdict    `json:"final"`
	ReferenceOccluderID int        `json:"reference_occluder_id"`
	DecisionMargin      float64    `json:"decision_margin"`
	LowerBound          float64    `json:"lower_bound"`
	UpperBound          float64    `json:"upper_bound"`
	ScreenBounds        [4]float64 `json:"screen_bounds"`
	InFrustum           bool       `json:"in_frustum"`
}

type ErrorReport struct {
	SampleIndex            int     `json:"sample_index"`
	ShadowQuantization     float64 `json:"shadow_quantization"`
	DepthTruncation        float64 `json:"depth_truncation"`
	ProjectionQuantization float64 `json:"projection_quantization"`
	SamplingPropagation    float64 `json:"sampling_propagation"`
	StreamingDelayImpact   int     `json:"streaming_delay_impact"`
	CumulativeUpperBound   float64 `json:"cumulative_upper_bound"`
	EstimatedSetDifference int     `json:"estimated_set_difference_upper_bound"`
	Sufficient             bool    `json:"sufficient"`
}

type ResourceWait struct {
	SampleIndex int    `json:"sample_index"`
	ChunkID     int    `json:"chunk_id"`
	Reason      string `json:"reason"`
}

type SampleResult struct {
	SampleIndex       int            `json:"sample_index"`
	LogicalTimeMs     int64          `json:"logical_time_ms"`
	PoseKey           string         `json:"pose_key"`
	State             string         `json:"state"`
	ShadowResolution  int            `json:"shadow_resolution"`
	ShadowVersion     string         `json:"shadow_version"`
	CoarseOccludedIDs []int          `json:"coarse_occluded_ids"`
	Verdicts          []ChunkVerdict `json:"verdicts"`
	VisibleIDs        []int          `json:"visible_ids"`
	OccludedIDs       []int          `json:"occluded_ids"`
	UncertainIDs      []int          `json:"uncertain_ids"`
	ResidentIDs       []int          `json:"resident_ids"`
	QueuedIDs         []int          `json:"queued_ids"`
	CancelledIDs      []int          `json:"cancelled_ids,omitempty"`
	LoadEvents        []LoadEvent    `json:"load_events"`
	UnloadEvents      []UnloadEvent  `json:"unload_events"`
	Delayed           []DelayedChunk `json:"delayed_chunks"`
	ErrorReport       ErrorReport    `json:"error_report"`
	ResourceWaits     []ResourceWait `json:"resource_waits"`
	RevisitHit        bool           `json:"revisit_hit"`
	CameraOffset      int            `json:"camera_offset"`
	ContentHash       string         `json:"content_hash"`
}

type DelayedChunk struct {
	ChunkID         int    `json:"chunk_id"`
	RequestedSample int    `json:"requested_sample"`
	Reason          string `json:"reason"`
	CompletedSample int    `json:"completed_sample,omitempty"`
	DelaySamples    int    `json:"delay_samples"`
}

type PathComparison struct {
	Identical    bool   `json:"identical"`
	FirstSample  int    `json:"first_sample"`
	FirstChunkID int    `json:"first_chunk_id,omitempty"`
	Field        string `json:"field,omitempty"`
	RealtimeHash string `json:"realtime_hash,omitempty"`
	ResumeHash   string `json:"resume_hash,omitempty"`
	ReplayHash   string `json:"replay_hash,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

type PathReport struct {
	Name           string         `json:"name"`
	Results        []SampleResult `json:"results"`
	CheckpointPath string         `json:"checkpoint_path,omitempty"`
}

type DiagnosticReport struct {
	OK                 bool           `json:"ok"`
	CommittedSamples   int            `json:"committed_samples"`
	TotalSamples       int            `json:"total_samples"`
	Project            Project        `json:"project"`
	Realtime           PathReport     `json:"realtime"`
	Resume             PathReport     `json:"resume"`
	Replay             PathReport     `json:"replay"`
	Comparison         PathComparison `json:"comparison"`
	TruthMatched       bool           `json:"truth_matched"`
	VerificationErrors []string       `json:"verification_errors,omitempty"`
	ShadowFrames       []ShadowFrame  `json:"shadow_frames,omitempty"`
}

type ShadowFrame struct {
	SampleIndex int       `json:"sample_index"`
	Resolution  int       `json:"resolution"`
	Depth       []float64 `json:"depth"`
}

// CanonicalJSON is used for stable hashes and SQLite payload digests.
func CanonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

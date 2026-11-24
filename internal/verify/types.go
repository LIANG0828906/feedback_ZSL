package verify

import (
	"screenverify/internal/model"
)

type EvidenceType string

const (
	EvWatermark EvidenceType = "watermark_decode"
	EvTimeline  EvidenceType = "timeline_continuity"
	EvResidual  EvidenceType = "residual_energy"
	EvNoise     EvidenceType = "noise_variance"
	EvEdge      EvidenceType = "edge_consistency"
	EvDecision  EvidenceType = "decision"
)

type Params struct {
	GridX              int         `json:"grid_x"`
	GridY              int         `json:"grid_y"`
	WatermarkThreshold float64     `json:"watermark_threshold"`
	WatermarkRegion    *model.Rect `json:"watermark_region,omitempty"`
	ResidualWindow     int         `json:"residual_window"`
	BlockSize          int         `json:"block_size"`
	TamperThreshold    float64     `json:"tamper_threshold"`
	ResidualZThreshold float64     `json:"residual_z_threshold"`
	NoiseZThreshold    float64     `json:"noise_z_threshold"`
	EdgeThreshold      float64     `json:"edge_threshold"`
	FrameTolerance     int         `json:"frame_tolerance"`
	BatchFrames        int         `json:"batch_frames"`
}

func DefaultParams() Params {
	return Params{
		GridX:              8,
		GridY:              4,
		WatermarkThreshold: 0.72,
		ResidualWindow:     3,
		BlockSize:          16,
		TamperThreshold:    0.55,
		ResidualZThreshold: 3.0,
		NoiseZThreshold:    4.0,
		EdgeThreshold:      0.72,
		FrameTolerance:     1,
		BatchFrames:        4,
	}
}

type RuntimeConfig struct {
	Parallelism           int          `json:"parallelism"`
	BufferPixelsPerThread int64        `json:"buffer_pixels_per_thread"`
	FailureFrame          int          `json:"-"`
	FailureType           EvidenceType `json:"-"`
	FailureArmed          bool         `json:"-"`
}

func DefaultRuntime() RuntimeConfig {
	return RuntimeConfig{Parallelism: 2, BufferPixelsPerThread: 24576}
}

type BitEvidence struct {
	Index      int     `json:"index"`
	Row        int     `json:"row"`
	Col        int     `json:"col"`
	X          int     `json:"x"`
	Y          int     `json:"y"`
	Bit        int     `json:"bit"`
	Luma       float64 `json:"luma"`
	Confidence float64 `json:"confidence"`
}

type WatermarkEvidence struct {
	Frame          int           `json:"frame"`
	Region         model.Rect    `json:"region"`
	GridX          int           `json:"grid_x"`
	GridY          int           `json:"grid_y"`
	Threshold      float64       `json:"threshold"`
	DecodedFrame   int           `json:"decoded_frame"`
	Decoded        bool          `json:"decoded"`
	MeanConfidence float64       `json:"mean_confidence"`
	MinConfidence  float64       `json:"min_confidence"`
	Missing        bool          `json:"missing"`
	Misaligned     bool          `json:"misaligned"`
	Bits           []BitEvidence `json:"bits"`
}

type TimelineEvidence struct {
	Frame            int     `json:"frame"`
	ExpectedFrame    int     `json:"expected_frame"`
	DecodedFrame     int     `json:"decoded_frame"`
	PreviousDecoded  int     `json:"previous_decoded"`
	HasPrevious      bool    `json:"has_previous"`
	Jump             bool    `json:"jump"`
	PositionMismatch bool    `json:"position_mismatch"`
	Missing          bool    `json:"missing"`
	Misaligned       bool    `json:"misaligned"`
	Anomaly          bool    `json:"anomaly"`
	Score            float64 `json:"score"`
}

type ResidualEvidence struct {
	Frame        int     `json:"frame"`
	Window       int     `json:"window"`
	Energy       float64 `json:"energy"`
	PreviousMean float64 `json:"previous_mean"`
	PreviousStd  float64 `json:"previous_std"`
	ZScore       float64 `json:"z_score"`
	Score        float64 `json:"score"`
}

type NoiseBlock struct {
	BlockX      int     `json:"block_x"`
	BlockY      int     `json:"block_y"`
	X           int     `json:"x"`
	Y           int     `json:"y"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	Variance    float64 `json:"variance"`
	RawVariance float64 `json:"raw_variance"`
	ZScore      float64 `json:"z_score"`
	Ratio       float64 `json:"ratio"`
}

type NoiseEvidence struct {
	Frame           int          `json:"frame"`
	BlockSize       int          `json:"block_size"`
	Blocks          []NoiseBlock `json:"blocks"`
	MedianVariance  float64      `json:"median_variance"`
	MAD             float64      `json:"mad"`
	Dispersion      float64      `json:"dispersion"`
	MaxZScore       float64      `json:"max_z_score"`
	OutlierFraction float64      `json:"outlier_fraction"`
	Score           float64      `json:"score"`
}

type EdgePrimitive struct {
	Label       string     `json:"label"`
	BBox        model.Rect `json:"bbox"`
	Consistency float64    `json:"consistency"`
	Histogram   []float64  `json:"histogram"`
}

type EdgeEvidence struct {
	Frame           int             `json:"frame"`
	Bins            int             `json:"bins"`
	Primitives      []EdgePrimitive `json:"primitives"`
	MeanConsistency float64         `json:"mean_consistency"`
	MinConsistency  float64         `json:"min_consistency"`
	Score           float64         `json:"score"`
}

type DecisionEvidence struct {
	Frame          int      `json:"frame"`
	Tampered       bool     `json:"tampered"`
	Score          float64  `json:"score"`
	Confidence     float64  `json:"confidence"`
	Threshold      float64  `json:"threshold"`
	WatermarkScore float64  `json:"watermark_score"`
	ResidualScore  float64  `json:"residual_score"`
	NoiseScore     float64  `json:"noise_score"`
	EdgeScore      float64  `json:"edge_score"`
	Reasons        []string `json:"reasons"`
}

type Interval struct {
	StartFrame    int            `json:"start_frame"`
	EndFrame      int            `json:"end_frame"`
	Kind          string         `json:"kind"`
	FrameCount    int            `json:"frame_count"`
	EvidenceTypes []EvidenceType `json:"evidence_types"`
}

type CommitItem struct {
	Seq          int64        `json:"seq"`
	BatchID      string       `json:"batch_id"`
	Ordinal      int          `json:"ordinal"`
	Frame        int          `json:"frame"`
	EvidenceType EvidenceType `json:"evidence_type"`
	CacheKey     string       `json:"cache_key"`
}

type QueueWait struct {
	Frame        int          `json:"frame"`
	EvidenceType EvidenceType `json:"evidence_type"`
	Pixels       int64        `json:"pixels"`
	Reason       string       `json:"reason"`
	WaitedMS     int64        `json:"waited_ms"`
}

type Stats struct {
	RecomputeCount int         `json:"recompute_count"`
	HitCount       int         `json:"hit_count"`
	Parallelism    int         `json:"parallelism"`
	QueueWaits     []QueueWait `json:"queue_waits"`
	FailureCount   int         `json:"failure_count"`
	RetryCount     int         `json:"retry_count"`
}

type Report struct {
	Clip               string              `json:"clip"`
	TruthFingerprint   string              `json:"truth_fingerprint"`
	ContentRoot        string              `json:"content_root"`
	AlgorithmVersion   string              `json:"algorithm_version"`
	Params             Params              `json:"params"`
	ParamVersion       string              `json:"param_version"`
	Decisions          []DecisionEvidence  `json:"decisions"`
	Watermarks         []WatermarkEvidence `json:"watermarks"`
	Timelines          []TimelineEvidence  `json:"timelines"`
	Residuals          []ResidualEvidence  `json:"residuals"`
	Noises             []NoiseEvidence     `json:"noises"`
	Edges              []EdgeEvidence      `json:"edges"`
	Intervals          []Interval          `json:"intervals"`
	CommitSequence     []CommitItem        `json:"commit_sequence"`
	SubmissionSequence []CommitItem        `json:"submission_sequence"`
	DependencyGraph    map[string][]string `json:"dependency_graph"`
	Failures           []FailureRecord     `json:"failures,omitempty"`
	Invalidations      []Invalidation      `json:"invalidations,omitempty"`
	Stats              Stats               `json:"stats"`
	Recovery           *RecoveryInfo       `json:"recovery,omitempty"`
}

type FailureRecord struct {
	Frame        int          `json:"frame"`
	EvidenceType EvidenceType `json:"evidence_type"`
	Code         string       `json:"code"`
	Message      string       `json:"message"`
	Resolved     bool         `json:"resolved"`
}

type RecoveryInfo struct {
	Recovered             bool   `json:"recovered"`
	SnapshotDiscarded     bool   `json:"snapshot_discarded"`
	Reason                string `json:"reason,omitempty"`
	CompletedCheckpoints  int    `json:"completed_checkpoints"`
	LoadedEvidenceEntries int    `json:"loaded_evidence_entries"`
}

type Invalidation struct {
	Frame        int          `json:"frame"`
	EvidenceType EvidenceType `json:"evidence_type"`
	CacheKey     string       `json:"cache_key,omitempty"`
	Reason       string       `json:"reason"`
}

type Snapshot struct {
	Decisions          []DecisionEvidence `json:"decisions"`
	Noises             []NoiseEvidence    `json:"noises"`
	Intervals          []Interval         `json:"intervals"`
	CommitSequence     []CommitItem       `json:"commit_sequence"`
	SubmissionSequence []CommitItem       `json:"submission_sequence"`
}

type Difference struct {
	Code         string `json:"code"`
	Frame        int    `json:"frame,omitempty"`
	BlockX       int    `json:"block_x,omitempty"`
	BlockY       int    `json:"block_y,omitempty"`
	EvidenceType string `json:"evidence_type,omitempty"`
	Field        string `json:"field,omitempty"`
	Message      string `json:"message"`
	PathA        string `json:"path_a,omitempty"`
	PathB        string `json:"path_b,omitempty"`
}

func (d *Difference) Error() string {
	return d.Message
}

package dialogue

// Algorithm/version identifiers participate in cache keys and snapshot
// validation. Bumping an algorithm invalidates old checkpoints deliberately.
const (
	AlgorithmVersion = "acoustic-calib-int-v1"
	QuantVersion     = "quant-sample-frame-v1"
	CacheSchemaVer   = "checkpoint-v1"
	FixedSeed        = int64(42042)

	DefaultSampleRate  = 24_000
	DefaultFrameRate   = 25
	TransientThreshold = 18_000
	SilenceRMSMicro    = 2_000
)

// FieldTrace records a source-field mapping, default fill or conversion.
type FieldTrace struct {
	Path          string      `json:"path"`
	SourceVersion int         `json:"source_version,omitempty"`
	TargetVersion int         `json:"target_version,omitempty"`
	SourceField   string      `json:"source_field,omitempty"`
	Rule          string      `json:"rule"`
	OldValue      any         `json:"old_value,omitempty"`
	NewValue      any         `json:"new_value,omitempty"`
	DefaultValue  any         `json:"default_value,omitempty"`
	Truncation    *Truncation `json:"truncation,omitempty"`
}

// LossItem is one explicit E_LOSSY downgrade report entry.
type LossItem struct {
	Code        string `json:"code"`
	Path        string `json:"path"`
	Original    any    `json:"original,omitempty"`
	Disposition string `json:"disposition"`
	Message     string `json:"message"`
}

// LossReport contains all non-fatal downgrade losses for an export.
type LossReport struct {
	FromVersion int        `json:"from_version"`
	ToVersion   int        `json:"to_version"`
	Losses      []LossItem `json:"losses"`
}

func (r *LossReport) HasLoss() bool { return r != nil && len(r.Losses) > 0 }

// SceneGroup preserves v3 nested scene organization.
type SceneGroup struct {
	Name     string        `json:"name"`
	ClipIDs  []string      `json:"clip_ids,omitempty"`
	Children []*SceneGroup `json:"children,omitempty"`
}

// Marker is an optional calibration/transient marker.
type Marker struct {
	ID      string `json:"id"`
	ClipID  string `json:"clip_id"`
	Kind    string `json:"kind"`
	Sample  int64  `json:"sample"`
	FrameNo int    `json:"frame_no"`
	Value   int64  `json:"value,omitempty"`
}

// Clip is the unified internal dialogue clip model.
type Clip struct {
	ID                  string       `json:"id"`
	ScenePath           []string     `json:"scene_path"`
	StartSample         int64        `json:"start_sample"`
	Samples             []int16      `json:"-"`
	DeclaredSampleCount int          `json:"declared_sample_count"`
	GainBP              int64        `json:"gain_bp"`
	ToneHz              int          `json:"tone_hz,omitempty"`
	TransientSample     int64        `json:"transient_sample,omitempty"`
	AudioHash           string       `json:"audio_hash"`
	ForceStageFailure   bool         `json:"force_stage_failure,omitempty"`
	Truth               *ClipTruth   `json:"truth,omitempty"`
	LoadTraces          []FieldTrace `json:"load_traces,omitempty"`
}

// Project is the version-independent dialogue engineering model.
type Project struct {
	SchemaVersion    int          `json:"schema_version"`
	SchemaID         string       `json:"schema_id"`
	SourceFile       string       `json:"source_file"`
	DialogueID       string       `json:"dialogue_id"`
	Seed             int64        `json:"seed"`
	SampleRate       int          `json:"sample_rate"`
	FrameRate        int          `json:"frame_rate"`
	SessionGainBP    int64        `json:"session_gain_bp"`
	TargetAmpMicro   int64        `json:"target_amp_micro"`
	TargetLoudMDB    int64        `json:"target_loud_mdb"`
	MaxAdjustBP      int64        `json:"max_adjust_bp"`
	TotalBudgetBP    int64        `json:"total_budget_bp"`
	SceneTree        []SceneGroup `json:"scene_tree,omitempty"`
	Markers          []Marker     `json:"markers,omitempty"`
	Clips            []*Clip      `json:"clips"`
	Traces           []FieldTrace `json:"traces,omitempty"`
	AlgorithmVersion string       `json:"algorithm_version"`
	QuantVersion     string       `json:"quant_version"`
}

// FrameInterval identifies one quantized logical frame on the timeline.
type FrameInterval struct {
	ClipID           string `json:"clip_id"`
	FrameNo          int    `json:"frame_no"`
	RangeID          string `json:"range_id"`
	FrameStartSample int64  `json:"frame_start_sample"`
	FrameEndSample   int64  `json:"frame_end_sample"`
}

// FrameFeature contains integer fixed-point acoustic features.
type FrameFeature struct {
	ClipID           string       `json:"clip_id"`
	FrameNo          int          `json:"frame_no"`
	RangeID          string       `json:"range_id"`
	FrameStartSample int64        `json:"frame_start_sample"`
	FrameEndSample   int64        `json:"frame_end_sample"`
	EvaluatedSamples int          `json:"evaluated_samples"`
	EnergyFixed      int64        `json:"energy_fixed"`
	RMSMicro         int64        `json:"rms_micro"`
	ZCRBP            int64        `json:"zcr_bp"`
	PeakSample       int64        `json:"peak_sample"`
	PeakOffset       int          `json:"peak_offset"`
	PeakAmp          int          `json:"peak_amp"`
	Transient        bool         `json:"transient"`
	Truncations      []Truncation `json:"truncations"`
}

// CalibratedFrame is the loudness result for one frame after clip adjustment.
type CalibratedFrame struct {
	ClipID         string       `json:"clip_id"`
	FrameNo        int          `json:"frame_no"`
	RangeID        string       `json:"range_id"`
	RawRMSMicro    int64        `json:"raw_rms_micro"`
	LoudBeforeMDB  int64        `json:"loud_before_mdb"`
	LoudAfterMDB   int64        `json:"loud_after_mdb"`
	TargetLoudMDB  int64        `json:"target_loud_mdb"`
	GainAdjustBP   int64        `json:"gain_adjust_bp"`
	CombinedGainBP int64        `json:"combined_gain_bp"`
	Silent         bool         `json:"silent"`
	Truncations    []Truncation `json:"truncations,omitempty"`
}

// SampleTruth is an analytically known fixed-point sample observation.
type SampleTruth struct {
	SampleNo   int64 `json:"sample_no"`
	ClipSample int   `json:"clip_sample"`
	PCM        int   `json:"pcm"`
	AmpMicro   int64 `json:"amp_micro"`
	EnvelopeBP int64 `json:"envelope_bp"`
	FrameNo    int   `json:"frame_no"`
}

// ClipTruth is generator-owned expected data.
type ClipTruth struct {
	ClipID              string            `json:"clip_id"`
	AudioHash           string            `json:"audio_hash"`
	TransientSample     int64             `json:"transient_sample"`
	TransientAmp        int               `json:"transient_amp"`
	GainAdjustBP        int64             `json:"gain_adjust_bp"`
	ReachableMaxLoudMDB int64             `json:"reachable_max_loud_mdb"`
	SampleChecks        []SampleTruth     `json:"sample_checks"`
	Features            []FrameFeature    `json:"features"`
	Calibration         []CalibratedFrame `json:"calibration"`
}

// FrameResult bundles a feature and its downstream calibration.
type FrameResult struct {
	ClipID         string          `json:"clip_id"`
	FrameNo        int             `json:"frame_no"`
	RangeID        string          `json:"range_id"`
	Feature        FrameFeature    `json:"feature"`
	Calibration    CalibratedFrame `json:"calibration"`
	FeatureKey     string          `json:"feature_key"`
	CalibrationKey string          `json:"calibration_key"`
}

// LoudnessGap reports one clip's upper reachable loudness and target gap.
type LoudnessGap struct {
	ClipID              string `json:"clip_id"`
	Reachable           bool   `json:"reachable"`
	CurrentLoudMDB      int64  `json:"current_loud_mdb"`
	ReachableMaxLoudMDB int64  `json:"reachable_max_loud_mdb"`
	TargetLoudMDB       int64  `json:"target_loud_mdb"`
	GapMDB              int64  `json:"gap_mdb"`
	RequiredAdjustBP    int64  `json:"required_adjust_bp"`
	MaxAllowedAdjustBP  int64  `json:"max_allowed_adjust_bp"`
}

// LoudnessErrorDetails is carried by E_LOUD.
type LoudnessErrorDetails struct {
	Gaps          []LoudnessGap `json:"gaps"`
	TotalBudgetBP int64         `json:"total_budget_bp"`
}

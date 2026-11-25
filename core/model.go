package dialoguecal

import "time"

const CanonicalSchema = "dialogue.canonical.v1"

type PhonemeClass string

const (
	ClassVowel     PhonemeClass = "vowel"
	ClassConsonant PhonemeClass = "consonant"
	ClassSilence   PhonemeClass = "silence"
)

type CalibrationState string

const (
	StateUncalibrated CalibrationState = "UNCALIBRATED"
	StateCalibrated   CalibrationState = "CALIBRATED"
	StateRecalibrate  CalibrationState = "RECALIBRATE"
)

type Segment struct {
	ID                  string `json:"id"`
	Index               int    `json:"index"`
	StartSample         int    `json:"start_sample"`
	EndSample           int    `json:"end_sample"`
	GainPermille        int    `json:"gain_permille"`
	GainQ15             Q15    `json:"gain_q15"`
	TargetLevelPermille int    `json:"target_level_permille"`
	TargetEnergyQ15     Q15    `json:"target_energy_q15"`
	TargetDBQ8          Q8     `json:"target_db_q8"`
	WatermarkDBX100     int    `json:"watermark_db_x100"`
}

type Phoneme struct {
	ID             string       `json:"id"`
	SegmentID      string       `json:"segment_id,omitempty"`
	SyllableID     string       `json:"syllable_id"`
	StartSample    int          `json:"start_sample"`
	EndSample      int          `json:"end_sample"`
	Class          PhonemeClass `json:"class"`
	F0StartHzQ8    Q8           `json:"f0_start_hz_q8"`
	F0EndHzQ8      Q8           `json:"f0_end_hz_q8"`
	WeightPermille int          `json:"weight_permille"`
	WeightQ15      Q15          `json:"weight_q15"`
}

type Project struct {
	SchemaVersion        string    `json:"schema_version"`
	ProjectID            string    `json:"project_id"`
	Seed                 int64     `json:"seed"`
	SampleRateHz         int       `json:"sample_rate_hz"`
	PCM                  []int16   `json:"-"`
	FramesPerSecond      int       `json:"frames_per_second"`
	FrameShiftSamples    int       `json:"frame_shift_samples"`
	FrameWindowSamples   int       `json:"frame_window_samples"`
	EnergyCapPermille    int       `json:"energy_cap_permille"`
	EnergyCapQ15         Q15       `json:"energy_cap_q15"`
	CalibrationTolDBQ8   Q8        `json:"calibration_tol_db_q8"`
	LevelJumpThresholdQ8 Q8        `json:"level_jump_threshold_q8"`
	SettleFrames         int       `json:"settle_frames"`
	Segments             []Segment `json:"segments"`
	Phonemes             []Phoneme `json:"phonemes"`
}

type LoadResult struct {
	Project      *Project `json:"project"`
	SourceSchema string   `json:"source_schema"`
	Resolutions  []string `json:"resolutions,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

type Frame struct {
	GlobalIndex    int      `json:"global_index"`
	SegmentID      string   `json:"segment_id"`
	IndexInSegment int      `json:"index_in_segment"`
	StartSample    int      `json:"start_sample"`
	EndSample      int      `json:"end_sample"`
	PhonemeIDs     []string `json:"phoneme_ids"`
}

type F0Candidate struct {
	HzQ8          Q8  `json:"hz_q8"`
	LagSamples    int `json:"lag_samples"`
	ConfidenceQ15 Q15 `json:"confidence_q15"`
}

type Features struct {
	RMSQ15       Q15           `json:"rms_q15"`
	ZCRCount     int           `json:"zcr_count"`
	ZCRQ15       Q15           `json:"zcr_q15"`
	Voiced       bool          `json:"voiced"`
	F0Candidates []F0Candidate `json:"f0_candidates"`
}

type Calibration struct {
	GainedEnergyQ15       Q15              `json:"gained_energy_q15"`
	EnergyQ15             Q15              `json:"energy_q15"`
	Limited               bool             `json:"limited"`
	LimitSource           string           `json:"limit_source,omitempty"`
	LoudnessDBQ8          Q8               `json:"loudness_db_q8"`
	EffectiveTargetDBQ8   Q8               `json:"effective_target_db_q8"`
	DesiredCorrectionDBQ8 Q8               `json:"desired_correction_db_q8"`
	CorrectionDBQ8        Q8               `json:"correction_db_q8"`
	CorrectionGainQ15     Q15              `json:"correction_gain_q15"`
	CalibratedDBQ8        Q8               `json:"calibrated_db_q8"`
	ResidualDBQ8          Q8               `json:"residual_db_q8"`
	State                 CalibrationState `json:"state"`
	StateReason           string           `json:"state_reason,omitempty"`
	StableFrames          int              `json:"stable_frames"`
	Saturations           []string         `json:"saturations,omitempty"`
}

type Provenance struct {
	Revision   int64    `json:"revision"`
	CacheState string   `json:"cache_state"` // computed, reused, restored
	ClosureIDs []string `json:"closure_ids,omitempty"`
	Sources    []string `json:"sources,omitempty"`
	CommitSeq  int64    `json:"commit_seq,omitempty"`
}

type FrameResult struct {
	Frame       Frame       `json:"frame"`
	Features    Features    `json:"features"`
	Calibration Calibration `json:"calibration"`
	Provenance  Provenance  `json:"provenance"`
}

type FrameRange struct {
	SegmentID string `json:"segment_id"`
	Start     int    `json:"start"`
	End       int    `json:"end"` // exclusive, frame indices in segment
}

type InvalidationClosure struct {
	ID            string       `json:"id"`
	Source        string       `json:"source"`
	Code          Code         `json:"code,omitempty"`
	DirectRanges  []FrameRange `json:"direct_ranges"`
	ClosureRanges []FrameRange `json:"closure_ranges"`
	Reason        string       `json:"reason"`
}

type RecomputeReport struct {
	Revision       int64                    `json:"revision"`
	Results        []FrameResult            `json:"results"`
	Closures       []InvalidationClosure    `json:"closures,omitempty"`
	RecomputedSeqs []int                    `json:"recomputed_seqs"`
	ReusedSeqs     []int                    `json:"reused_seqs"`
	SegmentTimings map[string]time.Duration `json:"segment_timings,omitempty"`
	Backpressure   []BackpressureEvent      `json:"backpressure,omitempty"`
}

type BackpressureEvent struct {
	Code      Code   `json:"code"`
	SegmentID string `json:"segment_id,omitempty"`
	FrameSeq  int    `json:"frame_seq,omitempty"`
	Capacity  int    `json:"capacity"`
	InFlight  int    `json:"in_flight"`
}

type Budget struct {
	Workers         int `json:"workers"`
	FramesPerWorker int `json:"frames_per_worker"`
	MaxWorkers      int `json:"max_workers"`
}

func DefaultBudget() Budget {
	return Budget{Workers: 2, FramesPerWorker: 16, MaxWorkers: 8}
}

func (b Budget) Validate() error {
	if b.Workers <= 0 || (b.MaxWorkers > 0 && b.Workers > b.MaxWorkers) {
		return newError(CodeBudgetWorkers, "budget.workers", "workers=%d must be in [1,%d]", b.Workers, b.MaxWorkers)
	}
	if b.FramesPerWorker <= 0 {
		return newError(CodeBudgetBuffer, "budget.frames_per_worker", "frames per worker must be positive")
	}
	return nil
}

func (b Budget) Capacity() int {
	return b.Workers * b.FramesPerWorker
}

// Tolerances are declared by the prototype and used by truth comparisons.
type Tolerances struct {
	EnergyQ15      Q15  `json:"energy_q15"`
	ZCRQ15         Q15  `json:"zcr_q15"`
	F0HzQ8         Q8   `json:"f0_hz_q8"`
	LoudnessDBQ8   Q8   `json:"loudness_db_q8"`
	CorrectionDBQ8 Q8   `json:"correction_db_q8"`
	StateMustMatch bool `json:"state_must_match"`
}

func DefaultTolerances() Tolerances {
	return Tolerances{EnergyQ15: 6, ZCRQ15: 2, F0HzQ8: 5 * 256, LoudnessDBQ8: 12, CorrectionDBQ8: 12, StateMustMatch: true}
}

type FrameComparison struct {
	GlobalIndex         int    `json:"global_index"`
	EnergyDeltaQ15      int64  `json:"energy_delta_q15"`
	EnergyWithin        bool   `json:"energy_within"`
	ZCRDeltaQ15         int64  `json:"zcr_delta_q15"`
	ZCRWithin           bool   `json:"zcr_within"`
	F0DeltaHzQ8         int64  `json:"f0_delta_hz_q8"`
	F0Within            bool   `json:"f0_within"`
	LoudnessDeltaDBQ8   int64  `json:"loudness_delta_db_q8"`
	LoudnessWithin      bool   `json:"loudness_within"`
	CorrectionDeltaDBQ8 int64  `json:"correction_delta_db_q8"`
	CorrectionWithin    bool   `json:"correction_within"`
	StateMatch          bool   `json:"state_match"`
	AllWithin           bool   `json:"all_within"`
	Note                string `json:"note,omitempty"`
}

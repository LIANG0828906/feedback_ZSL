package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	CodeSchema   = "E_SCHEMA"
	CodePara     = "E_PARA"
	CodeDecode   = "E_DECODE"
	CodeStage    = "E_STAGE"
	CodeBand     = "E_BAND"
	CodeState    = "E_STATE"
	CodeBound    = "E_BOUND"
	CodeResource = "E_RESOURCE"
)

const (
	ParamSpecVersion            = "eval-params-v1"
	AlgorithmVersion            = "metric-core-v1.0.0"
	CheckpointSchemaVersion     = "checkpoint-v1.0.0"
	GeneratorVersion            = "generator-v1.0.0"
	MetricScale                 = int64(1_000_000)
	CoefficientFracBits         = 24
	BoundarySSIMToleranceFixed  = int64(1_000)
	BoundaryDBToleranceFixed    = int64(1_000)
	BoundaryNoiseToleranceFixed = int64(1_000)
)

type BlockCoord struct {
	Index              int `json:"index"`
	FirstFrame         int `json:"first_frame"`
	LastFrame          int `json:"last_frame"`
	CoreStartSample    int `json:"core_start_sample"`
	CoreEndSample      int `json:"core_end_sample"`
	BufferOriginSample int `json:"buffer_origin_sample"`
	BufferEndSample    int `json:"buffer_end_sample"`
	LeftRequired       int `json:"left_required_samples"`
	RightRequired      int `json:"right_required_samples"`
	MinBandSamples     int `json:"min_band_samples"`
	BandSamples        int `json:"band_samples,omitempty"`
	BufferSamples      int `json:"buffer_samples,omitempty"`
}

type Error struct {
	Code     string      `json:"code"`
	Field    string      `json:"field,omitempty"`
	Message  string      `json:"message"`
	Frame    *int        `json:"frame,omitempty"`
	Block    *BlockCoord `json:"block,omitempty"`
	Expected any         `json:"expected,omitempty"`
	Actual   any         `json:"actual,omitempty"`
	MinBand  int         `json:"min_band_samples,omitempty"`
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s field=%s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func WithFrame(err *Error, frame int) *Error {
	err.Frame = &frame
	return err
}

func WithBlock(err *Error, block BlockCoord) *Error {
	b := block
	err.Block = &b
	if block.MinBandSamples > 0 {
		err.MinBand = block.MinBandSamples
	}
	return err
}

type Manifest struct {
	SchemaVersion string `json:"schema_version"`
	SampleRate    int    `json:"sample_rate"`
	FrameSize     int    `json:"frame_size"`
	HopSize       int    `json:"hop_size"`
	Channels      int    `json:"channels"`
	FrameCount    int    `json:"frame_count"`
	QualityGear   int    `json:"quality_gear"`
	Seed          int64  `json:"seed"`
	SampleBits    int    `json:"sample_bits"`
}

func (m Manifest) SamplesPerChannel() int {
	if m.FrameCount <= 0 || m.FrameSize <= 0 {
		return 0
	}
	return (m.FrameCount-1)*m.HopSize + m.FrameSize
}

func (m Manifest) TotalInterleavedSamples() int {
	return m.SamplesPerChannel() * m.Channels
}

func (m Manifest) PeakAmplitude() int64 {
	bits := m.SampleBits
	if bits == 0 {
		bits = 16
	}
	return int64(1)<<(bits-1) - 1
}

type EvalParams struct {
	WindowSizeSamples int `json:"window_size_samples"`
	StepSamples       int `json:"step_samples"`
	SSIMWindowSamples int `json:"ssim_window_samples,omitempty"`
	SSIMStepSamples   int `json:"ssim_step_samples,omitempty"`
	SideBandSamples   int `json:"side_band_samples,omitempty"`
}

func (p EvalParams) WithDefaults(m Manifest) EvalParams {
	out := p
	if out.WindowSizeSamples <= 0 {
		out.WindowSizeSamples = m.FrameSize
	}
	if out.StepSamples <= 0 {
		out.StepSamples = m.HopSize
	}
	if out.SSIMWindowSamples <= 0 {
		out.SSIMWindowSamples = 32
		if m.FrameSize < out.SSIMWindowSamples {
			out.SSIMWindowSamples = m.FrameSize
		}
	}
	if out.SSIMStepSamples <= 0 {
		out.SSIMStepSamples = 8
	}
	return out
}

type TruncationRecord struct {
	Field        string  `json:"field"`
	Stage        string  `json:"stage"`
	FractionBits int     `json:"fraction_bits,omitempty"`
	Scale        int64   `json:"scale,omitempty"`
	MaxAbsError  float64 `json:"max_abs_error"`
	IntegerAccum bool    `json:"integer_accum"`
	Description  string  `json:"description"`
}

type Identity struct {
	ContentHash        string `json:"content_hash"`
	OriginalHash       string `json:"original_hash"`
	ReconstructionHash string `json:"reconstruction_hash"`
	TruthFingerprint   string `json:"truth_fingerprint"`
	ParamVersion       string `json:"param_version"`
	ParamHash          string `json:"param_hash"`
	AlgorithmVersion   string `json:"algorithm_version"`
	SchemaVersion      string `json:"schema_version"`
}

func HashCanonical(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

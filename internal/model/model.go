package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	SchemaVersion    = 1
	AlgorithmVersion = "audioeval-alg-v1"

	CodePara   = "E_PARA"
	CodeDecode = "E_DECODE"
	CodeState  = "E_STATE"
	CodeBand   = "E_BAND"
)

type QualityLevel string

const (
	QualityQ1 QualityLevel = "Q1"
	QualityQ2 QualityLevel = "Q2"
	QualityQ3 QualityLevel = "Q3"
	QualityQ4 QualityLevel = "Q4"
	QualityQ5 QualityLevel = "Q5"
)

type QualitySpec struct {
	Level QualityLevel `json:"level"`
	Step  int64        `json:"step"`
}

var qualitySpecs = map[QualityLevel]QualitySpec{
	QualityQ1: {Level: QualityQ1, Step: 128},
	QualityQ2: {Level: QualityQ2, Step: 64},
	QualityQ3: {Level: QualityQ3, Step: 32},
	QualityQ4: {Level: QualityQ4, Step: 16},
	QualityQ5: {Level: QualityQ5, Step: 8},
}

func LookupQuality(level QualityLevel) (QualitySpec, bool) {
	spec, ok := qualitySpecs[level]
	return spec, ok
}

func SupportedQualities() []QualitySpec {
	return []QualitySpec{
		qualitySpecs[QualityQ1],
		qualitySpecs[QualityQ2],
		qualitySpecs[QualityQ3],
		qualitySpecs[QualityQ4],
		qualitySpecs[QualityQ5],
	}
}

type EvalError struct {
	Code         string     `json:"code"`
	Field        string     `json:"field,omitempty"`
	Frame        int        `json:"frame"`
	Block        BlockCoord `json:"block"`
	HasBlock     bool       `json:"has_block"`
	RequiredBand int        `json:"required_band,omitempty"`
	Message      string     `json:"message"`
}

func (e *EvalError) Error() string {
	if e == nil {
		return ""
	}
	location := ""
	if e.Frame >= 0 {
		location = fmt.Sprintf(" frame=%d", e.Frame)
	}
	if e.HasBlock {
		location += fmt.Sprintf(" block=%d", e.Block.Block)
	}
	if e.Field != "" {
		location += fmt.Sprintf(" field=%s", e.Field)
	}
	return fmt.Sprintf("%s%s: %s", e.Code, location, e.Message)
}

func NewError(code, field string, frame int, message string) *EvalError {
	return &EvalError{Code: code, Field: field, Frame: frame, Message: message}
}

func (e *EvalError) WithFrame(frame int) *EvalError {
	e.Frame = frame
	return e
}

func (e *EvalError) WithBlock(coord BlockCoord) *EvalError {
	e.Block = coord
	e.HasBlock = true
	return e
}

func (e *EvalError) WithRequiredBand(width int) *EvalError {
	e.RequiredBand = width
	return e
}

func CodeOf(err error) string {
	if e, ok := err.(*EvalError); ok {
		return e.Code
	}
	return ""
}

type BlockCoord struct {
	Frame int `json:"frame"`
	Block int `json:"block"`
}

func (b BlockCoord) String() string {
	return fmt.Sprintf("frame=%d/block=%d", b.Frame, b.Block)
}

type BlockStage string

const (
	StageAlign       BlockStage = "align"
	StageMetrics     BlockStage = "metrics"
	StagePropagation BlockStage = "propagation"
)

func AllStages() []BlockStage {
	return []BlockStage{StageAlign, StageMetrics, StagePropagation}
}

type BlockStatus string

const (
	StatusPending    BlockStatus = "pending"
	StatusProcessing BlockStatus = "processing"
	StatusCommitted  BlockStatus = "committed"
	StatusFailed     BlockStatus = "failed"
)

type Manifest struct {
	SchemaVersion             int          `json:"schema_version"`
	SampleID                  string       `json:"sample_id"`
	Seed                      int64        `json:"seed"`
	SampleRate                int          `json:"sample_rate"`
	Channels                  int          `json:"channels"`
	FrameLength               int          `json:"frame_length"`
	FrameHop                  int          `json:"frame_hop"`
	FrameCount                int          `json:"frame_count"`
	TotalSamplesPerChannel    int          `json:"total_samples_per_channel"`
	Quality                   QualityLevel `json:"quality"`
	QuantStep                 int64        `json:"quant_step"`
	OriginalFile              string       `json:"original_file"`
	ReconstructionFile        string       `json:"reconstruction_file"`
	TruthFile                 string       `json:"truth_file"`
	OriginalHash              string       `json:"original_hash"`
	ReconstructionHash        string       `json:"reconstruction_hash"`
	TruthFingerprint          string       `json:"truth_fingerprint"`
	OriginalFrameHashes       []string     `json:"original_frame_hashes"`
	ReconstructionFrameHashes []string     `json:"reconstruction_frame_hashes"`
	InputHash                 string       `json:"input_hash"`
}

type Truth struct {
	SchemaVersion    int          `json:"schema_version"`
	SampleID         string       `json:"sample_id"`
	TruthFingerprint string       `json:"truth_fingerprint"`
	Frames           []FrameTruth `json:"frames"`
}

type FrameTruth struct {
	Frame            int   `json:"frame"`
	PulsePositions   []int `json:"pulse_positions"`
	ExpectedSNRUDb   int64 `json:"expected_snr_udb"`
	NoiseFloorStdQ16 int64 `json:"noise_floor_std_q16"`
}

type EvalParams struct {
	Window                 int `json:"window"`
	WindowHop              int `json:"window_hop"`
	BlocksPerFrame         int `json:"blocks_per_frame"`
	BandWidth              int `json:"band_width"`
	MaxWorkers             int `json:"max_workers"`
	PerThreadBufferSamples int `json:"per_thread_buffer_samples"`
	QueueBudget            int `json:"queue_budget"`
	CancelAfterCommits     int `json:"-"`
}

type ParamVersionFields struct {
	Window         int `json:"window"`
	WindowHop      int `json:"window_hop"`
	BlocksPerFrame int `json:"blocks_per_frame"`
	BandWidth      int `json:"band_width"`
}

func (p EvalParams) ParamVersion() string {
	fields := ParamVersionFields{
		Window:         p.Window,
		WindowHop:      p.WindowHop,
		BlocksPerFrame: p.BlocksPerFrame,
		BandWidth:      p.BandWidth,
	}
	data, _ := json.Marshal(fields)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type WindowStat struct {
	Channel int   `json:"channel"`
	Start   int   `json:"start"`
	Length  int   `json:"length"`
	SumX    int64 `json:"sum_x"`
	SumY    int64 `json:"sum_y"`
	SumXX   int64 `json:"sum_xx"`
	SumYY   int64 `json:"sum_yy"`
	SumXY   int64 `json:"sum_xy"`
	SSE     int64 `json:"sse"`
}

type ChannelCoreStat struct {
	Channel int   `json:"channel"`
	Count   int64 `json:"count"`
	SumX    int64 `json:"sum_x"`
	SumY    int64 `json:"sum_y"`
	SumXX   int64 `json:"sum_xx"`
	SumYY   int64 `json:"sum_yy"`
	SumXY   int64 `json:"sum_xy"`
	SSE     int64 `json:"sse"`
}

type ChannelResidual struct {
	Channel           int   `json:"channel"`
	Count             int64 `json:"count"`
	OriginalSSE       int64 `json:"original_sse"`
	ReconstructionSSE int64 `json:"reconstruction_sse"`
}

type BlockQuality struct {
	PSNRUDb int64 `json:"psnr_udb"`
	SSIMQ16 int64 `json:"ssim_q16"`
}

type BlockResult struct {
	SchemaVersion         int               `json:"schema_version"`
	Sequence              int               `json:"sequence"`
	Frame                 int               `json:"frame"`
	Block                 int               `json:"block"`
	CoreStart             int               `json:"core_start"`
	CoreEnd               int               `json:"core_end"`
	BandLeft              int               `json:"band_left"`
	BandRight             int               `json:"band_right"`
	RequiredBandRight     int               `json:"required_band_right"`
	Windows               []WindowStat      `json:"windows"`
	CoreStats             []ChannelCoreStat `json:"core_stats"`
	Residuals             []ChannelResidual `json:"residuals"`
	Quality               BlockQuality      `json:"quality"`
	QuantCoeffAbsQ14      int64             `json:"quant_coeff_abs_q14"`
	QuantCoeffSqQ28       int64             `json:"quant_coeff_sq_q28"`
	SampleErrorSSE        int64             `json:"sample_error_sse"`
	ReconstructionHash    string            `json:"reconstruction_hash"`
	OriginalHash          string            `json:"original_hash"`
	QuantStep             int64             `json:"quant_step"`
	TruthExpectedSNRUDb   int64             `json:"truth_expected_snr_udb"`
	TruthNoiseFloorStdQ16 int64             `json:"truth_noise_floor_std_q16"`
	ErrorUpperBound       int64             `json:"error_upper_bound"`
	BoundaryJump          int64             `json:"boundary_jump"`
	InputHash             string            `json:"input_hash"`
	ParamVersion          string            `json:"param_version"`
	AlgorithmVersion      string            `json:"algorithm_version"`
}

type BlockSummary struct {
	Frame              int          `json:"frame"`
	Block              int          `json:"block"`
	CoreStart          int          `json:"core_start"`
	CoreEnd            int          `json:"core_end"`
	BandLeft           int          `json:"band_left"`
	BandRight          int          `json:"band_right"`
	WindowCount        int          `json:"window_count"`
	Quality            BlockQuality `json:"quality"`
	SampleErrorSSE     int64        `json:"sample_error_sse"`
	ErrorUpperBound    int64        `json:"error_upper_bound"`
	BoundaryJump       int64        `json:"boundary_jump"`
	ReconstructionHash string       `json:"reconstruction_hash"`
	OriginalHash       string       `json:"original_hash"`
}

type ChannelFrameReport struct {
	Channel                   int     `json:"channel"`
	PSNRUDb                   int64   `json:"psnr_udb"`
	SSIMQ16                   int64   `json:"ssim_q16"`
	OriginalNoiseStdQ16       int64   `json:"original_noise_std_q16"`
	ReconstructionNoiseStdQ16 int64   `json:"reconstruction_noise_std_q16"`
	WindowCount               int     `json:"window_count"`
	QuantCoeffErrorsQ14       []int64 `json:"quant_coeff_errors_q14"`
	InverseErrorSSE           int64   `json:"inverse_error_sse"`
}

type FrameReport struct {
	Frame                      int                  `json:"frame"`
	PSNRUDb                    int64                `json:"psnr_udb"`
	SSIMQ16                    int64                `json:"ssim_q16"`
	OriginalNoiseStdQ16        int64                `json:"original_noise_std_q16"`
	ReconstructionNoiseStdQ16  int64                `json:"reconstruction_noise_std_q16"`
	QuantizationResidualStdQ16 int64                `json:"quantization_residual_std_q16"`
	TruthExpectedSNRUDb        int64                `json:"truth_expected_snr_udb"`
	TruthNoiseFloorStdQ16      int64                `json:"truth_noise_floor_std_q16"`
	NoiseFloorDeltaQ16         int64                `json:"noise_floor_delta_q16"`
	ErrorUpperBound            int64                `json:"error_upper_bound"`
	InverseErrorSSE            int64                `json:"inverse_error_sse"`
	BoundaryErrorJump          int64                `json:"boundary_error_jump"`
	Channels                   []ChannelFrameReport `json:"channels"`
	Blocks                     []BlockSummary       `json:"blocks"`
}

type ErrorPathEdge struct {
	FromFrame     int   `json:"from_frame"`
	FromBlock     int   `json:"from_block"`
	ToFrame       int   `json:"to_frame"`
	ToBlock       int   `json:"to_block"`
	FrameBoundary bool  `json:"frame_boundary"`
	ErrorJump     int64 `json:"error_jump"`
}

type QueueEvent struct {
	Sequence   int        `json:"sequence"`
	Coord      BlockCoord `json:"coord"`
	Reason     string     `json:"reason"`
	WaitMillis int64      `json:"wait_millis"`
}

type PrecisionPolicy struct {
	SampleAccumulator string   `json:"sample_accumulator"`
	PSNR              string   `json:"psnr"`
	SSIM              string   `json:"ssim"`
	NoiseStd          string   `json:"noise_std"`
	ErrorBound        string   `json:"error_bound"`
	Truncation        []string `json:"truncation"`
}

func DefaultPrecisionPolicy() PrecisionPolicy {
	return PrecisionPolicy{
		SampleAccumulator: "int64 integer sample accumulators; samples are int16 points",
		PSNR:              "10*log10(32767^2*window/sse), floored to integer micro-dB",
		SSIM:              "integer int64 moments, final ratios converted from declared float64 to floor Q16",
		NoiseStd:          "integer first-difference SSE plus sample-domain quantization residual SSE, final sqrt rounded to Q16",
		ErrorBound:        "declared float64 sqrt and ceil; uses quant step, frame length, frame hop, window and truth noise-floor std only",
		Truncation: []string{
			"PSNR: floor at micro-decibel after float64 log10",
			"SSIM: floor at Q16 after float64 ratio conversion",
			"noise std: round to Q16 at final sqrt only",
			"error bound: ceil at final sample units",
		},
	}
}

type AlignmentReport struct {
	Offset                int  `json:"offset"`
	OriginalSamples       int  `json:"original_samples"`
	ReconstructionSamples int  `json:"reconstruction_samples"`
	InterleavedSamples    int  `json:"interleaved_samples"`
	Aligned               bool `json:"aligned"`
}

type ResumeInfo struct {
	RequestedMode   string `json:"requested_mode"`
	Resumed         bool   `json:"resumed"`
	Invalidated     bool   `json:"invalidated"`
	InvalidReason   string `json:"invalid_reason,omitempty"`
	LoadedBlocks    int    `json:"loaded_blocks"`
	CommittedBlocks int    `json:"committed_blocks"`
	TotalBlocks     int    `json:"total_blocks"`
}

type SnapshotMeta struct {
	SchemaVersion    int          `json:"schema_version"`
	SampleID         string       `json:"sample_id"`
	Quality          QualityLevel `json:"quality"`
	InputHash        string       `json:"input_hash"`
	ParamVersion     string       `json:"param_version"`
	AlgorithmVersion string       `json:"algorithm_version"`
	TruthFingerprint string       `json:"truth_fingerprint"`
	FrameCount       int          `json:"frame_count"`
	FrameLength      int          `json:"frame_length"`
	FrameHop         int          `json:"frame_hop"`
	Channels         int          `json:"channels"`
	BlocksPerFrame   int          `json:"blocks_per_frame"`
	Window           int          `json:"window"`
	WindowHop        int          `json:"window_hop"`
}

type QualityReport struct {
	SchemaVersion    int             `json:"schema_version"`
	SampleID         string          `json:"sample_id"`
	Quality          QualityLevel    `json:"quality"`
	Mode             string          `json:"mode"`
	Complete         bool            `json:"complete"`
	InputHash        string          `json:"input_hash"`
	ParamVersion     string          `json:"param_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
	TruthFingerprint string          `json:"truth_fingerprint"`
	Params           EvalParams      `json:"params"`
	Alignment        AlignmentReport `json:"alignment"`
	Precision        PrecisionPolicy `json:"precision"`
	Resume           ResumeInfo      `json:"resume"`
	Frames           []FrameReport   `json:"frames"`
	ErrorPath        []ErrorPathEdge `json:"error_path"`
	CommitSequence   []int           `json:"commit_sequence"`
	QueueEvents      []QueueEvent    `json:"queue_events"`
}

func CanonicalHash(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func TruthFingerprint(t *Truth) (string, error) {
	copyTruth := *t
	copyTruth.TruthFingerprint = ""
	return CanonicalHash(&copyTruth)
}

func ComputeInputHash(m *Manifest, truthFingerprint string) (string, error) {
	fields := struct {
		SampleID               string       `json:"sample_id"`
		Seed                   int64        `json:"seed"`
		SampleRate             int          `json:"sample_rate"`
		Channels               int          `json:"channels"`
		FrameLength            int          `json:"frame_length"`
		FrameHop               int          `json:"frame_hop"`
		FrameCount             int          `json:"frame_count"`
		TotalSamplesPerChannel int          `json:"total_samples_per_channel"`
		Quality                QualityLevel `json:"quality"`
		QuantStep              int64        `json:"quant_step"`
		OriginalHash           string       `json:"original_hash"`
		ReconstructionHash     string       `json:"reconstruction_hash"`
		TruthFingerprint       string       `json:"truth_fingerprint"`
	}{
		SampleID:               m.SampleID,
		Seed:                   m.Seed,
		SampleRate:             m.SampleRate,
		Channels:               m.Channels,
		FrameLength:            m.FrameLength,
		FrameHop:               m.FrameHop,
		FrameCount:             m.FrameCount,
		TotalSamplesPerChannel: m.TotalSamplesPerChannel,
		Quality:                m.Quality,
		QuantStep:              m.QuantStep,
		OriginalHash:           m.OriginalHash,
		ReconstructionHash:     m.ReconstructionHash,
		TruthFingerprint:       truthFingerprint,
	}
	return CanonicalHash(fields)
}

func FrameStrideBytes(channels, frameLength int) int {
	return channels * frameLength * 2
}

func BlockFileName(sequence int) string {
	return fmt.Sprintf("block-%09d.json", sequence)
}

func ParseBlockFileName(name string) (int, bool) {
	if !strings.HasPrefix(name, "block-") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	number := strings.TrimSuffix(strings.TrimPrefix(name, "block-"), ".json")
	if number == "" {
		return 0, false
	}
	value, err := strconv.Atoi(number)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

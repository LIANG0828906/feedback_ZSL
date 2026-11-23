// Package engine orchestrates measurement, dynamics, global normalization,
// bounded parallel scheduling, state transitions and checkpoint recovery.
package engine

import (
	"fmt"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
	"loudnessworkbench/dsp"
)

type Mode string

const (
	ModeFull     Mode = "full"
	ModeRealtime Mode = "realtime"
	ModeResume   Mode = "resume"
)

type RunOptions struct {
	Mode                 Mode  `json:"mode"`
	MaxParallel          int   `json:"max_parallel"`
	PerWorkerBufferBytes int64 `json:"per_worker_buffer_bytes"`
}

func DefaultRunOptions() RunOptions {
	return RunOptions{
		Mode:                 ModeFull,
		MaxParallel:          4,
		PerWorkerBufferBytes: 16 << 20,
	}
}

func (o *RunOptions) Normalize() error {
	if o.Mode == "" {
		o.Mode = ModeFull
	}
	switch o.Mode {
	case ModeFull, ModeRealtime, ModeResume:
	default:
		return apperr.New(apperr.CodeParameter, "unknown processing mode").
			WithField("mode").WithActual(o.Mode)
	}
	if o.Mode == ModeRealtime {
		o.MaxParallel = 1
	}
	if o.MaxParallel == 0 {
		o.MaxParallel = 4
	}
	if o.MaxParallel < 1 || o.MaxParallel > 16 {
		return apperr.New(apperr.CodeParameter, "max parallel must be in [1, 16]").
			WithField("max_parallel").WithActual(o.MaxParallel)
	}
	if o.PerWorkerBufferBytes == 0 {
		o.PerWorkerBufferBytes = 16 << 20
	}
	if o.PerWorkerBufferBytes < 256<<10 || o.PerWorkerBufferBytes > 1<<30 {
		return apperr.New(apperr.CodeParameter, "per-worker buffer must be in [256 KiB, 1 GiB]").
			WithField("per_worker_buffer_bytes").WithActual(o.PerWorkerBufferBytes)
	}
	return nil
}

type RecoveryInfo struct {
	Requested       bool   `json:"requested"`
	CheckpointFound bool   `json:"checkpoint_found"`
	CheckpointUsed  bool   `json:"checkpoint_used"`
	Invalidated     bool   `json:"invalidated"`
	ReusedSegments  int    `json:"reused_segments"`
	Reason          string `json:"reason,omitempty"`
}

type SegmentResult struct {
	Index                     int             `json:"index"`
	ID                        string          `json:"id"`
	State                     SegmentState    `json:"state"`
	Channels                  int             `json:"channels"`
	Frames                    int             `json:"frames"`
	OutputFrames              int             `json:"output_frames"`
	OutputChannels            int             `json:"output_channels"`
	SourceMeasurement         dsp.Measurement `json:"source_measurement"`
	CompressedMeasurement     dsp.Measurement `json:"compressed_measurement"`
	OutputMeasurement         dsp.Measurement `json:"output_measurement"`
	SegmentIdealGainDB        float64         `json:"segment_ideal_gain_db"`
	NormalizationGainDB       float64         `json:"normalization_gain_db"`
	CompressionMaxReductionDB float64         `json:"compression_max_reduction_db"`
	LimiterSafetyTrimDB       float64         `json:"limiter_safety_trim_db"`
	SourceHash                string          `json:"source_hash"`
	OutputHash                string          `json:"output_hash"`
	CompressionGainCurveDB    []float64       `json:"compression_gain_curve_db,omitempty"`
	FromCheckpoint            bool            `json:"from_checkpoint"`
	Error                     *apperr.Error   `json:"error,omitempty"`
	Output                    audio.Signal    `json:"-"`
	Compressed                audio.Signal    `json:"-"`
	compressorGainDB          []float64
}

type MixSpan struct {
	Index      int    `json:"index"`
	ID         string `json:"id"`
	StartFrame int64  `json:"start_frame"`
	FrameCount int64  `json:"frame_count"`
	Channels   int    `json:"channels"`
}

type Result struct {
	ProjectID                string           `json:"project_id"`
	SourceVersion            string           `json:"source_version"`
	ParametersHash           string           `json:"parameters_hash"`
	AlgorithmVersion         string           `json:"algorithm_version"`
	Mode                     Mode             `json:"mode"`
	MaxParallel              int              `json:"max_parallel"`
	PerWorkerBufferBytes     int64            `json:"per_worker_buffer_bytes"`
	BufferPeakBytes          int64            `json:"buffer_peak_bytes"`
	TargetLUFS               float64          `json:"target_lufs"`
	SourceIntegratedLUFS     float64          `json:"source_integrated_lufs"`
	CompressedIntegratedLUFS float64          `json:"compressed_integrated_lufs"`
	ProgramTruePeakDBTP      float64          `json:"program_true_peak_dbtp"`
	GlobalGainDB             float64          `json:"global_gain_db"`
	FinalProgramLoudnessLUFS float64          `json:"final_program_loudness_lufs"`
	NormalizationIterations  int              `json:"normalization_iterations"`
	NormalizationConverged   bool             `json:"normalization_converged"`
	Segments                 []*SegmentResult `json:"segments"`
	Offsets                  []MixSpan        `json:"mix_offsets"`
	MixFrames                int              `json:"mix_frames"`
	MixChannels              int              `json:"mix_channels"`
	MixHash                  string           `json:"mix_hash"`
	Recovery                 RecoveryInfo     `json:"recovery"`
	Mix                      audio.Signal     `json:"-"`
	FirstError               *apperr.Error    `json:"first_error,omitempty"`
}

func (r *Result) AllCommitted() bool {
	if len(r.Segments) == 0 {
		return false
	}
	for _, segment := range r.Segments {
		if segment.State != StateCommitted {
			return false
		}
	}
	return true
}

func (r *Result) AllSucceeded() bool {
	if len(r.Segments) == 0 {
		return false
	}
	for _, segment := range r.Segments {
		if segment.Error != nil {
			return false
		}
	}
	return true
}

func (r *Result) FirstSegmentError() error {
	for _, segment := range r.Segments {
		if segment.Error != nil {
			return segment.Error
		}
	}
	return nil
}

func (r *Result) SegmentByID(id string) *SegmentResult {
	for _, segment := range r.Segments {
		if segment.ID == id {
			return segment
		}
	}
	return nil
}

func requiredBytes(frames, channels int) int64 {
	// source + compressed + normalized + output plus gain curves and bookkeeping.
	return int64(frames)*int64(channels)*8*4 + int64(frames)*24 + 4096
}

func segmentLabel(index int, id string) string {
	return fmt.Sprintf("%d:%s", index, id)
}

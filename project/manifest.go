package project

import (
	"encoding/json"
	"fmt"
	"math"
	"os"

	"loudnessworkbench/apperr"
	"loudnessworkbench/dsp"
)

type Tolerance struct {
	LoudnessLU float64 `json:"loudness_lu"`
	TruePeakDB float64 `json:"true_peak_db"`
	GainDB     float64 `json:"gain_db"`
}

type GateTruth struct {
	FrameCount            int     `json:"frame_count"`
	AboveAbsolute         int     `json:"frames_above_absolute"`
	AboveBothGates        int     `json:"frames_above_both_gates"`
	AbsoluteThresholdLUFS float64 `json:"absolute_threshold_lufs"`
	RelativeThresholdLUFS float64 `json:"relative_threshold_lufs"`
}

type SegmentTruth struct {
	IntegratedLUFS                float64   `json:"integrated_lufs"`
	TruePeakDBTP                  float64   `json:"true_peak_dbtp"`
	TargetGainDB                  float64   `json:"target_gain_db"`
	SegmentIdealGainDB            float64   `json:"segment_ideal_gain_db"`
	PostCompressionIntegratedLUFS float64   `json:"post_compression_integrated_lufs"`
	Gate                          GateTruth `json:"gate"`
}

type ManifestSegment struct {
	Index           int          `json:"index"`
	ID              string       `json:"id"`
	Channels        int          `json:"channels"`
	SampleRateHz    int          `json:"sample_rate_hz"`
	DurationSeconds float64      `json:"duration_seconds"`
	FrameCount      int          `json:"frame_count"`
	Audio           string       `json:"audio"`
	ContentSHA256   string       `json:"content_sha256"`
	Truth           SegmentTruth `json:"truth"`
}

type ProgramTruth struct {
	IntegratedLUFS                float64 `json:"integrated_lufs"`
	PostCompressionIntegratedLUFS float64 `json:"post_compression_integrated_lufs"`
	TruePeakDBTP                  float64 `json:"true_peak_dbtp"`
	TargetGainDB                  float64 `json:"target_gain_db"`
}

type Manifest struct {
	Schema       string               `json:"schema"`
	ProgramID    string               `json:"program_id"`
	Seed         int64                `json:"seed"`
	SampleRateHz int                  `json:"sample_rate_hz"`
	TargetLUFS   float64              `json:"target_lufs"`
	FrameMs      float64              `json:"frame_ms"`
	HopMs        float64              `json:"hop_ms"`
	Gate         dsp.GateConfig       `json:"gate"`
	Compressor   dsp.CompressorConfig `json:"compressor"`
	Limiter      dsp.LimiterConfig    `json:"limiter"`
	Tolerance    Tolerance            `json:"tolerance"`
	Segments     []ManifestSegment    `json:"segments"`
	ProgramTruth ProgramTruth         `json:"program_truth"`
}

func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeSchema, "cannot read manifest", err).WithPath(path)
	}
	if err := validateManifestShape(data); err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, apperr.Wrap(apperr.CodeSchema, "invalid manifest JSON", err).WithPath(path)
	}
	if manifest.Schema != "lw-manifest/1" {
		return nil, apperr.New(apperr.CodeSchema, "unknown manifest schema").
			WithField("schema").WithActual(manifest.Schema)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (m *Manifest) Validate() error {
	if m.ProgramID == "" {
		return apperr.New(apperr.CodeSchema, "manifest program id is required").WithField("program_id")
	}
	if !dsp.SupportedSampleRate(m.SampleRateHz) {
		return apperr.New(apperr.CodeSchema, "unsupported sample rate").WithField("sample_rate_hz").WithActual(m.SampleRateHz)
	}
	if m.FrameMs <= 0 || m.HopMs <= 0 || m.HopMs > m.FrameMs {
		return apperr.New(apperr.CodeSchema, "invalid manifest frame/hop").WithField("frame_ms")
	}
	if m.Tolerance.LoudnessLU <= 0 {
		return apperr.New(apperr.CodeSchema, "loudness tolerance must be positive").WithField("tolerance.loudness_lu")
	}
	if m.Tolerance.TruePeakDB <= 0 {
		return apperr.New(apperr.CodeSchema, "true-peak tolerance must be positive").WithField("tolerance.true_peak_db")
	}
	if m.Tolerance.GainDB <= 0 {
		return apperr.New(apperr.CodeSchema, "gain tolerance must be positive").WithField("tolerance.gain_db")
	}
	if len(m.Segments) == 0 {
		return apperr.New(apperr.CodeSchema, "manifest has no segments").WithField("segments")
	}
	seen := map[string]bool{}
	for i, segment := range m.Segments {
		path := fmt.Sprintf("segments[%d]", i)
		if segment.Index != i {
			return apperr.New(apperr.CodeSchema, "manifest segment index is not contiguous").
				WithField(path + ".index").WithExpected(i).WithActual(segment.Index)
		}
		if segment.ID == "" || seen[segment.ID] {
			return apperr.New(apperr.CodeSchema, "manifest segment id is missing or duplicated").
				WithField(path + ".id").WithActual(segment.ID)
		}
		seen[segment.ID] = true
		if segment.Channels != 1 && segment.Channels != 2 {
			return apperr.New(apperr.CodeSchema, "manifest segment channel count must be mono or stereo").
				WithField(path + ".channels").WithActual(segment.Channels)
		}
		if segment.SampleRateHz != m.SampleRateHz {
			return apperr.New(apperr.CodeSchema, "manifest segment sample rate differs from program").
				WithField(path + ".sample_rate_hz").WithExpected(m.SampleRateHz).WithActual(segment.SampleRateHz)
		}
		if segment.FrameCount <= 0 || segment.DurationSeconds <= 0 {
			return apperr.New(apperr.CodeSchema, "manifest segment duration is invalid").
				WithField(path + ".duration_seconds")
		}
		if math.Abs(segment.DurationSeconds-float64(segment.FrameCount)/float64(m.SampleRateHz)) > 0.51/float64(m.SampleRateHz) {
			return apperr.New(apperr.CodeSchema, "manifest duration and frame count disagree").
				WithField(path + ".duration_seconds").
				WithExpected(float64(segment.FrameCount) / float64(m.SampleRateHz)).
				WithActual(segment.DurationSeconds)
		}
		if segment.ContentSHA256 == "" {
			return apperr.New(apperr.CodeSchema, "manifest content hash is required").
				WithField(path + ".content_sha256")
		}
		if segment.Truth.Gate.FrameCount <= 0 {
			return apperr.New(apperr.CodeSchema, "manifest gate truth is required").
				WithField(path + ".truth.gate.frame_count")
		}
		if segment.Truth.IntegratedLUFS < -200 || segment.Truth.IntegratedLUFS > 40 ||
			segment.Truth.TruePeakDBTP < -200 || segment.Truth.TruePeakDBTP > 40 {
			return apperr.New(apperr.CodeSchema, "manifest truth level is out of range").
				WithField(path + ".truth")
		}
	}
	return nil
}

func validateManifestShape(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return apperr.Wrap(apperr.CodeSchema, "manifest is not a JSON object", err)
	}
	required := []string{
		"schema", "program_id", "seed", "sample_rate_hz", "target_lufs", "frame_ms", "hop_ms",
		"gate", "compressor", "limiter", "tolerance", "segments", "program_truth",
	}
	for _, key := range required {
		if _, ok := root[key]; !ok {
			return apperr.New(apperr.CodeSchema, "required manifest field is missing").
				WithField(key).WithPath(key)
		}
	}
	for key, fields := range map[string][]string{
		"gate":          {"absolute_lufs", "relative_lu"},
		"compressor":    {"threshold_dbfs", "ratio", "attack_ms", "release_ms"},
		"limiter":       {"ceiling_dbtp", "lookahead_ms"},
		"tolerance":     {"loudness_lu", "true_peak_db", "gain_db"},
		"program_truth": {"integrated_lufs", "post_compression_integrated_lufs", "true_peak_dbtp", "target_gain_db"},
	} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(root[key], &object); err != nil {
			return apperr.New(apperr.CodeSchema, "manifest field must be an object").WithField(key).WithPath(key)
		}
		for _, field := range fields {
			if _, ok := object[field]; !ok {
				path := key + "." + field
				return apperr.New(apperr.CodeSchema, "required manifest field is missing").WithField(path).WithPath(path)
			}
		}
	}
	var segments []map[string]json.RawMessage
	if err := json.Unmarshal(root["segments"], &segments); err != nil || len(segments) == 0 {
		return apperr.New(apperr.CodeSchema, "manifest segments must be a non-empty array").WithField("segments")
	}
	for i, segment := range segments {
		base := fmt.Sprintf("segments[%d]", i)
		for _, key := range []string{
			"index", "id", "channels", "sample_rate_hz", "duration_seconds", "frame_count",
			"audio", "content_sha256", "truth",
		} {
			if _, ok := segment[key]; !ok {
				return apperr.New(apperr.CodeSchema, "required manifest field is missing").
					WithField(base + "." + key).WithPath(base + "." + key)
			}
		}
		var truth map[string]json.RawMessage
		if err := json.Unmarshal(segment["truth"], &truth); err != nil {
			return apperr.New(apperr.CodeSchema, "manifest truth must be an object").WithField(base + ".truth")
		}
		for _, key := range []string{
			"integrated_lufs", "true_peak_dbtp", "target_gain_db", "segment_ideal_gain_db",
			"post_compression_integrated_lufs", "gate",
		} {
			if _, ok := truth[key]; !ok {
				return apperr.New(apperr.CodeSchema, "required truth field is missing").
					WithField(base + ".truth." + key).WithPath(base + ".truth." + key)
			}
		}
		var gate map[string]json.RawMessage
		if err := json.Unmarshal(truth["gate"], &gate); err != nil {
			return apperr.New(apperr.CodeSchema, "truth gate must be an object").WithField(base + ".truth.gate")
		}
		for _, key := range []string{
			"frame_count", "frames_above_absolute", "frames_above_both_gates",
			"absolute_threshold_lufs", "relative_threshold_lufs",
		} {
			if _, ok := gate[key]; !ok {
				path := base + ".truth.gate." + key
				return apperr.New(apperr.CodeSchema, "required truth gate field is missing").WithField(path).WithPath(path)
			}
		}
	}
	return nil
}

// ValidateAgainstManifest returns E_SCHEMA with the first conflicting path in
// a deterministic declaration order.
func ValidateAgainstManifest(program *Program, manifest *Manifest) error {
	if program == nil || manifest == nil {
		return apperr.New(apperr.CodeSchema, "program and manifest are required")
	}
	conflict := func(path string, expected, actual any) error {
		return apperr.New(apperr.CodeSchema, "program content conflicts with manifest").
			WithPath(path).WithField(path).WithExpected(expected).WithActual(actual)
	}
	if program.ProjectID != manifest.ProgramID {
		return conflict("program_id", manifest.ProgramID, program.ProjectID)
	}
	if program.SampleRateHz != manifest.SampleRateHz {
		return conflict("sample_rate_hz", manifest.SampleRateHz, program.SampleRateHz)
	}
	if !nearlyEqual(program.TargetLUFS, manifest.TargetLUFS, 1e-12) {
		return conflict("target_lufs", manifest.TargetLUFS, program.TargetLUFS)
	}
	if !nearlyEqual(program.FrameMs, manifest.FrameMs, 1e-12) {
		return conflict("frame_ms", manifest.FrameMs, program.FrameMs)
	}
	if !nearlyEqual(program.HopMs, manifest.HopMs, 1e-12) {
		return conflict("hop_ms", manifest.HopMs, program.HopMs)
	}
	if !nearlyEqual(program.Gate.AbsoluteLUFS, manifest.Gate.AbsoluteLUFS, 1e-12) {
		return conflict("gate.absolute_lufs", manifest.Gate.AbsoluteLUFS, program.Gate.AbsoluteLUFS)
	}
	if !nearlyEqual(program.Gate.RelativeLU, manifest.Gate.RelativeLU, 1e-12) {
		return conflict("gate.relative_lu", manifest.Gate.RelativeLU, program.Gate.RelativeLU)
	}
	if !nearlyEqual(program.Compressor.ThresholdDBFS, manifest.Compressor.ThresholdDBFS, 1e-12) ||
		!nearlyEqual(program.Compressor.Ratio, manifest.Compressor.Ratio, 1e-12) ||
		!nearlyEqual(program.Compressor.AttackMs, manifest.Compressor.AttackMs, 1e-12) ||
		!nearlyEqual(program.Compressor.ReleaseMs, manifest.Compressor.ReleaseMs, 1e-12) {
		return conflict("compressor", manifest.Compressor, program.Compressor)
	}
	if !nearlyEqual(program.Limiter.CeilingDBTP, manifest.Limiter.CeilingDBTP, 1e-12) ||
		!nearlyEqual(program.Limiter.LookaheadMs, manifest.Limiter.LookaheadMs, 1e-12) {
		return conflict("limiter", manifest.Limiter, program.Limiter)
	}
	if len(program.Segments) != len(manifest.Segments) {
		return conflict("segments", len(manifest.Segments), len(program.Segments))
	}
	for i, expected := range manifest.Segments {
		actual := program.Segments[i]
		base := fmt.Sprintf("segments[%d]", i)
		if actual.ID != expected.ID {
			return conflict(base+".id", expected.ID, actual.ID)
		}
		if actual.Channels != expected.Channels {
			return conflict(base+".channels", expected.Channels, actual.Channels)
		}
		if actual.Source.SampleRateHz != expected.SampleRateHz {
			return conflict(base+".sample_rate_hz", expected.SampleRateHz, actual.Source.SampleRateHz)
		}
		if actual.Frames() != expected.FrameCount {
			return conflict(base+".frame_count", expected.FrameCount, actual.Frames())
		}
		duration := float64(actual.Frames()) / float64(program.SampleRateHz)
		if math.Abs(duration-expected.DurationSeconds) > 0.51/float64(program.SampleRateHz) {
			return conflict(base+".duration_seconds", expected.DurationSeconds, duration)
		}
		if actual.ContentSHA256 != expected.ContentSHA256 {
			return conflict(base+".content_sha256", expected.ContentSHA256, actual.ContentSHA256)
		}
	}
	return nil
}

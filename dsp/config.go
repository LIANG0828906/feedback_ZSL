// Package dsp implements the deterministic K-weighting, gating, true-peak,
// compression, limiting and normalization primitives used by the workbench.
package dsp

import (
	"math"

	"loudnessworkbench/apperr"
)

type GateConfig struct {
	AbsoluteLUFS float64 `json:"absolute_lufs"`
	RelativeLU   float64 `json:"relative_lu"`
}

func (g GateConfig) Validate() error {
	if math.IsNaN(g.AbsoluteLUFS) || g.AbsoluteLUFS < -120 || g.AbsoluteLUFS > 0 {
		return apperr.New(apperr.CodeParameter, "absolute gate must be in [-120, 0] LUFS").
			WithField("gate.absolute_lufs").WithActual(g.AbsoluteLUFS)
	}
	if math.IsNaN(g.RelativeLU) || g.RelativeLU < 0 || g.RelativeLU > 60 {
		return apperr.New(apperr.CodeParameter, "relative gate must be in [0, 60] LU").
			WithField("gate.relative_lu").WithActual(g.RelativeLU)
	}
	return nil
}

type MeasureConfig struct {
	SampleRateHz int        `json:"sample_rate_hz"`
	FrameMs      float64    `json:"frame_ms"`
	HopMs        float64    `json:"hop_ms"`
	Gate         GateConfig `json:"gate"`
}

func DefaultMeasureConfig(sampleRate int) MeasureConfig {
	return MeasureConfig{
		SampleRateHz: sampleRate,
		FrameMs:      400,
		HopMs:        100,
		Gate:         GateConfig{AbsoluteLUFS: -70, RelativeLU: 10},
	}
}

func (c MeasureConfig) Validate() error {
	if !SupportedSampleRate(c.SampleRateHz) {
		return apperr.New(apperr.CodeParameter, "unsupported sample rate").
			WithField("sample_rate_hz").WithActual(c.SampleRateHz)
	}
	if math.IsNaN(c.FrameMs) || math.IsInf(c.FrameMs, 0) || c.FrameMs <= 0 || c.FrameMs > 60000 {
		return apperr.New(apperr.CodeParameter, "frame duration must be in (0, 60000] ms").
			WithField("frame_ms").WithActual(c.FrameMs)
	}
	if math.IsNaN(c.HopMs) || math.IsInf(c.HopMs, 0) || c.HopMs <= 0 || c.HopMs > 60000 {
		return apperr.New(apperr.CodeParameter, "hop duration must be in (0, 60000] ms").
			WithField("hop_ms").WithActual(c.HopMs)
	}
	frameSamples := int(math.Round(c.FrameMs * float64(c.SampleRateHz) / 1000))
	hopSamples := int(math.Round(c.HopMs * float64(c.SampleRateHz) / 1000))
	if frameSamples < 1 {
		return apperr.New(apperr.CodeParameter, "frame duration is shorter than one sample").
			WithField("frame_ms").WithActual(c.FrameMs)
	}
	if hopSamples < 1 {
		return apperr.New(apperr.CodeParameter, "hop duration is shorter than one sample").
			WithField("hop_ms").WithActual(c.HopMs)
	}
	if hopSamples > frameSamples {
		return apperr.New(apperr.CodeParameter, "hop duration must not exceed frame duration").
			WithField("hop_ms").WithActual(c.HopMs).WithLimit(c.FrameMs)
	}
	return c.Gate.Validate()
}

func (c MeasureConfig) FrameSamples() int {
	return int(math.Round(c.FrameMs * float64(c.SampleRateHz) / 1000))
}

func (c MeasureConfig) HopSamples() int {
	return int(math.Round(c.HopMs * float64(c.SampleRateHz) / 1000))
}

func SupportedSampleRate(rate int) bool {
	switch rate {
	case 8000, 11025, 16000, 22050, 24000, 32000, 44100, 48000, 88200, 96000, 192000:
		return true
	default:
		return false
	}
}

type CompressorConfig struct {
	ThresholdDBFS float64 `json:"threshold_dbfs"`
	Ratio         float64 `json:"ratio"`
	AttackMs      float64 `json:"attack_ms"`
	ReleaseMs     float64 `json:"release_ms"`
}

func (c CompressorConfig) Validate() error {
	if math.IsNaN(c.ThresholdDBFS) || c.ThresholdDBFS < -60 || c.ThresholdDBFS > 0 {
		return apperr.New(apperr.CodeParameter, "compressor threshold must be in [-60, 0] dBFS").
			WithField("compressor.threshold_dbfs").WithActual(c.ThresholdDBFS)
	}
	if math.IsNaN(c.Ratio) || c.Ratio < 1 || c.Ratio > 40 {
		return apperr.New(apperr.CodeParameter, "compressor ratio must be in [1, 40]").
			WithField("compressor.ratio").WithActual(c.Ratio)
	}
	if math.IsNaN(c.AttackMs) || c.AttackMs <= 0 || c.AttackMs > 5000 {
		return apperr.New(apperr.CodeParameter, "compressor attack must be in (0, 5000] ms").
			WithField("compressor.attack_ms").WithActual(c.AttackMs)
	}
	if math.IsNaN(c.ReleaseMs) || c.ReleaseMs <= 0 || c.ReleaseMs > 60000 {
		return apperr.New(apperr.CodeParameter, "compressor release must be in (0, 60000] ms").
			WithField("compressor.release_ms").WithActual(c.ReleaseMs)
	}
	return nil
}

type LimiterConfig struct {
	CeilingDBTP float64 `json:"ceiling_dbtp"`
	LookaheadMs float64 `json:"lookahead_ms"`
}

func (c LimiterConfig) Validate() error {
	if math.IsNaN(c.CeilingDBTP) || c.CeilingDBTP < -60 || c.CeilingDBTP > 0 {
		return apperr.New(apperr.CodeParameter, "limiter ceiling must be in [-60, 0] dBTP").
			WithField("limiter.ceiling_dbtp").WithActual(c.CeilingDBTP)
	}
	if math.IsNaN(c.LookaheadMs) || c.LookaheadMs < 0 || c.LookaheadMs > 500 {
		return apperr.New(apperr.CodeParameter, "limiter lookahead must be in [0, 500] ms").
			WithField("limiter.lookahead_ms").WithActual(c.LookaheadMs)
	}
	return nil
}

type ChainConfig struct {
	TargetLUFS float64          `json:"target_lufs"`
	FrameMs    float64          `json:"frame_ms"`
	HopMs      float64          `json:"hop_ms"`
	Gate       GateConfig       `json:"gate"`
	Compressor CompressorConfig `json:"compressor"`
	Limiter    LimiterConfig    `json:"limiter"`
}

func (c ChainConfig) Validate(sampleRate int) error {
	if math.IsNaN(c.TargetLUFS) || c.TargetLUFS < -60 || c.TargetLUFS > 0 {
		return apperr.New(apperr.CodeParameter, "target loudness must be in [-60, 0] LUFS").
			WithField("target_lufs").WithActual(c.TargetLUFS)
	}
	measure := MeasureConfig{SampleRateHz: sampleRate, FrameMs: c.FrameMs, HopMs: c.HopMs, Gate: c.Gate}
	if err := measure.Validate(); err != nil {
		return err
	}
	if err := c.Compressor.Validate(); err != nil {
		return err
	}
	return c.Limiter.Validate()
}

func DBToLinear(db float64) float64 {
	return math.Pow(10, db/20)
}

func LinearToDB(value float64) float64 {
	if value <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(value)
}

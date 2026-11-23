package dsp

import (
	"math"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
)

type CompressorResult struct {
	Signal         audio.Signal
	GainDB         []float64
	MaxReductionDB float64
}

// Compress uses a stereo-linked peak detector and one-pole attack/release
// smoothing. Both channels always receive the same gain, preserving the image.
func Compress(signal audio.Signal, cfg CompressorConfig) (CompressorResult, error) {
	if err := signal.Validate(); err != nil {
		return CompressorResult{}, err
	}
	if err := cfg.Validate(); err != nil {
		return CompressorResult{}, err
	}
	out := signal.Clone()
	gainDB := make([]float64, signal.Frames())
	gain := 1.0
	maxReduction := 0.0
	attackCoefficient := math.Exp(-1 / ((cfg.AttackMs / 1000) * float64(signal.SampleRateHz)))
	releaseCoefficient := math.Exp(-1 / ((cfg.ReleaseMs / 1000) * float64(signal.SampleRateHz)))

	for frame := 0; frame < signal.Frames(); frame++ {
		peak := 0.0
		for channel := 0; channel < signal.Channels; channel++ {
			value := math.Abs(signal.At(frame, channel))
			if value > peak {
				peak = value
			}
		}
		targetGain := 1.0
		if peak > 0 {
			levelDB := LinearToDB(peak)
			if levelDB > cfg.ThresholdDBFS {
				overDB := levelDB - cfg.ThresholdDBFS
				reductionDB := overDB * (1 - 1/cfg.Ratio)
				targetGain = DBToLinear(-reductionDB)
			}
		}
		coefficient := releaseCoefficient
		if targetGain < gain {
			coefficient = attackCoefficient
		}
		gain = coefficient*gain + (1-coefficient)*targetGain
		if gain > 1 {
			gain = 1
		}
		reduction := -LinearToDB(gain)
		if reduction > maxReduction {
			maxReduction = reduction
		}
		gainDB[frame] = -reduction
		for channel := 0; channel < signal.Channels; channel++ {
			out.Data[frame*signal.Channels+channel] = signal.At(frame, channel) * gain
		}
	}
	return CompressorResult{Signal: out, GainDB: gainDB, MaxReductionDB: maxReduction}, nil
}

type LimiterResult struct {
	Signal           audio.Signal
	GainDB           []float64
	SafetyTrimDB     float64
	OutputTruePeakDB float64
}

// Limit applies a lookahead sample-peak envelope with one-pole attack/release
// smoothing, then a final true-peak safety trim. The final safety trim is a
// program-wide constant for this segment, never a per-sample discontinuity.
func Limit(signal audio.Signal, cfg LimiterConfig) (LimiterResult, error) {
	if err := signal.Validate(); err != nil {
		return LimiterResult{}, err
	}
	if err := cfg.Validate(); err != nil {
		return LimiterResult{}, err
	}
	frames := signal.Frames()
	out := signal.Clone()
	gainCurve := make([]float64, frames)
	if frames == 0 {
		return LimiterResult{Signal: out, GainDB: gainCurve, OutputTruePeakDB: math.Inf(-1)}, nil
	}
	ceilingLinear := DBToLinear(cfg.CeilingDBTP)
	lookahead := int(math.Round(cfg.LookaheadMs * float64(signal.SampleRateHz) / 1000))
	if lookahead >= frames {
		lookahead = frames - 1
	}
	if lookahead < 0 {
		lookahead = 0
	}
	needed := make([]float64, frames)
	for frame := 0; frame < frames; frame++ {
		peak := 0.0
		for channel := 0; channel < signal.Channels; channel++ {
			value := math.Abs(signal.At(frame, channel))
			if value > peak {
				peak = value
			}
		}
		if peak <= ceilingLinear || peak == 0 {
			needed[frame] = 1
		} else {
			needed[frame] = ceilingLinear / peak
		}
	}
	// Sliding minimum over [frame, frame+lookahead] gives the limiter advance
	// warning without dropping or reordering samples.
	rawGain := slidingMinimum(needed, lookahead)
	attackMs := cfg.LookaheadMs / 3
	if attackMs < 0.25 {
		attackMs = 0.25
	}
	const releaseMs = 80.0
	attackCoefficient := math.Exp(-1 / ((attackMs / 1000) * float64(signal.SampleRateHz)))
	releaseCoefficient := math.Exp(-1 / ((releaseMs / 1000) * float64(signal.SampleRateHz)))
	gain := 1.0
	for frame := 0; frame < frames; frame++ {
		target := rawGain[frame]
		coefficient := releaseCoefficient
		if target < gain {
			coefficient = attackCoefficient
		}
		gain = coefficient*gain + (1-coefficient)*target
		if gain > 1 {
			gain = 1
		}
		gainCurve[frame] = gain
		for channel := 0; channel < signal.Channels; channel++ {
			out.Data[frame*signal.Channels+channel] = signal.At(frame, channel) * gain
		}
	}

	base := append([]float64(nil), out.Data...)
	scale := 1.0
	const attempts = 8
	for attempt := 0; attempt < attempts; attempt++ {
		for i, value := range base {
			out.Data[i] = float64(float32(value * scale))
		}
		peak := TruePeakLinear(out)
		if peak <= ceilingLinear*(1+1e-12) {
			safetyDB := LinearToDB(scale)
			if math.IsInf(safetyDB, -1) {
				safetyDB = 0
			}
			for i := range gainCurve {
				gainCurve[i] *= scale
			}
			return LimiterResult{
				Signal:           out,
				GainDB:           gainCurve,
				SafetyTrimDB:     safetyDB,
				OutputTruePeakDB: LinearToDB(peak),
			}, nil
		}
		scale *= (ceilingLinear / peak) * 0.999999
	}
	err := apperr.New(apperr.CodeClip, "limiter could not keep true peak below ceiling").
		WithField("limiter.ceiling_dbtp").
		WithActual(TruePeakDBTP(out)).
		WithLimit(cfg.CeilingDBTP)
	return LimiterResult{}, err
}

// CheckTruePeakCeiling is the explicit E_CLIP post-condition used by the
// engine and available for diagnostics/tests.
func CheckTruePeakCeiling(signal audio.Signal, ceilingDBTP float64) error {
	measured := TruePeakDBTP(signal)
	if measured > ceilingDBTP+1e-7 {
		return apperr.New(apperr.CodeClip, "measured true peak exceeds limiter ceiling").
			WithField("limiter.ceiling_dbtp").
			WithActual(measured).WithLimit(ceilingDBTP)
	}
	return nil
}

func slidingMinimum(values []float64, window int) []float64 {
	n := len(values)
	out := make([]float64, n)
	if n == 0 {
		return out
	}
	if window < 0 {
		window = 0
	}
	deque := make([]int, 0, window+1)
	push := func(index int) {
		for len(deque) > 0 && values[deque[len(deque)-1]] >= values[index] {
			deque = deque[:len(deque)-1]
		}
		deque = append(deque, index)
	}
	initialEnd := window
	if initialEnd >= n {
		initialEnd = n - 1
	}
	for index := 0; index <= initialEnd; index++ {
		push(index)
	}
	for frame := 0; frame < n; frame++ {
		next := frame + window
		if next < n && next > initialEnd {
			push(next)
		}
		for len(deque) > 0 && deque[0] < frame {
			deque = deque[1:]
		}
		out[frame] = values[deque[0]]
	}
	return out
}

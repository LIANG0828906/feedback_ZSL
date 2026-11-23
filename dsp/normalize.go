package dsp

import (
	"math"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
)

type NormalizedSegment struct {
	Output       audio.Signal
	Measurement  Measurement
	SafetyTrimDB float64
}

type ProgramNormalization struct {
	GainDB        float64
	Segments      []NormalizedSegment
	Program       Measurement
	Iterations    int
	Converged     bool
	InitialGainDB float64
}

// NormalizeProgram solves the global normalization gain with the limiter in
// the loop. This keeps the final gated program loudness on target while never
// allowing a segment true peak above the configured ceiling. All segments are
// updated together, so the decision remains a global one.
func NormalizeProgram(
	compressed []audio.Signal,
	initialGainDB float64,
	measureConfig MeasureConfig,
	limiterConfig LimiterConfig,
	targetLUFS float64,
) (ProgramNormalization, error) {
	if len(compressed) == 0 {
		return ProgramNormalization{}, apperr.New(apperr.CodeParameter, "at least one segment is required").
			WithField("segments")
	}
	if err := measureConfig.Validate(); err != nil {
		return ProgramNormalization{}, err
	}
	if err := limiterConfig.Validate(); err != nil {
		return ProgramNormalization{}, err
	}
	if math.IsNaN(targetLUFS) || targetLUFS < -60 || targetLUFS > 0 {
		return ProgramNormalization{}, apperr.New(apperr.CodeParameter, "target loudness is invalid").
			WithField("target_lufs").WithActual(targetLUFS)
	}
	gain := initialGainDB
	var best ProgramNormalization
	bestError := math.Inf(1)
	const maxIterations = 8
	for iteration := 0; iteration < maxIterations; iteration++ {
		candidate, err := evaluateNormalization(compressed, gain, measureConfig, limiterConfig)
		if err != nil {
			return ProgramNormalization{}, err
		}
		candidate.GainDB = gain
		candidate.Iterations = iteration + 1
		candidate.InitialGainDB = initialGainDB
		if math.IsInf(candidate.Program.IntegratedLUFS, -1) {
			return ProgramNormalization{}, apperr.New(apperr.CodeParameter, "normalized program has no gated frames").
				WithField("program_loudness_lufs")
		}
		errorDB := targetLUFS - candidate.Program.IntegratedLUFS
		if math.Abs(errorDB) < bestError {
			bestError = math.Abs(errorDB)
			best = candidate
		}
		if math.Abs(errorDB) <= 0.005 {
			candidate.Converged = true
			best = candidate
			return best, nil
		}
		// The limiter is nonlinear. A damped update converges monotonically on
		// reachable targets and avoids overshoot near the ceiling.
		gain += errorDB * 0.8
	}
	return best, nil
}

func evaluateNormalization(
	compressed []audio.Signal,
	gainDB float64,
	measureConfig MeasureConfig,
	limiterConfig LimiterConfig,
) (ProgramNormalization, error) {
	gain := DBToLinear(gainDB)
	result := ProgramNormalization{Segments: make([]NormalizedSegment, len(compressed))}
	measurements := make([]Measurement, len(compressed))
	for i, signal := range compressed {
		if signal.SampleRateHz != measureConfig.SampleRateHz {
			return ProgramNormalization{}, apperr.New(apperr.CodeSchema, "segment sample rate differs from normalization config").
				WithField("sample_rate_hz").WithExpected(measureConfig.SampleRateHz).WithActual(signal.SampleRateHz).
				WithSegment(i, "")
		}
		limited, err := Limit(audio.Scale(signal, gain), limiterConfig)
		if err != nil {
			if e, ok := apperr.As(err); ok {
				e.WithSegment(i, "")
				return ProgramNormalization{}, e
			}
			return ProgramNormalization{}, apperr.Wrap(apperr.CodeInternal, err.Error(), err).WithSegment(i, "")
		}
		measurement, err := MeasureLoudness(limited.Signal, measureConfig)
		if err != nil {
			if e, ok := apperr.As(err); ok {
				e.WithSegment(i, "")
				return ProgramNormalization{}, e
			}
			return ProgramNormalization{}, err
		}
		measurement.TruePeakDBTP = limited.OutputTruePeakDB
		if err := CheckTruePeakCeiling(limited.Signal, limiterConfig.CeilingDBTP); err != nil {
			if e, ok := apperr.As(err); ok {
				e.WithSegment(i, "")
			}
			return ProgramNormalization{}, err
		}
		result.Segments[i] = NormalizedSegment{
			Output: limited.Signal, Measurement: measurement, SafetyTrimDB: limited.SafetyTrimDB,
		}
		measurements[i] = measurement
	}
	program, err := CombineMeasurements(measurements, measureConfig.Gate)
	if err != nil {
		return ProgramNormalization{}, err
	}
	result.Program = program
	return result, nil
}

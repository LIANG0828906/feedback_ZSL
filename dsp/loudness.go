package dsp

import (
	"encoding/json"
	"math"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
)

type Measurement struct {
	IntegratedLUFS        float64   `json:"integrated_lufs"`
	TruePeakDBTP          float64   `json:"true_peak_dbtp"`
	UngatedLUFS           float64   `json:"ungated_lufs"`
	RelativeThresholdLUFS float64   `json:"relative_threshold_lufs"`
	FramesTotal           int       `json:"frames_total"`
	FramesAboveAbsolute   int       `json:"frames_above_absolute"`
	FramesAboveBothGates  int       `json:"frames_above_both_gates"`
	FramePowers           []float64 `json:"-"`
}

type measurementWire struct {
	IntegratedLUFS        *float64 `json:"integrated_lufs"`
	TruePeakDBTP          *float64 `json:"true_peak_dbtp"`
	UngatedLUFS           *float64 `json:"ungated_lufs"`
	RelativeThresholdLUFS *float64 `json:"relative_threshold_lufs"`
	FramesTotal           int      `json:"frames_total"`
	FramesAboveAbsolute   int      `json:"frames_above_absolute"`
	FramesAboveBothGates  int      `json:"frames_above_both_gates"`
}

func wireLevel(value float64) *float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	copy := value
	return &copy
}

func levelOrNegativeInfinity(value *float64) float64 {
	if value == nil {
		return math.Inf(-1)
	}
	return *value
}

// MarshalJSON represents non-finite loudness levels (silence) as null while
// retaining a valid JSON document.
func (m Measurement) MarshalJSON() ([]byte, error) {
	return json.Marshal(measurementWire{
		IntegratedLUFS:        wireLevel(m.IntegratedLUFS),
		TruePeakDBTP:          wireLevel(m.TruePeakDBTP),
		UngatedLUFS:           wireLevel(m.UngatedLUFS),
		RelativeThresholdLUFS: wireLevel(m.RelativeThresholdLUFS),
		FramesTotal:           m.FramesTotal,
		FramesAboveAbsolute:   m.FramesAboveAbsolute,
		FramesAboveBothGates:  m.FramesAboveBothGates,
	})
}

func (m *Measurement) UnmarshalJSON(data []byte) error {
	var wire measurementWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	m.IntegratedLUFS = levelOrNegativeInfinity(wire.IntegratedLUFS)
	m.TruePeakDBTP = levelOrNegativeInfinity(wire.TruePeakDBTP)
	m.UngatedLUFS = levelOrNegativeInfinity(wire.UngatedLUFS)
	m.RelativeThresholdLUFS = levelOrNegativeInfinity(wire.RelativeThresholdLUFS)
	m.FramesTotal = wire.FramesTotal
	m.FramesAboveAbsolute = wire.FramesAboveAbsolute
	m.FramesAboveBothGates = wire.FramesAboveBothGates
	return nil
}

// Measure applies K-weighting, computes per-frame mean-square energy and gates
// the integrated loudness according to BS.1770. True peak is measured on the
// unweighted source.
func Measure(signal audio.Signal, cfg MeasureConfig) (Measurement, error) {
	measurement, err := MeasureLoudness(signal, cfg)
	if err != nil {
		return Measurement{}, err
	}
	measurement.TruePeakDBTP = TruePeakDBTP(signal)
	return measurement, nil
}

// MeasureLoudness is the K-weighted/gated subset used by normalization
// iterations where true peak is already supplied by the limiter.
func MeasureLoudness(signal audio.Signal, cfg MeasureConfig) (Measurement, error) {
	if err := signal.Validate(); err != nil {
		return Measurement{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Measurement{}, err
	}
	if signal.SampleRateHz != cfg.SampleRateHz {
		return Measurement{}, apperr.New(apperr.CodeSchema, "signal sample rate differs from measurement config").
			WithField("sample_rate_hz").WithExpected(cfg.SampleRateHz).WithActual(signal.SampleRateHz)
	}
	frameSamples := cfg.FrameSamples()
	hopSamples := cfg.HopSamples()
	frames := 0
	if signal.Frames() >= frameSamples {
		frames = (signal.Frames()-frameSamples)/hopSamples + 1
	}

	energyPerFrame := make([]float64, frames)
	energyPerSample := make([]float64, signal.Frames())
	for channel := 0; channel < signal.Channels; channel++ {
		filter, err := newKFilter(signal.SampleRateHz)
		if err != nil {
			return Measurement{}, err
		}
		for frame := 0; frame < signal.Frames(); frame++ {
			y := filter.Process(signal.At(frame, channel))
			energyPerSample[frame] += y * y
		}
	}
	for frame := 0; frame < frames; frame++ {
		start := frame * hopSamples
		sum := 0.0
		for sample := start; sample < start+frameSamples; sample++ {
			sum += energyPerSample[sample]
		}
		energyPerFrame[frame] = sum / float64(frameSamples)
	}

	measurement := gatePowers(energyPerFrame, cfg.Gate)
	return measurement, nil
}

// CombineMeasurements gates the union of frame energies from program segments.
// Frames do not cross segment boundaries by design, keeping parallel and serial
// decisions identical.
func CombineMeasurements(parts []Measurement, gate GateConfig) (Measurement, error) {
	if err := gate.Validate(); err != nil {
		return Measurement{}, err
	}
	all := make([]float64, 0)
	truePeak := math.Inf(-1)
	for _, part := range parts {
		all = append(all, part.FramePowers...)
		if part.TruePeakDBTP > truePeak {
			truePeak = part.TruePeakDBTP
		}
	}
	combined := gatePowers(all, gate)
	combined.TruePeakDBTP = truePeak
	return combined, nil
}

func gatePowers(powers []float64, gate GateConfig) Measurement {
	result := Measurement{
		FramePowers:           append([]float64(nil), powers...),
		FramesTotal:           len(powers),
		IntegratedLUFS:        math.Inf(-1),
		UngatedLUFS:           math.Inf(-1),
		RelativeThresholdLUFS: math.Inf(-1),
		TruePeakDBTP:          math.Inf(-1),
	}
	absoluteLinear := math.Pow(10, (gate.AbsoluteLUFS+0.691)/10)
	absoluteSum := 0.0
	absoluteCount := 0
	for _, power := range powers {
		if power > absoluteLinear {
			absoluteSum += power
			absoluteCount++
		}
	}
	result.FramesAboveAbsolute = absoluteCount
	if absoluteCount == 0 {
		return result
	}
	ungatedMean := absoluteSum / float64(absoluteCount)
	result.UngatedLUFS = -0.691 + 10*math.Log10(ungatedMean)
	result.RelativeThresholdLUFS = result.UngatedLUFS - gate.RelativeLU
	relativeLinear := ungatedMean * math.Pow(10, -gate.RelativeLU/10)

	finalSum := 0.0
	finalCount := 0
	for _, power := range powers {
		if power > absoluteLinear && power > relativeLinear {
			finalSum += power
			finalCount++
		}
	}
	result.FramesAboveBothGates = finalCount
	if finalCount > 0 {
		result.IntegratedLUFS = -0.691 + 10*math.Log10(finalSum/float64(finalCount))
	}
	return result
}

func GatingError(actual, expected Measurement) (float64, float64) {
	loudnessError := math.Abs(actual.IntegratedLUFS - expected.IntegratedLUFS)
	peakError := math.Abs(actual.TruePeakDBTP - expected.TruePeakDBTP)
	if math.IsNaN(loudnessError) {
		loudnessError = math.Inf(1)
	}
	if math.IsNaN(peakError) {
		peakError = math.Inf(1)
	}
	return loudnessError, peakError
}

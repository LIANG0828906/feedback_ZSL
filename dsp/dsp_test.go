package dsp

import (
	"encoding/json"
	"math"
	"testing"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
)

func sineSignal(rate, frames, channels int, frequency, amplitude float64) audio.Signal {
	signal := audio.New(rate, channels, frames)
	for frame := 0; frame < frames; frame++ {
		value := amplitude * math.Sin(2*math.Pi*frequency*float64(frame)/float64(rate))
		for channel := 0; channel < channels; channel++ {
			signal.Set(frame, channel, value)
		}
	}
	return signal
}

func TestKWeightingCoefficientsMatch48kReference(t *testing.T) {
	shelf, highPass, err := KWeightingCoefficients(48000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(shelf.B0-1.53512485958697) > 1e-12 ||
		math.Abs(shelf.B1-(-2.69169618940638)) > 1e-12 ||
		math.Abs(shelf.B2-1.19839281085285) > 1e-12 {
		t.Fatalf("unexpected shelf coefficients: %+v", shelf)
	}
	if math.Abs(highPass.A1-(-1.99004745483398)) > 1e-12 ||
		math.Abs(highPass.A2-0.99007225036621) > 1e-12 {
		t.Fatalf("unexpected high-pass coefficients: %+v", highPass)
	}
}

func TestMeasureStereoThousandHertzSine(t *testing.T) {
	signal := sineSignal(48000, 48000*2, 2, 1000, 0.1)
	cfg := DefaultMeasureConfig(48000)
	measurement, err := Measure(signal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if math.IsInf(measurement.IntegratedLUFS, 0) {
		t.Fatal("integrated loudness is not finite")
	}
	if measurement.IntegratedLUFS < -21 || measurement.IntegratedLUFS > -19 {
		t.Fatalf("1 kHz stereo -20 dBFS peak sine measured %.3f LUFS, expected near -20", measurement.IntegratedLUFS)
	}
	if math.Abs(measurement.TruePeakDBTP-(-20.0)) > 0.05 {
		t.Fatalf("true peak %.6f dBTP, want about -20", measurement.TruePeakDBTP)
	}
	if measurement.FramesTotal != 17 {
		t.Fatalf("frame count = %d, want 17", measurement.FramesTotal)
	}
}

func TestMeasureSilenceGatesEverything(t *testing.T) {
	signal := audio.New(48000, 1, 48000)
	measurement, err := Measure(signal, DefaultMeasureConfig(48000))
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsInf(measurement.IntegratedLUFS, -1) {
		t.Fatalf("silence loudness = %v, want -Inf", measurement.IntegratedLUFS)
	}
	if measurement.FramesAboveAbsolute != 0 || measurement.FramesAboveBothGates != 0 {
		t.Fatal("silence frames passed the gates")
	}
	data, err := json.Marshal(measurement)
	if err != nil {
		t.Fatalf("silence measurement is not JSON encodable: %v", err)
	}
	var decoded Measurement
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !math.IsInf(decoded.IntegratedLUFS, -1) {
		t.Fatalf("decoded silence loudness = %v", decoded.IntegratedLUFS)
	}
}

func TestMeasureParameterErrors(t *testing.T) {
	signal := sineSignal(48000, 48000, 1, 1000, 0.1)
	cfg := DefaultMeasureConfig(11000)
	_, err := Measure(signal, cfg)
	if !apperr.IsCode(err, apperr.CodeParameter) {
		t.Fatalf("unsupported rate: got %v", err)
	}
	cfg = DefaultMeasureConfig(48000)
	cfg.FrameMs = 0
	if _, err := Measure(signal, cfg); !apperr.IsCode(err, apperr.CodeParameter) {
		t.Fatalf("zero frame: got %v", err)
	}
	cfg = DefaultMeasureConfig(48000)
	cfg.HopMs = 500
	if _, err := Measure(signal, cfg); !apperr.IsCode(err, apperr.CodeParameter) {
		t.Fatalf("hop > frame: got %v", err)
	}
}

func TestTruePeakEstimatesInterSamplePeak(t *testing.T) {
	signal := sineSignal(48000, 4800, 1, 12000, 0.5)
	fade := 48
	for frame := 0; frame < fade; frame++ {
		gain := float64(frame) / float64(fade)
		signal.Data[frame] *= gain
		signal.Data[len(signal.Data)-1-frame] *= gain
	}
	peak := TruePeakLinear(signal)
	if peak < 0.49 || peak > 0.52 {
		t.Fatalf("true peak = %.6f, want about 0.5", peak)
	}
}

func TestCompressorPreservesShapeAndSmoothsGain(t *testing.T) {
	signal := sineSignal(48000, 48000, 2, 1000, 0.6)
	cfg := CompressorConfig{ThresholdDBFS: -20, Ratio: 4, AttackMs: 20, ReleaseMs: 100}
	result, err := Compress(signal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Signal.Frames() != signal.Frames() || result.Signal.Channels != signal.Channels {
		t.Fatal("compressor changed signal shape")
	}
	if result.MaxReductionDB <= 0 {
		t.Fatal("expected gain reduction")
	}
	for i := 1; i < len(result.GainDB); i++ {
		if math.Abs(result.GainDB[i]-result.GainDB[i-1]) > 0.1 {
			t.Fatalf("gain stepped by %.6f dB at sample %d", result.GainDB[i]-result.GainDB[i-1], i)
		}
	}
}

func TestLimiterMeetsCeilingAndPreservesShape(t *testing.T) {
	signal := sineSignal(48000, 48000, 2, 700, 0.95)
	cfg := LimiterConfig{CeilingDBTP: -3, LookaheadMs: 5}
	result, err := Limit(signal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Signal.Frames() != signal.Frames() || result.Signal.Channels != signal.Channels {
		t.Fatal("limiter changed signal shape")
	}
	if got := TruePeakDBTP(result.Signal); got > cfg.CeilingDBTP+1e-6 {
		t.Fatalf("limiter output true peak %.6f > ceiling %.6f", got, cfg.CeilingDBTP)
	}
	if err := CheckTruePeakCeiling(signal, cfg.CeilingDBTP); !apperr.IsCode(err, apperr.CodeClip) {
		t.Fatalf("check raw over-ceiling signal: got %v, want E_CLIP", err)
	}
}

func TestCombinedMeasurementsUseAllFramePowers(t *testing.T) {
	cfg := DefaultMeasureConfig(48000)
	first, err := Measure(sineSignal(48000, 48000, 1, 1000, 0.1), cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Measure(sineSignal(48000, 48000, 1, 1000, 0.05), cfg)
	if err != nil {
		t.Fatal(err)
	}
	combined, err := CombineMeasurements([]Measurement{first, second}, cfg.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if combined.FramesTotal != first.FramesTotal+second.FramesTotal {
		t.Fatalf("combined frame count = %d", combined.FramesTotal)
	}
	if combined.IntegratedLUFS >= first.IntegratedLUFS {
		t.Fatal("quieter second segment should reduce combined loudness")
	}
	expectedPeak := math.Max(first.TruePeakDBTP, second.TruePeakDBTP)
	if combined.TruePeakDBTP != expectedPeak {
		t.Fatalf("combined true peak %.6f, want %.6f", combined.TruePeakDBTP, expectedPeak)
	}
}

func TestNormalizeProgramReachesTargetUnderLimiter(t *testing.T) {
	first := sineSignal(48000, 72000, 2, 1000, 0.5)
	second := sineSignal(48000, 72000, 1, 440, 0.2)
	cfg := DefaultMeasureConfig(48000)
	compressor := CompressorConfig{ThresholdDBFS: -20, Ratio: 3, AttackMs: 20, ReleaseMs: 200}
	firstCompressed, err := Compress(first, compressor)
	if err != nil {
		t.Fatal(err)
	}
	secondCompressed, err := Compress(second, compressor)
	if err != nil {
		t.Fatal(err)
	}
	firstMeasure, _ := Measure(firstCompressed.Signal, cfg)
	secondMeasure, _ := Measure(secondCompressed.Signal, cfg)
	compressedProgram, _ := CombineMeasurements([]Measurement{firstMeasure, secondMeasure}, cfg.Gate)
	limiter := LimiterConfig{CeilingDBTP: -1, LookaheadMs: 5}
	normalization, err := NormalizeProgram(
		[]audio.Signal{firstCompressed.Signal, secondCompressed.Signal},
		-20-compressedProgram.IntegratedLUFS, cfg, limiter, -20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(normalization.Program.IntegratedLUFS-(-20)) > 0.02 {
		t.Fatalf("final loudness %.4f, want -20", normalization.Program.IntegratedLUFS)
	}
	for _, segment := range normalization.Segments {
		if peak := TruePeakDBTP(segment.Output); peak > limiter.CeilingDBTP+1e-6 {
			t.Fatalf("segment peak %.6f above ceiling", peak)
		}
	}
}

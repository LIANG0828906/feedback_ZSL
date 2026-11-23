// Package samplegen generates the deterministic, self-contained sample program used
// by the CLI, tests and verification report.
package samplegen

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
	"loudnessworkbench/dsp"
	"loudnessworkbench/project"
)

const (
	DefaultSeed       int64 = 20261124
	DefaultSampleRate       = 48000
	DefaultTargetLUFS       = -23
)

type Result struct {
	ManifestPath string            `json:"manifest_path"`
	AudioDir     string            `json:"audio_dir"`
	ProjectsDir  string            `json:"projects_dir"`
	ProjectPaths map[string]string `json:"project_paths"`
	Manifest     *project.Manifest `json:"manifest"`
}

type segmentSpec struct {
	id       string
	channels int
	duration float64
	kind     int
}

var defaultSpecs = []segmentSpec{
	{id: "seg-01-tonal-bed", channels: 2, duration: 3.2, kind: 1},
	{id: "seg-02-speech", channels: 1, duration: 2.8, kind: 2},
	{id: "seg-03-transients", channels: 2, duration: 3.0, kind: 3},
	{id: "seg-04-gated", channels: 1, duration: 2.6, kind: 4},
	{id: "seg-05-wide-music", channels: 2, duration: 3.4, kind: 5},
	{id: "seg-06-quiet", channels: 1, duration: 2.5, kind: 6},
}

func DefaultChain() dsp.ChainConfig {
	return dsp.ChainConfig{
		TargetLUFS: DefaultTargetLUFS,
		FrameMs:    400,
		HopMs:      100,
		Gate:       dsp.GateConfig{AbsoluteLUFS: -70, RelativeLU: 10},
		Compressor: dsp.CompressorConfig{
			ThresholdDBFS: -20,
			Ratio:         3,
			AttackMs:      20,
			ReleaseMs:     250,
		},
		Limiter: dsp.LimiterConfig{CeilingDBTP: -1, LookaheadMs: 5},
	}
}

// Generate writes WAV samples, truth manifest and v1/v2/v3 project files.
func Generate(outDir string, seed int64) (*Result, error) {
	if seed == 0 {
		seed = DefaultSeed
	}
	audioDir := filepath.Join(outDir, "audio")
	projectsDir := filepath.Join(outDir, "projects")
	if err := os.MkdirAll(audioDir, 0o755); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "cannot create sample directory", err)
	}
	chain := DefaultChain()
	if err := chain.Validate(DefaultSampleRate); err != nil {
		return nil, err
	}
	measureConfig := dsp.MeasureConfig{
		SampleRateHz: DefaultSampleRate,
		FrameMs:      chain.FrameMs,
		HopMs:        chain.HopMs,
		Gate:         chain.Gate,
	}
	signals := make([]audio.Signal, len(defaultSpecs))
	paths := make([]string, len(defaultSpecs))
	hashes := make([]string, len(defaultSpecs))
	for i, spec := range defaultSpecs {
		signal := generateSegment(seed+int64(i)*7919, DefaultSampleRate, spec.channels, spec.duration, spec.kind)
		signal = audio.QuantizeF32(signal)
		if err := signal.Validate(); err != nil {
			return nil, err
		}
		path := filepath.Join(audioDir, spec.id+".wav")
		if err := audio.WriteWAV(path, signal); err != nil {
			return nil, err
		}
		signals[i] = signal
		paths[i] = path
		hashes[i] = audio.ContentHash(signal)
	}

	sourceMeasurements := make([]dsp.Measurement, len(signals))
	compressedMeasurements := make([]dsp.Measurement, len(signals))
	compressedSignals := make([]audio.Signal, len(signals))
	for i, signal := range signals {
		sourceMeasurement, err := dsp.Measure(signal, measureConfig)
		if err != nil {
			return nil, err
		}
		compressed, err := dsp.Compress(signal, chain.Compressor)
		if err != nil {
			return nil, err
		}
		compressedMeasurement, err := dsp.Measure(compressed.Signal, measureConfig)
		if err != nil {
			return nil, err
		}
		sourceMeasurements[i] = sourceMeasurement
		compressedMeasurements[i] = compressedMeasurement
		compressedSignals[i] = compressed.Signal
	}
	sourceProgram, err := dsp.CombineMeasurements(sourceMeasurements, chain.Gate)
	if err != nil {
		return nil, err
	}
	compressedProgram, err := dsp.CombineMeasurements(compressedMeasurements, chain.Gate)
	if err != nil {
		return nil, err
	}
	initialGain := chain.TargetLUFS - compressedProgram.IntegratedLUFS
	normalization, err := dsp.NormalizeProgram(compressedSignals, initialGain, measureConfig, chain.Limiter, chain.TargetLUFS)
	if err != nil {
		return nil, err
	}
	globalGain := normalization.GainDB

	manifest := &project.Manifest{
		Schema:       "lw-manifest/1",
		ProgramID:    fmt.Sprintf("game-audio-demo-%d", seed),
		Seed:         seed,
		SampleRateHz: DefaultSampleRate,
		TargetLUFS:   chain.TargetLUFS,
		FrameMs:      chain.FrameMs,
		HopMs:        chain.HopMs,
		Gate:         chain.Gate,
		Compressor:   chain.Compressor,
		Limiter:      chain.Limiter,
		Tolerance: project.Tolerance{
			LoudnessLU: 0.05,
			TruePeakDB: 0.10,
			GainDB:     0.05,
		},
		Segments: make([]project.ManifestSegment, len(defaultSpecs)),
		ProgramTruth: project.ProgramTruth{
			IntegratedLUFS:                sourceProgram.IntegratedLUFS,
			PostCompressionIntegratedLUFS: compressedProgram.IntegratedLUFS,
			TruePeakDBTP:                  sourceProgram.TruePeakDBTP,
			TargetGainDB:                  globalGain,
		},
	}
	for i, spec := range defaultSpecs {
		source := sourceMeasurements[i]
		manifest.Segments[i] = project.ManifestSegment{
			Index:           i,
			ID:              spec.id,
			Channels:        spec.channels,
			SampleRateHz:    DefaultSampleRate,
			DurationSeconds: float64(signals[i].Frames()) / float64(DefaultSampleRate),
			FrameCount:      signals[i].Frames(),
			Audio:           filepath.ToSlash(filepath.Join("audio", spec.id+".wav")),
			ContentSHA256:   hashes[i],
			Truth: project.SegmentTruth{
				IntegratedLUFS:                source.IntegratedLUFS,
				TruePeakDBTP:                  source.TruePeakDBTP,
				TargetGainDB:                  globalGain,
				SegmentIdealGainDB:            chain.TargetLUFS - source.IntegratedLUFS,
				PostCompressionIntegratedLUFS: compressedMeasurements[i].IntegratedLUFS,
				Gate: project.GateTruth{
					FrameCount:            source.FramesTotal,
					AboveAbsolute:         source.FramesAboveAbsolute,
					AboveBothGates:        source.FramesAboveBothGates,
					AbsoluteThresholdLUFS: chain.Gate.AbsoluteLUFS,
					RelativeThresholdLUFS: source.RelativeThresholdLUFS,
				},
			},
		}
	}
	manifestPath := filepath.Join(outDir, "manifest.json")
	if err := writeJSON(manifestPath, manifest); err != nil {
		return nil, err
	}

	program := &project.Program{
		ProjectID:     manifest.ProgramID,
		SourceVersion: project.SourceV1,
		SampleRateHz:  DefaultSampleRate,
		TargetLUFS:    chain.TargetLUFS,
		FrameMs:       chain.FrameMs,
		HopMs:         chain.HopMs,
		Gate:          chain.Gate,
		Compressor:    chain.Compressor,
		Limiter:       chain.Limiter,
		StageOrder:    append([]string(nil), project.CanonicalStageOrder...),
		Segments:      make([]project.Segment, len(defaultSpecs)),
	}
	for i, spec := range defaultSpecs {
		program.Segments[i] = project.Segment{
			Index: i, ID: spec.id, Channels: spec.channels,
			SourcePath: paths[i], ContentSHA256: hashes[i], Source: signals[i],
		}
	}
	projectPaths, err := project.WriteVersions(projectsDir, program)
	if err != nil {
		return nil, err
	}
	return &Result{
		ManifestPath: manifestPath,
		AudioDir:     audioDir,
		ProjectsDir:  projectsDir,
		ProjectPaths: projectPaths,
		Manifest:     manifest,
	}, nil
}

func generateSegment(seed int64, sampleRate, channels int, duration float64, kind int) audio.Signal {
	frames := int(math.Round(duration * float64(sampleRate)))
	signal := audio.New(sampleRate, channels, frames)
	random := newSplitMix(seed)
	fadeFrames := int(0.015 * float64(sampleRate))
	for frame := 0; frame < frames; frame++ {
		t := float64(frame) / float64(sampleRate)
		duck := 1.0
		if frame < fadeFrames {
			duck = float64(frame) / float64(fadeFrames)
		}
		if remaining := frames - frame; remaining < fadeFrames {
			duck *= float64(remaining) / float64(fadeFrames)
		}
		left, right := synthesize(t, kind, random)
		left *= duck
		right *= duck
		if left > 0.995 {
			left = 0.995
		}
		if left < -0.995 {
			left = -0.995
		}
		if right > 0.995 {
			right = 0.995
		}
		if right < -0.995 {
			right = -0.995
		}
		if channels == 1 {
			signal.Set(frame, 0, (left+right)*0.5)
		} else {
			signal.Set(frame, 0, left)
			signal.Set(frame, 1, right)
		}
	}
	return signal
}

func synthesize(t float64, kind int, random *splitMix) (float64, float64) {
	noise := func() float64 { return random.uniform() * 2 }
	switch kind {
	case 1:
		env := 0.72 + 0.20*math.Sin(2*math.Pi*0.37*t) + 0.08*math.Sin(2*math.Pi*0.91*t)
		left := 0.090*math.Sin(2*math.Pi*440*t) + 0.045*math.Sin(2*math.Pi*880*t) +
			0.024*math.Sin(2*math.Pi*2640*t) + 0.004*noise()
		right := 0.090*math.Sin(2*math.Pi*442*t) + 0.040*math.Sin(2*math.Pi*1320*t) +
			0.024*math.Sin(2*math.Pi*3080*t) + 0.004*noise()
		return left * env, right * env
	case 2:
		syllable := math.Max(0, math.Sin(2*math.Pi*3.1*t)+0.20)
		phrase := 1.0
		if math.Mod(t, 1.35) > 0.92 {
			phrase = 0
		}
		pitch := 135 + 18*math.Sin(2*math.Pi*0.83*t)
		voice := 0.11*math.Sin(2*math.Pi*pitch*t) +
			0.055*math.Sin(2*math.Pi*pitch*2.02*t) +
			0.028*math.Sin(2*math.Pi*pitch*3.07*t)
		value := (voice + 0.018*noise()*syllable) * syllable * phrase
		return value, value
	case 3:
		base := 0.008 * noise()
		left, right := base, 0.008*noise()
		hitSpacing := 0.31
		hitPhase := math.Mod(t, hitSpacing)
		hitIndex := int(t / hitSpacing)
		if hitPhase < 0.18 {
			decay := math.Exp(-hitPhase * 24)
			tone := math.Sin(2 * math.Pi * (85 + 12*float64(hitIndex%3)) * hitPhase)
			impulse := (0.68*tone + 0.22*noise()) * decay
			if hitIndex%2 == 0 {
				left += impulse
			} else {
				right += impulse
			}
		}
		if math.Abs(t-1.47) < 0.002 {
			left += 0.82
			right -= 0.76
		}
		return left, right
	case 4:
		active := math.Mod(t, 0.80) < 0.48
		if !active {
			return 0.0004 * noise(), 0
		}
		gate := math.Min(1, math.Mod(t, 0.80)/0.035)
		gate *= math.Min(1, (0.48-math.Mod(t, 0.80))/0.05)
		value := (0.120*math.Sin(2*math.Pi*220*t) + 0.040*math.Sin(2*math.Pi*660*t)) * gate
		return value, value
	case 5:
		env := 0.55 + 0.35*math.Sin(2*math.Pi*0.19*t+0.4)
		left := 0.055*math.Sin(2*math.Pi*261.63*t) + 0.040*math.Sin(2*math.Pi*329.63*t) +
			0.035*math.Sin(2*math.Pi*392.00*t) + 0.003*noise()
		right := 0.052*math.Sin(2*math.Pi*263.2*t+0.2) + 0.043*math.Sin(2*math.Pi*327.1*t) +
			0.033*math.Sin(2*math.Pi*393.8*t+0.5) + 0.003*noise()
		return left * env, right * env
	default:
		value := 0.0035*math.Sin(2*math.Pi*997*t) + 0.0012*noise()
		return value, value
	}
}

type splitMix struct {
	state uint64
}

func newSplitMix(seed int64) *splitMix {
	return &splitMix{state: uint64(seed)}
}

func (s *splitMix) next() uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (s *splitMix) uniform() float64 {
	return float64(s.next()>>11) / float64(uint64(1)<<53)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cannot encode manifest", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cannot write manifest", err).WithPath(path)
	}
	return nil
}

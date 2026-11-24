package generator

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"spatialworkbench/internal/model"
)

type Options struct {
	Seed    int64
	Samples int
	Prefix  string
}

type sampleSpec struct {
	Name       string
	SampleRate int
	Channels   []string
	IRLength   int
	DryLength  int
}

// Generate builds a fully deterministic manifest. All truth values are written
// during construction; no analyzer output participates in this package.
func Generate(opts Options) model.Manifest {
	if opts.Samples <= 0 {
		opts.Samples = 3
	}
	if opts.Prefix == "" {
		opts.Prefix = "local"
	}
	specs := []sampleSpec{
		{Name: "arena-5.1", SampleRate: 48000, Channels: []string{"FL", "FR", "C", "LFE", "SL", "SR"}, IRLength: 4096, DryLength: 4096},
		{Name: "studio-quad", SampleRate: 44100, Channels: []string{"FL", "FR", "RL", "RR"}, IRLength: 3584, DryLength: 3400},
		{Name: "stage-stereo", SampleRate: 96000, Channels: []string{"L", "R"}, IRLength: 6144, DryLength: 5000},
	}
	samples := make([]model.Sample, 0, opts.Samples)
	for i := 0; i < opts.Samples; i++ {
		spec := specs[i%len(specs)]
		if i >= len(specs) {
			spec.Name = fmt.Sprintf("%s-%d", spec.Name, i/len(specs))
			spec.IRLength += (i / len(specs)) * 256
			spec.DryLength += (i / len(specs)) * 128
		}
		seed := opts.Seed + int64(i+1)*104729
		samples = append(samples, buildSample(opts.Prefix, spec, seed))
	}
	return model.Manifest{
		FormatVersion: model.FormatVersion,
		Generator: model.Metadata{
			Name:    "deterministic-spatial-ir",
			Seed:    opts.Seed,
			Build:   "generator-v1",
			Created: "deterministic-clock",
		},
		Samples: samples,
	}
}

func buildSample(prefix string, spec sampleSpec, seed int64) model.Sample {
	sample := model.Sample{
		ID:         fmt.Sprintf("%s-%s-%d", prefix, spec.Name, seed),
		Seed:       seed,
		SampleRate: spec.SampleRate,
		Layout: model.Layout{
			Name:     spec.Name,
			Channels: append([]string(nil), spec.Channels...),
		},
		IRSamples: spec.IRLength,
		Channels:  make([]model.Channel, 0, len(spec.Channels)),
		Dry: model.Dry{
			Description: "deterministic two-tone dry phrase with exponential articulation",
			Samples:     make([]float64, spec.DryLength),
		},
	}

	for i := range sample.Dry.Samples {
		t := float64(i) / float64(spec.SampleRate)
		env := math.Exp(-3.2 * t)
		sample.Dry.Samples[i] = 0.22 * env * (math.Sin(2*math.Pi*220*t) + 0.45*math.Sin(2*math.Pi*660*t+0.3))
	}

	referenceDelay := directDelay(spec.Channels[0], spec.SampleRate, 0)
	for chIndex, id := range spec.Channels {
		direct := directDelay(id, spec.SampleRate, chIndex)
		directEnergy := 0.5625
		reflections := []model.Reflection{
			{Arrival: direct + 72, Energy: 0.0300},
			{Arrival: direct + 176, Energy: 0.0170},
			{Arrival: direct + 312, Energy: 0.0090},
			{Arrival: direct + 488, Energy: 0.0045},
		}
		ir := make([]float64, spec.IRLength)
		ir[direct] = math.Sqrt(directEnergy)
		reflectionEnergy := 0.0
		rng := newLCG(seed ^ int64((chIndex+1)*0x9e3779b1))
		for rIndex, refl := range reflections {
			if refl.Arrival >= len(ir) {
				continue
			}
			sign := 1.0
			if (rIndex+chIndex)%2 == 1 {
				sign = -1
			}
			ir[refl.Arrival] += sign * math.Sqrt(refl.Energy)
			reflectionEnergy += refl.Energy
		}
		tailStart := direct + 560
		tailEnergy := 0.0
		for n := tailStart; n < len(ir); n++ {
			elapsed := float64(n - tailStart)
			amp := 0.025 * math.Exp(-elapsed/700.0)
			noise := rng.Float64()*2 - 1
			v := amp * (0.25 + 0.75*math.Abs(noise))
			if noise < 0 {
				v = -v
			}
			ir[n] += v
			tailEnergy += v * v
		}
		drr := 10 * math.Log10(directEnergy/math.Max(reflectionEnergy+tailEnergy, 1e-15))
		sample.Channels = append(sample.Channels, model.Channel{
			ID:      id,
			Samples: ir,
			Truth: model.ChannelTruth{
				DirectArrival: direct,
				DirectEnergy:  directEnergy,
				Reflections:   reflections,
				DRRDB:         round(drr, 9),
				ITDSamples:    direct - referenceDelay,
				TailEnergy:    round(tailEnergy, 9),
			},
		})
	}
	return sample
}

func directDelay(channelID string, sampleRate, index int) int {
	base := 64 + index*16
	switch channelID {
	case "C":
		base += 8
	case "LFE":
		base += 24
	case "SL", "RL":
		base += 32
	case "SR", "RR":
		base += 40
	case "R":
		base += 8
	}
	if sampleRate >= 96000 {
		base *= 2
	}
	return base
}

func round(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}

type lcg struct {
	state uint64
}

func newLCG(seed int64) *lcg {
	return &lcg{state: uint64(seed) ^ 0xa0761d6478bd642f}
}

func (l *lcg) Uint64() uint64 {
	l.state = l.state*6364136223846793005 + 1442695040888963407
	x := l.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	return x * 2685821657736338717
}

func (l *lcg) Float64() float64 {
	return float64(l.Uint64()>>11) / float64(uint64(1)<<53)
}

func Marshal(manifest model.Manifest) ([]byte, error) {
	return json.MarshalIndent(manifest, "", "  ")
}

func WriteFile(path string, manifest model.Manifest) error {
	b, err := Marshal(manifest)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

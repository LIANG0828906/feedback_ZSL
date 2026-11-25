package samplegen

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"audiofp"
)

const FixedSeed int64 = 2026112501

type EventSpec struct {
	Category    string
	StartSample int
	EndSample   int
}

func DefaultCategories() []audiofp.Category {
	return []audiofp.Category{
		{Name: "voice", Description: "steady low fundamental speech-like event", Tone: audiofp.ToneParams{FrequencyHz: 196.0, Decay: 0.25, Amplitude: 0.74}},
		{Name: "music", Description: "A4 harmonic musical phrase", Tone: audiofp.ToneParams{FrequencyHz: 440.0, Decay: 0.35, Amplitude: 0.70}},
		{Name: "sfx", Description: "E5 game effect tone", Tone: audiofp.ToneParams{FrequencyHz: 659.25, Decay: 0.90, Amplitude: 0.72}},
		{Name: "ui", Description: "high B5 interface chirp", Tone: audiofp.ToneParams{FrequencyHz: 987.77, Decay: 1.35, Amplitude: 0.68}},
	}
}

func DefaultSpecs() []EventSpec {
	return []EventSpec{
		{Category: "voice", StartSample: 2048, EndSample: 8192},
		// 128-sample offset intentionally exercises frame-boundary reporting.
		{Category: "ui", StartSample: 9344, EndSample: 13440},
		{Category: "music", StartSample: 15360, EndSample: 22528},
		{Category: "sfx", StartSample: 24576, EndSample: 30720},
		{Category: "voice", StartSample: 32768, EndSample: 38912},
		{Category: "music", StartSample: 40960, EndSample: 48128},
	}
}

// GenerateDefault creates the fixed-seed validation material.
func GenerateDefault() *audiofp.Material {
	categories := DefaultCategories()
	specs := DefaultSpecs()
	sampleRate := 16000
	sampleCount := 49152
	rng := rand.New(rand.NewSource(FixedSeed))
	samples := make([]float64, sampleCount)

	events := make([]audiofp.Event, 0, len(specs))
	for i, spec := range specs {
		category, _ := findCategory(categories, spec.Category)
		amplitude := category.Tone.Amplitude * (0.94 + rng.Float64()*0.12)
		tone := category.Tone
		tone.Amplitude = amplitude
		for pos := spec.StartSample; pos < spec.EndSample && pos < sampleCount; pos++ {
			t := float64(pos-spec.StartSample) / float64(sampleRate)
			samples[pos] += audiofp.SynthesizeToneSample(tone, t)
		}
		events = append(events, audiofp.Event{
			Index: i + 1, ID: eventID(i + 1), Category: spec.Category,
			StartSample: spec.StartSample, EndSample: spec.EndSample, Tone: tone,
		})
	}

	// Deterministic low-level bus noise. It is generated after events so the
	// stream is stable even if event amplitudes change.
	for i := range samples {
		samples[i] += rng.NormFloat64() * 0.004
		if samples[i] > 0.98 {
			samples[i] = 0.98
		} else if samples[i] < -0.98 {
			samples[i] = -0.98
		}
	}

	pcm := audiofp.PCMDescription{
		SchemaVersion: "audiofp.pcm.v1", ID: "sample-001", Seed: FixedSeed,
		SampleRate: sampleRate, Channels: 1, SampleFormat: "float64-json",
		SampleCount: sampleCount, Samples: samples,
	}
	pcm.SHA256 = pcm.ContentHash()
	manifest := audiofp.Manifest{
		SchemaVersion: "audiofp.manifest.v1", MaterialID: pcm.ID, Seed: FixedSeed,
		Categories: categories, Events: events,
		Quality: audiofp.QualitySpec{ToleranceSamples: 512},
	}
	return &audiofp.Material{PCM: pcm, Manifest: manifest}
}

// WriteDefaultDataset writes PCM and manifest JSON into dir.
func WriteDefaultDataset(dir string) (*audiofp.Material, string, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", "", err
	}
	material := GenerateDefault()
	pcmPath := filepath.Join(dir, material.PCM.ID+".pcm.json")
	manifestPath := filepath.Join(dir, material.PCM.ID+".manifest.json")

	pcmData, err := json.MarshalIndent(material.PCM, "", "  ")
	if err != nil {
		return nil, "", "", err
	}
	if err := os.WriteFile(pcmPath, pcmData, 0o644); err != nil {
		return nil, "", "", err
	}
	manifestData, err := json.MarshalIndent(material.Manifest, "", "  ")
	if err != nil {
		return nil, "", "", err
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		return nil, "", "", err
	}
	return material, pcmPath, manifestPath, nil
}

func findCategory(categories []audiofp.Category, name string) (audiofp.Category, bool) {
	for _, c := range categories {
		if c.Name == name {
			return c, true
		}
	}
	return audiofp.Category{}, false
}

func eventID(no int) string {
	return fmt.Sprintf("evt-%03d", no)
}

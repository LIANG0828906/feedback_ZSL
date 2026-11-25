package audiofp

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
)

const AlgorithmVersion = "audiofp-v1"

// PCMDescription is the local deterministic mono PCM descriptor. The generated
// fixture embeds float64 samples so examples remain self-contained.
type PCMDescription struct {
	SchemaVersion string    `json:"schema_version"`
	ID            string    `json:"id"`
	Seed          int64     `json:"seed,omitempty"`
	SampleRate    int       `json:"sample_rate"`
	Channels      int       `json:"channels"`
	SampleFormat  string    `json:"sample_format"`
	SampleCount   int       `json:"sample_count"`
	Samples       []float64 `json:"samples"`
	SHA256        string    `json:"sha256,omitempty"`
}

type ToneParams struct {
	FrequencyHz float64 `json:"frequency_hz"`
	Decay       float64 `json:"decay"`
	Amplitude   float64 `json:"amplitude,omitempty"`
}

type Category struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Tone        ToneParams `json:"tone"`
}

type Event struct {
	Index       int        `json:"index,omitempty"`
	ID          string     `json:"id,omitempty"`
	Category    string     `json:"category"`
	StartSample int        `json:"start_sample"`
	EndSample   int        `json:"end_sample"`
	Tone        ToneParams `json:"tone"`
}

type QualitySpec struct {
	ToleranceSamples int `json:"tolerance_samples"`
}

type Manifest struct {
	SchemaVersion string      `json:"schema_version"`
	MaterialID    string      `json:"material_id"`
	Seed          int64       `json:"seed,omitempty"`
	Categories    []Category  `json:"categories"`
	Events        []Event     `json:"events"`
	Quality       QualitySpec `json:"quality"`
}

type Material struct {
	PCM          PCMDescription `json:"pcm"`
	Manifest     Manifest       `json:"manifest"`
	PCMPath      string         `json:"pcm_path,omitempty"`
	ManifestPath string         `json:"manifest_path,omitempty"`
}

// ContentHash is a stable content hash over the audio description. It does not
// depend on JSON map ordering or file formatting.
func (p PCMDescription) ContentHash() string {
	h := sha256.New()
	var buf [8]byte
	putInt64 := func(v int64) {
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		_, _ = h.Write(buf[:])
	}
	putInt64(int64(p.SampleRate))
	putInt64(int64(p.Channels))
	putInt64(int64(len(p.Samples)))
	for _, v := range p.Samples {
		binary.BigEndian.PutUint64(buf[:], math.Float64bits(v))
		_, _ = h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (m *Material) ContentHash() string {
	return m.PCM.ContentHash()
}

func (m *Material) Category(name string) (Category, bool) {
	for _, c := range m.Manifest.Categories {
		if c.Name == name {
			return c, true
		}
	}
	return Category{}, false
}

func (e Event) EventNo() int {
	if e.Index > 0 {
		return e.Index
	}
	return 0
}

package audiofp

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

type pcmDTO struct {
	SchemaVersion *string    `json:"schema_version"`
	ID            *string    `json:"id"`
	Seed          *int64     `json:"seed"`
	SampleRate    *int       `json:"sample_rate"`
	Channels      *int       `json:"channels"`
	SampleFormat  *string    `json:"sample_format"`
	SampleCount   *int       `json:"sample_count"`
	Samples       *[]float64 `json:"samples"`
	SHA256        *string    `json:"sha256"`
}

type toneDTO struct {
	FrequencyHz *float64 `json:"frequency_hz"`
	Decay       *float64 `json:"decay"`
	Amplitude   *float64 `json:"amplitude"`
}

type categoryDTO struct {
	Name        *string  `json:"name"`
	Description *string  `json:"description"`
	Tone        *toneDTO `json:"tone"`
}

type eventDTO struct {
	Index       *int     `json:"index"`
	ID          *string  `json:"id"`
	Category    *string  `json:"category"`
	StartSample *int     `json:"start_sample"`
	EndSample   *int     `json:"end_sample"`
	Tone        *toneDTO `json:"tone"`
}

type qualityDTO struct {
	ToleranceSamples *int `json:"tolerance_samples"`
}

type manifestDTO struct {
	SchemaVersion *string        `json:"schema_version"`
	MaterialID    *string        `json:"material_id"`
	Seed          *int64         `json:"seed"`
	Categories    *[]categoryDTO `json:"categories"`
	Events        *[]eventDTO    `json:"events"`
	Quality       *qualityDTO    `json:"quality"`
}

func readJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return schemaError("", "read JSON %s: %v", filepath.Base(path), err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return schemaError("", "parse JSON %s: %v", filepath.Base(path), err)
	}
	return nil
}

func requireString(p *string, field string) (string, error) {
	if p == nil || *p == "" {
		return "", schemaError(field, "field is required")
	}
	return *p, nil
}

func requireInt(p *int, field string) (int, error) {
	if p == nil {
		return 0, schemaError(field, "field is required")
	}
	return *p, nil
}

func requireTone(d *toneDTO, fieldPrefix string) (ToneParams, error) {
	if d == nil {
		return ToneParams{}, schemaError(fieldPrefix, "tone is required")
	}
	freq, err := requireFloat(d.FrequencyHz, fieldPrefix+".frequency_hz")
	if err != nil {
		return ToneParams{}, err
	}
	decay, err := requireFloat(d.Decay, fieldPrefix+".decay")
	if err != nil {
		return ToneParams{}, err
	}
	amp := 1.0
	if d.Amplitude != nil {
		amp = *d.Amplitude
	}
	return ToneParams{FrequencyHz: freq, Decay: decay, Amplitude: amp}, nil
}

func requireFloat(p *float64, field string) (float64, error) {
	if p == nil {
		return 0, schemaError(field, "field is required")
	}
	return *p, nil
}

// LoadMaterial reads and validates a PCM descriptor and event manifest.
func LoadMaterial(pcmPath, manifestPath string) (*Material, error) {
	var pd pcmDTO
	if err := readJSON(pcmPath, &pd); err != nil {
		return nil, err
	}
	pcm, err := pcmFromDTO(&pd)
	if err != nil {
		return nil, err
	}

	var md manifestDTO
	if err := readJSON(manifestPath, &md); err != nil {
		return nil, err
	}
	manifest, err := manifestFromDTO(&md, pcm.SampleRate)
	if err != nil {
		return nil, err
	}
	if manifest.MaterialID != pcm.ID {
		return nil, schemaError("material_id", "manifest material_id %q does not match PCM id %q", manifest.MaterialID, pcm.ID)
	}

	m := &Material{PCM: pcm, Manifest: manifest, PCMPath: pcmPath, ManifestPath: manifestPath}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if pcm.SHA256 != "" && pcm.SHA256 != pcm.ContentHash() {
		return nil, schemaError("sha256", "PCM content hash mismatch: expected %s, computed %s", pcm.SHA256, pcm.ContentHash())
	}
	return m, nil
}

func pcmFromDTO(d *pcmDTO) (PCMDescription, error) {
	schema, err := requireString(d.SchemaVersion, "schema_version")
	if err != nil {
		return PCMDescription{}, err
	}
	id, err := requireString(d.ID, "id")
	if err != nil {
		return PCMDescription{}, err
	}
	sr, err := requireInt(d.SampleRate, "sample_rate")
	if err != nil {
		return PCMDescription{}, err
	}
	channels, err := requireInt(d.Channels, "channels")
	if err != nil {
		return PCMDescription{}, err
	}
	format, err := requireString(d.SampleFormat, "sample_format")
	if err != nil {
		return PCMDescription{}, err
	}
	count, err := requireInt(d.SampleCount, "sample_count")
	if err != nil {
		return PCMDescription{}, err
	}
	if d.Samples == nil {
		return PCMDescription{}, schemaError("samples", "field is required")
	}

	pcm := PCMDescription{
		SchemaVersion: schema,
		ID:            id,
		SampleRate:    sr,
		Channels:      channels,
		SampleFormat:  format,
		SampleCount:   count,
		Samples:       *d.Samples,
	}
	if d.Seed != nil {
		pcm.Seed = *d.Seed
	}
	if d.SHA256 != nil {
		pcm.SHA256 = *d.SHA256
	}
	return pcm, nil
}

func manifestFromDTO(d *manifestDTO, sampleRate int) (Manifest, error) {
	schema, err := requireString(d.SchemaVersion, "schema_version")
	if err != nil {
		return Manifest{}, err
	}
	materialID, err := requireString(d.MaterialID, "material_id")
	if err != nil {
		return Manifest{}, err
	}
	if d.Categories == nil {
		return Manifest{}, schemaError("categories", "field is required")
	}
	if d.Events == nil {
		return Manifest{}, schemaError("events", "field is required")
	}
	if d.Quality == nil || d.Quality.ToleranceSamples == nil {
		return Manifest{}, schemaError("quality.tolerance_samples", "declared quality tolerance is required")
	}

	m := Manifest{
		SchemaVersion: schema,
		MaterialID:    materialID,
		Categories:    make([]Category, 0, len(*d.Categories)),
		Events:        make([]Event, 0, len(*d.Events)),
		Quality:       QualitySpec{ToleranceSamples: *d.Quality.ToleranceSamples},
	}
	if d.Seed != nil {
		m.Seed = *d.Seed
	}

	for i, cd := range *d.Categories {
		prefix := fmt.Sprintf("categories[%d]", i)
		name, err := requireString(cd.Name, prefix+".name")
		if err != nil {
			return Manifest{}, err
		}
		tone, err := requireTone(cd.Tone, prefix+".tone")
		if err != nil {
			return Manifest{}, err
		}
		desc := ""
		if cd.Description != nil {
			desc = *cd.Description
		}
		m.Categories = append(m.Categories, Category{Name: name, Description: desc, Tone: tone})
	}

	for i, ed := range *d.Events {
		prefix := fmt.Sprintf("events[%d]", i)
		category, err := requireString(ed.Category, prefix+".category")
		if err != nil {
			return Manifest{}, err
		}
		start, err := requireInt(ed.StartSample, prefix+".start_sample")
		if err != nil {
			return Manifest{}, err
		}
		end, err := requireInt(ed.EndSample, prefix+".end_sample")
		if err != nil {
			return Manifest{}, err
		}
		tone, err := requireTone(ed.Tone, prefix+".tone")
		if err != nil {
			return Manifest{}, err
		}
		ev := Event{Category: category, StartSample: start, EndSample: end, Tone: tone}
		if ed.Index != nil {
			ev.Index = *ed.Index
		} else {
			ev.Index = i + 1
		}
		if ed.ID != nil {
			ev.ID = *ed.ID
		} else {
			ev.ID = fmt.Sprintf("event-%03d", ev.Index)
		}
		m.Events = append(m.Events, ev)
	}
	return m, nil
}

// Validate checks ranges, overlaps, enum membership and finite tone parameters.
func (m *Material) Validate() error {
	if m == nil {
		return schemaError("", "material is nil")
	}
	p := &m.PCM
	if p.SampleRate <= 0 {
		return newError(CodeRange, "sample_rate", "sample rate must be positive, got %d", p.SampleRate)
	}
	if p.Channels != 1 {
		return schemaError("channels", "only mono PCM is supported, got %d channels", p.Channels)
	}
	if p.SampleCount < 0 {
		return newError(CodeRange, "sample_count", "sample count must not be negative")
	}
	if p.SampleCount == 0 {
		return newError(CodeRange, "sample_count", "material contains zero samples")
	}
	if len(p.Samples) != p.SampleCount {
		return schemaError("samples", "sample_count=%d but samples length=%d", p.SampleCount, len(p.Samples))
	}
	for i, v := range p.Samples {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return schemaError(fmt.Sprintf("samples[%d]", i), "sample must be finite")
		}
	}

	allowed := make(map[string]Category, len(m.Manifest.Categories))
	for i, c := range m.Manifest.Categories {
		if c.Name == "" {
			return schemaError(fmt.Sprintf("categories[%d].name", i), "category name is required")
		}
		if _, dup := allowed[c.Name]; dup {
			return schemaError(fmt.Sprintf("categories[%d].name", i), "duplicate category %q", c.Name)
		}
		if err := validateTone(c.Tone, fmt.Sprintf("categories[%d].tone", i), p.SampleRate); err != nil {
			return err
		}
		allowed[c.Name] = c
	}
	if len(allowed) == 0 {
		return schemaError("categories", "at least one declared category is required")
	}
	if m.Manifest.Quality.ToleranceSamples < 0 {
		return newError(CodeRange, "quality.tolerance_samples", "tolerance must not be negative")
	}

	type positioned struct {
		ev  Event
		no  int
		idx int
	}
	events := make([]positioned, 0, len(m.Manifest.Events))
	for i, ev := range m.Manifest.Events {
		no := ev.Index
		if no <= 0 {
			no = i + 1
		}
		if ev.StartSample < 0 || ev.StartSample > p.SampleCount {
			return rangeError(fmt.Sprintf("events[%d].start_sample", i), no,
				"start_sample=%d outside [0,%d]", ev.StartSample, p.SampleCount)
		}
		if ev.EndSample < 0 || ev.EndSample > p.SampleCount {
			return rangeError(fmt.Sprintf("events[%d].end_sample", i), no,
				"end_sample=%d outside [0,%d]", ev.EndSample, p.SampleCount)
		}
		if ev.StartSample >= ev.EndSample {
			return rangeError(fmt.Sprintf("events[%d].start_sample", i), no,
				"event has non-positive point count: start=%d end=%d", ev.StartSample, ev.EndSample)
		}
		if _, ok := allowed[ev.Category]; !ok {
			return schemaError(fmt.Sprintf("events[%d].category", i),
				"category %q is not in declared enum %v", ev.Category, categoryNames(allowed))
		}
		if err := validateTone(ev.Tone, fmt.Sprintf("events[%d].tone", i), p.SampleRate); err != nil {
			re, ok := err.(*Error)
			if ok {
				re.EventNo = no
				re.EventIndex = i
			}
			return err
		}
		events = append(events, positioned{ev: ev, no: no, idx: i})
	}

	sort.SliceStable(events, func(i, j int) bool {
		if events[i].ev.StartSample == events[j].ev.StartSample {
			return events[i].ev.EndSample < events[j].ev.EndSample
		}
		return events[i].ev.StartSample < events[j].ev.StartSample
	})
	for i := 1; i < len(events); i++ {
		a := events[i-1]
		b := events[i]
		if a.ev.EndSample > b.ev.StartSample {
			err := rangeError(fmt.Sprintf("events[%d].start_sample", b.idx), b.no,
				"event #%d [%d,%d) overlaps event #%d [%d,%d)",
				a.no, a.ev.StartSample, a.ev.EndSample,
				b.no, b.ev.StartSample, b.ev.EndSample)
			err.Details = map[string]any{
				"event_a": a.no, "event_b": b.no,
				"event_a_range": [2]int{a.ev.StartSample, a.ev.EndSample},
				"event_b_range": [2]int{b.ev.StartSample, b.ev.EndSample},
			}
			return err
		}
	}
	return nil
}

func validateTone(t ToneParams, field string, sampleRate int) error {
	if math.IsNaN(t.FrequencyHz) || math.IsInf(t.FrequencyHz, 0) {
		return schemaError(field+".frequency_hz", "frequency must be finite")
	}
	if math.IsNaN(t.Decay) || math.IsInf(t.Decay, 0) {
		return schemaError(field+".decay", "decay must be finite")
	}
	if t.FrequencyHz <= 0 {
		return newError(CodeRange, field+".frequency_hz", "frequency must be positive, got %v", t.FrequencyHz)
	}
	if sampleRate > 0 && t.FrequencyHz >= float64(sampleRate)/2 {
		return newError(CodeRange, field+".frequency_hz", "frequency %v must be below Nyquist %v", t.FrequencyHz, float64(sampleRate)/2)
	}
	if t.Decay < 0 {
		return newError(CodeRange, field+".decay", "decay must be non-negative, got %v", t.Decay)
	}
	return nil
}

func categoryNames(m map[string]Category) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

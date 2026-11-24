package model

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	MinQualityGear = 0
	MaxQualityGear = 3
)

type rawManifest struct {
	SchemaVersion *string `json:"schema_version"`
	SampleRate    *int    `json:"sample_rate"`
	FrameSize     *int    `json:"frame_size"`
	HopSize       *int    `json:"hop_size"`
	Channels      *int    `json:"channels"`
	FrameCount    *int    `json:"frame_count"`
	QualityGear   *int    `json:"quality_gear"`
	Seed          *int64  `json:"seed"`
	SampleBits    *int    `json:"sample_bits"`
}

func ParseManifest(data []byte) (*Manifest, error) {
	var raw rawManifest
	if err := json.Unmarshal(data, &raw); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return nil, NewError(CodeSchema, typeErr.Field, "invalid JSON type: %v", err)
		}
		return nil, NewError(CodeSchema, "manifest_json", "invalid JSON: %v", err)
	}

	m := Manifest{}
	required := map[string]any{}
	if raw.SchemaVersion == nil {
		required["schema_version"] = nil
	} else {
		m.SchemaVersion = *raw.SchemaVersion
	}
	if raw.SampleRate == nil {
		required["sample_rate"] = nil
	} else {
		m.SampleRate = *raw.SampleRate
	}
	if raw.FrameSize == nil {
		required["frame_size"] = nil
	} else {
		m.FrameSize = *raw.FrameSize
	}
	if raw.HopSize == nil {
		required["hop_size"] = nil
	} else {
		m.HopSize = *raw.HopSize
	}
	if raw.Channels == nil {
		required["channels"] = nil
	} else {
		m.Channels = *raw.Channels
	}
	if raw.FrameCount == nil {
		required["frame_count"] = nil
	} else {
		m.FrameCount = *raw.FrameCount
	}
	if raw.QualityGear == nil {
		required["quality_gear"] = nil
	} else {
		m.QualityGear = *raw.QualityGear
	}
	if len(required) > 0 {
		fields := make([]string, 0, len(required))
		for name := range required {
			fields = append(fields, name)
		}
		return nil, NewError(CodeSchema, firstField(fields), "missing required field(s): %v", fields)
	}
	if raw.Seed != nil {
		m.Seed = *raw.Seed
	}
	if raw.SampleBits != nil {
		m.SampleBits = *raw.SampleBits
	} else {
		m.SampleBits = 16
	}
	if err := m.ValidateSchema(); err != nil {
		return nil, err
	}
	if err := m.ValidateParams(); err != nil {
		return nil, err
	}
	return &m, nil
}

func firstField(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func (m Manifest) ValidateSchema() error {
	if m.SchemaVersion == "" {
		return NewError(CodeSchema, "schema_version", "must not be empty")
	}
	if m.SampleRate <= 0 {
		return NewError(CodeSchema, "sample_rate", "must be positive")
	}
	if m.FrameSize <= 0 {
		return NewError(CodeSchema, "frame_size", "must be positive")
	}
	if m.HopSize <= 0 {
		return NewError(CodeSchema, "hop_size", "must be positive")
	}
	if m.FrameSize%m.HopSize != 0 {
		return NewError(CodeSchema, "frame_size", "frame_size=%d must be divisible by hop_size=%d", m.FrameSize, m.HopSize)
	}
	if m.Channels <= 0 {
		return NewError(CodeSchema, "channels", "must be positive")
	}
	if m.FrameCount <= 0 {
		return NewError(CodeSchema, "frame_count", "must be positive")
	}
	if m.SampleBits != 16 && m.SampleBits != 24 {
		return NewError(CodeSchema, "sample_bits", "only 16 or 24 bit integer samples are supported")
	}
	return nil
}

func (m Manifest) ValidateParams() error {
	if m.QualityGear < MinQualityGear || m.QualityGear > MaxQualityGear {
		return NewError(CodePara, "quality_gear", "must be between %d and %d", MinQualityGear, MaxQualityGear)
	}
	return nil
}

func (p EvalParams) Validate(m Manifest) error {
	full := p.WithDefaults(m)
	total := m.SamplesPerChannel()
	if full.WindowSizeSamples <= 0 || full.WindowSizeSamples > total {
		return NewError(CodePara, "window_size_samples", "must be in [1,%d]", total)
	}
	if full.StepSamples <= 0 || full.StepSamples > total {
		return NewError(CodePara, "step_samples", "must be in [1,%d]", total)
	}
	if full.SSIMWindowSamples <= 0 || full.SSIMWindowSamples > total {
		return NewError(CodePara, "ssim_window_samples", "must be in [1,%d]", total)
	}
	if full.SSIMStepSamples <= 0 || full.SSIMStepSamples > total {
		return NewError(CodePara, "ssim_step_samples", "must be in [1,%d]", total)
	}
	if full.SideBandSamples < 0 {
		return NewError(CodePara, "side_band_samples", "must be non-negative")
	}
	return nil
}

type Sequence struct {
	Manifest      Manifest `json:"manifest"`
	Original      []int32  `json:"original"`
	Reconstructed []int32  `json:"reconstructed"`
	QuantSteps    []int64  `json:"quant_steps,omitempty"`
}

func ValidateSequence(m Manifest, original, reconstructed []int32) error {
	want := m.TotalInterleavedSamples()
	if len(original) != want {
		frame := firstUnavailableFrame(m, len(original))
		err := NewError(CodeDecode, "original_samples", "declared %d interleaved samples, got %d", want, len(original))
		if frame >= 0 {
			WithFrame(err, frame)
		}
		return err
	}
	if len(reconstructed) != want {
		frame := firstUnavailableFrame(m, len(reconstructed))
		err := NewError(CodeDecode, "reconstructed_samples", "declared %d interleaved samples, got %d", want, len(reconstructed))
		if frame >= 0 {
			WithFrame(err, frame)
		}
		return err
	}
	return nil
}

func firstUnavailableFrame(m Manifest, gotInterleaved int) int {
	gotPerChannel := 0
	if m.Channels > 0 {
		gotPerChannel = gotInterleaved / m.Channels
	}
	for f := 0; f < m.FrameCount; f++ {
		end := f*m.HopSize + m.FrameSize
		if gotPerChannel < end {
			return f
		}
	}
	return -1
}

func (s Sequence) Validate() error {
	return ValidateSequence(s.Manifest, s.Original, s.Reconstructed)
}

func MustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("marshal: %v", err))
	}
	return b
}

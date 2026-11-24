// Package capability loads and validates the declared sound-card capability
// manifest.
package capability

import (
	"encoding/json"
	"fmt"
	"os"

	"audioforge/internal/errs"
	"audioforge/internal/model"
)

type rawCapability struct {
	Version          *string              `json:"version"`
	ChannelCount     *int                 `json:"channel_count"`
	SampleRate       *int64               `json:"sample_rate"`
	Channels         *[]model.ChannelSpec `json:"channels"`
	QuantUnit        *int64               `json:"quant_unit"`
	QuantRounding    *model.Rounding      `json:"quant_rounding"`
	MinEventInterval *int64               `json:"min_event_interval"`
	MaxPacketSamples *int64               `json:"max_packet_samples"`
	TimingWindow     *model.TimingWindow  `json:"timing_window"`
}

func Load(path string) (model.Capability, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.Capability{}, errs.Wrap(errs.EVersion, err, "read capability manifest %s", path)
	}
	return Decode(data)
}

func Decode(data []byte) (model.Capability, error) {
	var raw rawCapability
	if err := json.Unmarshal(data, &raw); err != nil {
		return model.Capability{}, errs.BranchField("", "capability", "decode capability manifest: %v", err)
	}
	required := []struct {
		field string
		value any
	}{
		{"version", raw.Version},
		{"channel_count", raw.ChannelCount},
		{"sample_rate", raw.SampleRate},
		{"channels", raw.Channels},
		{"quant_unit", raw.QuantUnit},
		{"quant_rounding", raw.QuantRounding},
		{"min_event_interval", raw.MinEventInterval},
		{"max_packet_samples", raw.MaxPacketSamples},
		{"timing_window", raw.TimingWindow},
	}
	for _, field := range required {
		if field.value == nil {
			return model.Capability{}, errs.BranchField("", "capability."+field.field, "missing required field")
		}
	}
	capability := model.Capability{
		Version:          *raw.Version,
		ChannelCount:     *raw.ChannelCount,
		SampleRate:       *raw.SampleRate,
		Channels:         *raw.Channels,
		QuantUnit:        *raw.QuantUnit,
		QuantRounding:    *raw.QuantRounding,
		MinEventInterval: *raw.MinEventInterval,
		MaxPacketSamples: *raw.MaxPacketSamples,
		TimingWindow:     *raw.TimingWindow,
	}
	if err := capability.Validate(); err != nil {
		return model.Capability{}, errs.BranchField("", "capability", "invalid capability manifest: %v", err)
	}
	model.EnsureTemplateHashes(&capability)
	return capability, nil
}

func Encode(capability model.Capability) ([]byte, error) {
	if err := capability.Validate(); err != nil {
		return nil, fmt.Errorf("invalid capability: %w", err)
	}
	return json.MarshalIndent(capability, "", "  ")
}

package boundary

import (
	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
)

type Boundary struct {
	Channel       string `json:"channel"`
	PacketIndex   int    `json:"packet_index"`
	StartSample   int    `json:"start_sample"`
	EndSample     int    `json:"end_sample"`
	SampleCount   int    `json:"sample_count"`
	TimestampMS   int64  `json:"timestamp_ms"`
	WindowStartMS int64  `json:"window_start_ms"`
	WindowEndMS   int64  `json:"window_end_ms"`
}

func PlanChannel(channel string, length int, capability model.Capability) []Boundary {
	maxSamples := capability.MaxSamplesPerPacket
	if maxSamples <= 0 {
		maxSamples = 8
	}
	var out []Boundary
	for start, index := 0, 0; start < length; start, index = start+maxSamples, index+1 {
		end := start + maxSamples
		if end > length {
			end = length
		}
		b := Boundary{
			Channel: channel, PacketIndex: index, StartSample: start, EndSample: end,
			SampleCount: end - start, TimestampMS: int64(start) * int64(capability.SamplePeriodMS),
			WindowStartMS: int64(start) * int64(capability.SamplePeriodMS),
			WindowEndMS:   int64(start+maxSamples) * int64(capability.SamplePeriodMS),
		}
		if capability.PacketWindowMS > 0 {
			b.WindowEndMS = b.WindowStartMS + int64(capability.PacketWindowMS)
		}
		out = append(out, b)
	}
	return out
}

func Validate(b Boundary, capability model.Capability, seq int) error {
	if b.SampleCount <= 0 || b.SampleCount > capability.MaxSamplesPerPacket {
		e := errs.New(errs.EState, "packet sample count exceeds capability")
		e.Seq = seq
		e.Channel = b.Channel
		e.Details = map[string]any{"sample_count": b.SampleCount, "max": capability.MaxSamplesPerPacket}
		return e
	}
	if b.TimestampMS < b.WindowStartMS || b.TimestampMS >= b.WindowEndMS {
		e := errs.New(errs.EState, "packet timing marker is outside timing window")
		e.Seq = seq
		e.Channel = b.Channel
		e.Details = map[string]any{"timestamp_ms": b.TimestampMS, "window_start_ms": b.WindowStartMS, "window_end_ms": b.WindowEndMS}
		return e
	}
	return nil
}

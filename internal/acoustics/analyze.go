package acoustics

import (
	"math"

	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

func FrameSpan(sample model.Sample, params model.Params, frame int) (start, end int, err *werr.Error) {
	total := params.TotalFrames(len(sample.Dry.Samples))
	if frame < 0 || frame >= total {
		return 0, 0, werr.Frame(werr.CodeFrame, frame, "", "frame is outside [0,%d)", total)
	}
	start = frame * params.Hop
	end = start + params.FrameLength
	if end > sample.IRSamples {
		end = sample.IRSamples
	}
	if start >= end || end-start < params.QuantStep {
		e := werr.Frame(werr.CodeFrame, frame, "", "analysis span [%d,%d) is shorter than quantStep %d", start, end, params.QuantStep)
		e.Field = "frameLength"
		return 0, 0, e
	}
	return start, end, nil
}

// EstimateFrame is a pure function of sample content, parameters, and frame
// index. Channel order is the layout order, so completion order cannot affect
// the result.
func EstimateFrame(sample model.Sample, params model.Params, frame int) (FrameEstimate, *werr.Error) {
	start, end, err := FrameSpan(sample, params, frame)
	if err != nil {
		return FrameEstimate{}, err
	}
	result := FrameEstimate{Frame: frame, Start: start, End: end, Channels: make([]ChannelEstimate, 0, len(sample.Channels))}
	for _, ch := range sample.Channels {
		if len(ch.Samples) != sample.IRSamples {
			e := werr.Frame(werr.CodeDecode, frame, ch.ID, "channel sample count %d differs from manifest irSamples %d", len(ch.Samples), sample.IRSamples)
			e.Field = "channels.samples"
			return FrameEstimate{}, e
		}
		direct, peak, ok := quantizedPeak(ch.Samples, start, end, params.QuantStep)
		if !ok {
			e := werr.Frame(werr.CodeFrame, frame, ch.ID, "no quantized direct-path candidate in [%d,%d) with step %d", start, end, params.QuantStep)
			e.Field = "quantStep"
			return FrameEstimate{}, e
		}
		energyStart := direct + 1
		energyEnd := energyStart + params.WindowLength
		if energyEnd > end {
			energyEnd = end
		}
		if energyStart > end {
			energyStart = end
		}
		early := 0.0
		for n := energyStart; n < energyEnd; n++ {
			early += ch.Samples[n] * ch.Samples[n]
		}
		result.Channels = append(result.Channels, ChannelEstimate{
			ID:              ch.ID,
			DirectArrival:   direct,
			DirectOffset:    direct - start,
			DirectEnergy:    peak * peak,
			EarlyEnergy:     early,
			IntegratedWidth: maxInt(0, energyEnd-energyStart),
			PeakAmplitude:   peak,
		})
	}
	return result, nil
}

func quantizedPeak(samples []float64, start, end, step int) (position int, peak float64, ok bool) {
	if step <= 0 || start >= end || len(samples) == 0 {
		return 0, 0, false
	}
	first := start
	remainder := first % step
	if remainder != 0 {
		first += step - remainder
	}
	best := 0.0
	bestPos := -1
	for n := first; n < end && n < len(samples); n += step {
		v := math.Abs(samples[n])
		if bestPos < 0 || v > best {
			best = v
			bestPos = n
		}
	}
	if bestPos < 0 {
		return 0, 0, false
	}
	return bestPos, best, true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

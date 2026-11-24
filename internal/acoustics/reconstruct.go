package acoustics

import (
	"math"

	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

func Reconstruct(sample model.Sample, params model.Params, estimate FrameEstimate, previous ReconstructionState) (FrameReconstruction, *werr.Error) {
	if len(estimate.Channels) == 0 {
		return FrameReconstruction{}, werr.Frame(werr.CodeFrame, estimate.Frame, "", "cannot reconstruct an empty frame")
	}
	result := FrameReconstruction{
		Frame:         estimate.Frame,
		Start:         estimate.Start,
		End:           estimate.End,
		PreviousState: previous,
		Channels:      make([]ChannelReconstruction, 0, len(estimate.Channels)),
	}
	reference := estimate.Channels[0].DirectArrival
	maxITD := float64(params.WindowLength)
	for _, ch := range estimate.Channels {
		early := ch.EarlyEnergy
		if previous.Valid && params.EarlyEnergySmoothingWeight > 0 {
			w := params.EarlyEnergySmoothingWeight
			early = w*previous.SmoothedEarlyEnergy + (1-w)*early
		}
		drr := 10 * math.Log10(math.Max(ch.DirectEnergy, 1e-15)/math.Max(early, 1e-15))
		itd := ch.DirectArrival - reference
		degree := 0.0
		if maxITD > 0 {
			ratio := math.Max(-1, math.Min(1, float64(itd)/maxITD))
			degree = math.Asin(ratio) * 180 / math.Pi
		}
		result.Channels = append(result.Channels, ChannelReconstruction{
			ID:              ch.ID,
			DirectDelay:     ch.DirectArrival,
			DirectOffset:    ch.DirectOffset,
			DirectEnergy:    ch.DirectEnergy,
			RawEarlyEnergy:  ch.EarlyEnergy,
			EarlyEnergy:     early,
			DRRDB:           drr,
			ITDSamples:      itd,
			DirectionDegree: degree,
		})
	}
	integratedWidth := 0
	for _, ch := range estimate.Channels {
		if ch.IntegratedWidth > integratedWidth {
			integratedWidth = ch.IntegratedWidth
		}
	}
	bounds, err := Bounds(params, sample.SampleRate, integratedWidth, estimate.Frame, previous)
	if err != nil {
		return FrameReconstruction{}, err
	}
	result.Bounds = bounds
	state := ReconstructionState{
		Frame:                  estimate.Frame,
		SmoothedEarlyEnergy:    averageEarly(result.Channels),
		CumulativeDirect:       previous.CumulativeDirect + sumDirect(result.Channels),
		CumulativeEarly:        previous.CumulativeEarly + sumEarly(result.Channels),
		QuantizationCumulative: bounds.Quantization.Cumulative,
		EnergyCumulative:       bounds.EnergyTruncation.Cumulative,
		ITDCumulative:          bounds.ITD.Cumulative,
		ConvCumulative:         bounds.ConvolutionInterp.Cumulative,
		Valid:                  true,
	}
	result.State = state
	return result, nil
}

func averageEarly(channels []ChannelReconstruction) float64 {
	if len(channels) == 0 {
		return 0
	}
	total := 0.0
	for _, ch := range channels {
		total += ch.EarlyEnergy
	}
	return total / float64(len(channels))
}

func sumDirect(channels []ChannelReconstruction) float64 {
	total := 0.0
	for _, ch := range channels {
		total += ch.DirectEnergy
	}
	return total
}

func sumEarly(channels []ChannelReconstruction) float64 {
	total := 0.0
	for _, ch := range channels {
		total += ch.EarlyEnergy
	}
	return total
}

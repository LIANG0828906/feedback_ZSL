package acoustics

import (
	"math"

	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

// Bounds derives its values only from observed parameters and the actual
// in-frame integration width. Truth fields and rendered samples are never
// consulted.
func Bounds(params model.Params, sampleRate, integratedWidth, frame int, previous ReconstructionState) (ErrorBounds, *werr.Error) {
	quantCurrent := float64(params.QuantStep) / 2
	quantCumulative := previous.QuantizationCumulative + quantCurrent
	energyCurrent := 0.0
	if params.WindowLength > 0 && integratedWidth < params.WindowLength {
		energyCurrent = float64(params.WindowLength-integratedWidth) / float64(params.WindowLength)
	}
	energyCumulative := previous.EnergyCumulative + energyCurrent
	itdCurrent := float64(params.QuantStep)
	itdCumulative := previous.ITDCumulative + itdCurrent
	convCurrent := math.Pow(0.5, float64(params.InterpOrder+1))
	convCumulative := previous.ConvCumulative + convCurrent
	bounds := ErrorBounds{
		Quantization:      BoundValue{Current: quantCurrent, Cumulative: quantCumulative},
		EnergyTruncation:  BoundValue{Current: energyCurrent, Cumulative: energyCumulative},
		ITD:               BoundValue{Current: itdCurrent, Cumulative: itdCumulative},
		ConvolutionInterp: BoundValue{Current: convCurrent, Cumulative: convCumulative},
		Derivation: Derivation{
			FrameLength:      params.FrameLength,
			Hop:              params.Hop,
			WindowLength:     params.WindowLength,
			IntegratedWidth:  integratedWidth,
			InterpOrder:      params.InterpOrder,
			SampleRate:       sampleRate,
			QuantStepSamples: params.QuantStep,
			QuantStepSeconds: float64(params.QuantStep) / float64(sampleRate),
		},
	}
	checks := []struct {
		item       string
		current    float64
		cumulative float64
	}{
		{ErrorQuantization, quantCurrent, quantCumulative},
		{ErrorEnergyTruncation, energyCurrent, energyCumulative},
		{ErrorITD, itdCurrent, itdCumulative},
		{ErrorConvolutionInterp, convCurrent, convCumulative},
	}
	for _, check := range checks {
		if check.current > params.ErrorTolerance || check.cumulative > params.ErrorTolerance {
			e := werr.Frame(werr.CodeErr, frame, "", "%s current/cumulative upper bound %.12g/%.12g exceeds tolerance %.12g", check.item, check.current, check.cumulative, params.ErrorTolerance)
			e.Item = check.item
			e.Actual = check.cumulative
			e.Expected = params.ErrorTolerance
			e.Field = "errorTolerance"
			return ErrorBounds{}, e
		}
	}
	return bounds, nil
}

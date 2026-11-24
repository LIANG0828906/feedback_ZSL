package acoustics

import "spatialworkbench/internal/model"

const (
	ErrorQuantization      = "direct_peak_quantization"
	ErrorEnergyTruncation  = "early_energy_integration_truncation"
	ErrorITD               = "inter_channel_time_difference"
	ErrorConvolutionInterp = "convolution_interpolation"
)

type ChannelEstimate struct {
	ID              string  `json:"id"`
	DirectArrival   int     `json:"directArrival"`
	DirectOffset    int     `json:"directOffset"`
	DirectEnergy    float64 `json:"directEnergy"`
	EarlyEnergy     float64 `json:"earlyEnergy"`
	IntegratedWidth int     `json:"integratedWidth"`
	PeakAmplitude   float64 `json:"peakAmplitude"`
}

type FrameEstimate struct {
	Frame    int               `json:"frame"`
	Start    int               `json:"start"`
	End      int               `json:"end"`
	Channels []ChannelEstimate `json:"channels"`
}

type ChannelReconstruction struct {
	ID              string  `json:"id"`
	DirectDelay     int     `json:"directDelay"`
	DirectOffset    int     `json:"directOffset"`
	DirectEnergy    float64 `json:"directEnergy"`
	RawEarlyEnergy  float64 `json:"rawEarlyEnergy"`
	EarlyEnergy     float64 `json:"earlyEnergy"`
	DRRDB           float64 `json:"drrDb"`
	ITDSamples      int     `json:"itdSamples"`
	DirectionDegree float64 `json:"directionDegree"`
}

type ReconstructionState struct {
	Frame                  int     `json:"frame"`
	SmoothedEarlyEnergy    float64 `json:"smoothedEarlyEnergy"`
	CumulativeDirect       float64 `json:"cumulativeDirectEnergy"`
	CumulativeEarly        float64 `json:"cumulativeEarlyEnergy"`
	QuantizationCumulative float64 `json:"quantizationCumulative"`
	EnergyCumulative       float64 `json:"energyCumulative"`
	ITDCumulative          float64 `json:"itdCumulative"`
	ConvCumulative         float64 `json:"convolutionCumulative"`
	Valid                  bool    `json:"valid"`
}

type FrameReconstruction struct {
	Frame         int                     `json:"frame"`
	Start         int                     `json:"start"`
	End           int                     `json:"end"`
	Channels      []ChannelReconstruction `json:"channels"`
	State         ReconstructionState     `json:"state"`
	PreviousState ReconstructionState     `json:"previousState"`
	Bounds        ErrorBounds             `json:"bounds"`
}

type BoundValue struct {
	Current    float64 `json:"current"`
	Cumulative float64 `json:"cumulative"`
}

type ErrorBounds struct {
	Quantization      BoundValue `json:"directPeakQuantization"`
	EnergyTruncation  BoundValue `json:"earlyEnergyTruncation"`
	ITD               BoundValue `json:"interChannelTimeDifference"`
	ConvolutionInterp BoundValue `json:"convolutionInterpolation"`
	Derivation        Derivation `json:"derivation"`
}

type Derivation struct {
	FrameLength      int     `json:"frameLength"`
	Hop              int     `json:"hop"`
	WindowLength     int     `json:"windowLength"`
	IntegratedWidth  int     `json:"integratedWidth"`
	InterpOrder      int     `json:"interpOrder"`
	SampleRate       int     `json:"sampleRate"`
	QuantStepSamples int     `json:"quantStepSamples"`
	QuantStepSeconds float64 `json:"quantStepSeconds"`
}

type FrameResult struct {
	Frame          int                 `json:"frame"`
	Params         model.Params        `json:"params"`
	Estimate       FrameEstimate       `json:"estimate"`
	Reconstruction FrameReconstruction `json:"reconstruction"`
	Render         [][]float64         `json:"render"`
	InputHash      string              `json:"inputHash"`
	ParamsHash     string              `json:"paramsHash"`
	Algorithm      string              `json:"algorithmVersion"`
}

func (f FrameResult) Clone() FrameResult {
	out := f
	out.Params = f.Params.Clone()
	out.Estimate.Channels = append([]ChannelEstimate(nil), f.Estimate.Channels...)
	out.Reconstruction.Channels = append([]ChannelReconstruction(nil), f.Reconstruction.Channels...)
	out.Render = make([][]float64, len(f.Render))
	for i := range f.Render {
		out.Render[i] = append([]float64(nil), f.Render[i]...)
	}
	return out
}

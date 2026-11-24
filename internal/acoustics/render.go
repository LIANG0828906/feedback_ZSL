package acoustics

import (
	"math"

	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

// RenderFrame convolves the dry chunk assigned to a frame with an IR rebuilt
// from that frame's reconstructed parameters. The output is exactly as long as
// the dry chunk: the causal tail outside the chunk is not zero-padded back into
// the stream.
func RenderFrame(sample model.Sample, params model.Params, estimate FrameEstimate, recon FrameReconstruction) ([][]float64, *werr.Error) {
	dryStart := estimate.Frame * params.Hop
	dryEnd := dryStart + params.Hop
	if dryEnd > len(sample.Dry.Samples) {
		dryEnd = len(sample.Dry.Samples)
	}
	if dryStart < 0 || dryStart >= dryEnd {
		e := werr.Frame(werr.CodeFrame, estimate.Frame, "", "dry chunk [%d,%d) is empty", dryStart, dryEnd)
		e.Field = "dry.samples"
		return nil, e
	}
	dry := sample.Dry.Samples[dryStart:dryEnd]
	outChannels := len(sample.Layout.Channels)
	output := make([][]float64, outChannels)
	for i := range output {
		output[i] = make([]float64, len(dry))
	}
	reconByID := make(map[string]ChannelReconstruction, len(recon.Channels))
	for _, ch := range recon.Channels {
		reconByID[ch.ID] = ch
	}
	for _, channel := range sample.Channels {
		chRecon, ok := reconByID[channel.ID]
		if !ok {
			e := werr.Frame(werr.CodeFrame, estimate.Frame, channel.ID, "reconstruction is missing channel")
			e.Field = "reconstruction.channels"
			return nil, e
		}
		target, ok := params.Routing[channel.ID]
		if !ok || target < 0 || target >= outChannels {
			e := werr.New(werr.CodeLayout, "routing target for channel %q is outside [0,%d)", channel.ID, outChannels)
			e.Field = "routing"
			e.Frame = estimate.Frame
			e.Channel = channel.ID
			e.Actual = target
			return nil, e
		}
		gain := params.RenderGain
		if channelGain, exists := params.ChannelGain[channel.ID]; exists {
			gain *= channelGain
		}
		if math.IsNaN(gain) || math.IsInf(gain, 0) || gain < 0 || gain > 1 {
			e := werr.Frame(werr.CodeClip, estimate.Frame, channel.ID, "effective gain %.12g is outside [0,1]", gain)
			e.Field = "channelGain"
			e.Actual = gain
			return nil, e
		}
		directAmplitude := math.Sqrt(math.Max(chRecon.DirectEnergy, 0)) * gain
		if directAmplitude > 1+1e-12 {
			e := werr.Frame(werr.CodeClip, estimate.Frame, channel.ID, "reconstructed direct amplitude %.12g exceeds unity before convolution", directAmplitude)
			e.Field = "directEnergy"
			e.Actual = directAmplitude
			return nil, e
		}
		ir, err := buildIR(params, estimate, chRecon)
		if err != nil {
			err.Frame = estimate.Frame
			err.Channel = channel.ID
			return nil, err
		}
		dst := output[target]
		for n := 0; n < len(dry); n++ {
			acc := 0.0
			limit := n
			if limit >= len(ir) {
				limit = len(ir) - 1
			}
			for k := 0; k <= limit; k++ {
				acc += dry[n-k] * ir[k] * gain
			}
			if math.IsNaN(acc) || math.IsInf(acc, 0) {
				e := werr.Frame(werr.CodeFrame, estimate.Frame, channel.ID, "convolution produced non-finite output at sample %d", n)
				e.Field = "render"
				return nil, e
			}
			if math.Abs(acc) > 1+1e-12 {
				e := werr.Frame(werr.CodeClip, estimate.Frame, channel.ID, "convolution output sample %d has amplitude %.12g outside [-1,1]", n, acc)
				e.Field = "render"
				e.Actual = acc
				return nil, e
			}
			dst[n] = acc
		}
	}
	return output, nil
}

func buildIR(params model.Params, estimate FrameEstimate, ch ChannelReconstruction) ([]float64, *werr.Error) {
	length := estimate.End - estimate.Start
	if length <= 0 {
		return nil, werr.New(werr.CodeFrame, "cannot build IR for an empty frame")
	}
	ir := make([]float64, length)
	localDirect := ch.DirectDelay - estimate.Start
	if localDirect < 0 || localDirect >= length {
		return nil, werr.New(werr.CodeFrame, "reconstructed direct offset %d is outside frame length %d", localDirect, length)
	}
	directAmp := math.Sqrt(math.Max(ch.DirectEnergy, 0))
	ir[localDirect] += directAmp
	reflectionAmp := math.Sqrt(math.Max(ch.EarlyEnergy, 0) * 0.5)
	if reflectionAmp > 0 {
		frac := 0.5
		addFractionalTap(ir, float64(localDirect)+float64(params.WindowLength)*0.35+frac, reflectionAmp*0.7745966692, params.InterpOrder)
		addFractionalTap(ir, float64(localDirect)+float64(params.WindowLength)*0.70+frac, reflectionAmp*0.4472135955, params.InterpOrder)
	}
	totalReverb := ch.DirectEnergy / math.Max(math.Pow(10, ch.DRRDB/10), 1e-15)
	tailEnergy := math.Max(0, totalReverb-ch.EarlyEnergy)
	if tailEnergy > 0 {
		tailAmp := math.Sqrt(tailEnergy)
		tailStart := localDirect + params.WindowLength
		if tailStart < length {
			denom := math.Max(1, float64(length-tailStart))
			for n := tailStart; n < length; n++ {
				progress := float64(n-tailStart) / denom
				ir[n] += tailAmp * 0.06 * math.Exp(-3.5*progress)
			}
		}
	}
	return ir, nil
}

func addFractionalTap(ir []float64, position, amplitude float64, order int) {
	if len(ir) == 0 || amplitude == 0 {
		return
	}
	base := int(math.Floor(position))
	frac := position - float64(base)
	if frac < 1e-12 {
		if base >= 0 && base < len(ir) {
			ir[base] += amplitude
		}
		return
	}
	if order < 1 {
		order = 1
	}
	if order > 5 {
		order = 5
	}
	start := base - order/2
	weights := lagrangeWeights(order+1, frac)
	for j, w := range weights {
		idx := start + j
		if idx >= 0 && idx < len(ir) {
			ir[idx] += amplitude * w
		}
	}
}

func lagrangeWeights(count int, x float64) []float64 {
	weights := make([]float64, count)
	for i := 0; i < count; i++ {
		w := 1.0
		for j := 0; j < count; j++ {
			if i == j {
				continue
			}
			w *= (x - float64(j)) / float64(i-j)
		}
		weights[i] = w
	}
	return weights
}

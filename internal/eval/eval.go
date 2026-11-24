package eval

import (
	"math"

	"audiocheck/internal/bound"
	"audiocheck/internal/model"
)

type RangeBuffer struct {
	Origin        int
	End           int
	Original      [][]int32
	Reconstructed [][]int32
}

type Result struct {
	Frames  []model.FrameReport
	Windows []model.WindowMetric
}

func TruncationLog() []model.TruncationRecord {
	return []model.TruncationRecord{
		{Field: "sum_x,sum_y,sum_xx,sum_yy,sum_xy", Stage: "ssim_window_accumulation", IntegerAccum: true, Description: "All window sufficient statistics are exact signed 64-bit integer accumulators."},
		{Field: "signal_energy,error_energy", Stage: "frame_snr_psnr_accumulation", IntegerAccum: true, Description: "Sample products and squared errors use exact int64 accumulation for supported local frame sizes."},
		{Field: "ssim_db/noise_std/snr_db_fixed", Stage: "final_round", Scale: model.MetricScale, MaxAbsError: 0.5 / float64(model.MetricScale), Description: "Floating point normalization is rounded to six decimal fixed-point units; halfway cases use math.Round."},
		{Field: "transform_coefficients", Stage: "encoder_dct_fixed_conversion", FractionBits: model.CoefficientFracBits, MaxAbsError: 0.5 / float64(int64(1)<<model.CoefficientFracBits), Description: "DCT coefficients are converted to Q24 fixed point before quantization."},
	}
}

func ToPlanar(samples []int32, m model.Manifest) [][]int32 {
	total := m.SamplesPerChannel()
	out := make([][]int32, m.Channels)
	for ch := 0; ch < m.Channels; ch++ {
		out[ch] = make([]int32, total)
	}
	for t := 0; t < total; t++ {
		for ch := 0; ch < m.Channels; ch++ {
			out[ch][t] = samples[t*m.Channels+ch]
		}
	}
	return out
}

func SliceRange(original, reconstructed []int32, m model.Manifest, origin, end int) (*RangeBuffer, error) {
	total := m.SamplesPerChannel()
	if origin < 0 || end > total || origin > end {
		return nil, model.NewError(model.CodeBand, "sample_range", "invalid range [%d,%d) for total %d", origin, end, total)
	}
	length := end - origin
	buf := &RangeBuffer{Origin: origin, End: end, Original: make([][]int32, m.Channels), Reconstructed: make([][]int32, m.Channels)}
	for ch := 0; ch < m.Channels; ch++ {
		buf.Original[ch] = make([]int32, length)
		buf.Reconstructed[ch] = make([]int32, length)
		for local := 0; local < length; local++ {
			globalTime := origin + local
			interleavedIndex := globalTime*m.Channels + ch
			buf.Original[ch][local] = original[interleavedIndex]
			buf.Reconstructed[ch][local] = reconstructed[interleavedIndex]
		}
	}
	return buf, nil
}

func GridStarts(p model.EvalParams, total int) []int {
	w := p.WindowSizeSamples
	s := p.StepSamples
	var starts []int
	for start := 0; start+w <= total; start += s {
		starts = append(starts, start)
	}
	if len(starts) == 0 && w <= total {
		starts = []int{0}
	}
	return starts
}

func SelectWindows(m model.Manifest, p model.EvalParams, starts []int, frame int) []int {
	w := p.WindowSizeSamples
	a := frame * m.HopSize
	b := a + m.FrameSize
	var selected []int
	for i, start := range starts {
		center := start + w/2
		if center >= a && center < b {
			selected = append(selected, i)
		}
	}
	if len(selected) > 0 {
		return selected
	}

	target := a + m.FrameSize/2
	best := -1
	bestDist := math.MaxInt
	for i, start := range starts {
		center := start + w/2
		dist := center - target
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist || (dist == bestDist && (best == -1 || i < best)) {
			bestDist = dist
			best = i
		}
	}
	if best >= 0 {
		return []int{best}
	}
	return nil
}

type BlockPlan struct {
	Coord        model.BlockCoord
	WindowStarts []int
	Selected     map[int][]int
	Origin       int
	End          int
}

func PlanBlock(m model.Manifest, p model.EvalParams, chunkIndex, chunkFrames int) (*BlockPlan, error) {
	if chunkFrames <= 0 {
		return nil, model.NewError(model.CodePara, "chunk_frames", "must be positive")
	}
	f0 := chunkIndex * chunkFrames
	if f0 >= m.FrameCount {
		return nil, model.NewError(model.CodePara, "chunk_index", "block starts at %d beyond frame_count %d", f0, m.FrameCount)
	}
	f1 := minInt(f0+chunkFrames-1, m.FrameCount-1)
	pp := p.WithDefaults(m)
	total := m.SamplesPerChannel()
	starts := GridStarts(pp, total)
	selected := make(map[int][]int)
	minStart := math.MaxInt
	maxEnd := 0
	for f := f0; f <= f1; f++ {
		idxs := SelectWindows(m, pp, starts, f)
		selected[f] = idxs
		for _, idx := range idxs {
			start := starts[idx]
			end := start + pp.WindowSizeSamples
			if start < minStart {
				minStart = start
			}
			if end > maxEnd {
				maxEnd = end
			}
		}
	}
	coreStart := f0 * m.HopSize
	coreEnd := f1*m.HopSize + m.FrameSize
	leftRequired := maxInt(0, coreStart-minStart)
	rightRequired := maxInt(0, maxEnd-coreEnd)
	minBand := maxInt(leftRequired, rightRequired)
	coord := model.BlockCoord{
		Index: chunkIndex, FirstFrame: f0, LastFrame: f1,
		CoreStartSample: coreStart, CoreEndSample: coreEnd,
		LeftRequired: leftRequired, RightRequired: rightRequired,
		MinBandSamples: minBand,
	}
	if pp.SideBandSamples > 0 && pp.SideBandSamples < minBand {
		coord.BandSamples = pp.SideBandSamples
		err := model.NewError(model.CodeBand, "side_band_samples",
			"block %d frames [%d,%d] requires %d side-band samples, configured %d",
			chunkIndex, f0, f1, minBand, pp.SideBandSamples)
		model.WithBlock(err, coord)
		return nil, err
	}
	band := pp.SideBandSamples
	if band <= 0 {
		band = minBand
	}
	origin := maxInt(0, coreStart-band)
	end := minInt(total, coreEnd+band)
	coord.BandSamples = band
	coord.BufferOriginSample = origin
	coord.BufferEndSample = end
	coord.BufferSamples = 2 * (end - origin) * m.Channels
	return &BlockPlan{Coord: coord, WindowStarts: starts, Selected: selected, Origin: origin, End: end}, nil
}

func EvaluateFull(m model.Manifest, original, reconstructed []int32, p model.EvalParams, quant []model.FrameQuantRecord) (*Result, error) {
	pp := p.WithDefaults(m)
	if err := pp.Validate(m); err != nil {
		return nil, err
	}
	if err := model.ValidateSequence(m, original, reconstructed); err != nil {
		return nil, err
	}
	total := m.SamplesPerChannel()
	buf, err := SliceRange(original, reconstructed, m, 0, total)
	if err != nil {
		return nil, err
	}
	return EvaluateRange(m, pp, buf, 0, m.FrameCount-1, quant)
}

func EvaluateRange(m model.Manifest, p model.EvalParams, buf *RangeBuffer, firstFrame, lastFrame int, quant []model.FrameQuantRecord) (*Result, error) {
	pp := p.WithDefaults(m)
	if err := pp.Validate(m); err != nil {
		return nil, err
	}
	starts := GridStarts(pp, m.SamplesPerChannel())
	out := &Result{}
	for f := firstFrame; f <= lastFrame; f++ {
		idxs, ok := selectedForRange(m, pp, starts, f, buf)
		if !ok {
			idxs = SelectWindows(m, pp, starts, f)
		}
		frame, windows, err := evaluateOneFrame(m, pp, buf, f, idxs, starts, quant)
		if err != nil {
			return nil, err
		}
		out.Frames = append(out.Frames, frame)
		out.Windows = append(out.Windows, windows...)
	}
	return out, nil
}

func selectedForRange(m model.Manifest, p model.EvalParams, starts []int, frame int, buf *RangeBuffer) ([]int, bool) {
	idxs := SelectWindows(m, p, starts, frame)
	for _, idx := range idxs {
		start := starts[idx]
		end := start + p.WindowSizeSamples
		if start < buf.Origin || end > buf.End {
			return idxs, false
		}
	}
	return idxs, true
}

func evaluateOneFrame(m model.Manifest, p model.EvalParams, buf *RangeBuffer, f int, windowIdxs []int, starts []int, quant []model.FrameQuantRecord) (model.FrameReport, []model.WindowMetric, error) {
	a := f * m.HopSize
	b := a + m.FrameSize
	if a < buf.Origin || b > buf.End {
		coord := model.BlockCoord{FirstFrame: f, LastFrame: f, CoreStartSample: a, CoreEndSample: b, MinBandSamples: maxInt(buf.Origin-a, b-buf.End)}
		err := model.NewError(model.CodeBand, "sample_buffer", "frame %d is outside sample buffer [%d,%d)", f, buf.Origin, buf.End)
		model.WithFrame(err, f)
		model.WithBlock(err, coord)
		return model.FrameReport{}, nil, err
	}

	var signalEnergy, errorEnergy int64
	frameResidual := residualStats{}
	heatSums := make([]int64, 32)
	heatCounts := make([]int64, 32)
	for ch := 0; ch < m.Channels; ch++ {
		for n := 0; n < m.FrameSize; n++ {
			t := a + n
			x := int64(buf.Original[ch][t-buf.Origin])
			y := int64(buf.Reconstructed[ch][t-buf.Origin])
			e := y - x
			signalEnergy += x * x
			errorEnergy += e * e
			frameResidual.add(e)
			bin := n * len(heatSums) / m.FrameSize
			if bin >= len(heatSums) {
				bin = len(heatSums) - 1
			}
			absE := e
			if absE < 0 {
				absE = -absE
			}
			heatSums[bin] += absE
			heatCounts[bin]++
		}
	}

	peak := m.PeakAmplitude()
	psnrFixed, psnrInf := fixedPSNR(peak, m.FrameSize*m.Channels, errorEnergy, model.MetricScale)
	snrFixed, snrInf := fixedDBFromEnergy(signalEnergy, errorEnergy, model.MetricScale)

	var ssimSum int64
	var ssimCount int64
	windowResidual := residualStats{}
	windows := make([]model.WindowMetric, 0, len(windowIdxs))
	for _, idx := range windowIdxs {
		start := starts[idx]
		end := start + p.WindowSizeSamples
		if start < buf.Origin || end > buf.End {
			minBand := maxInt(buf.Origin-start, end-buf.End)
			coord := model.BlockCoord{FirstFrame: f, LastFrame: f, MinBandSamples: minBand}
			err := model.NewError(model.CodeBand, "side_band_samples", "window [%d,%d) for frame %d is outside buffer [%d,%d); need %d samples", start, end, f, buf.Origin, buf.End, minBand)
			model.WithFrame(err, f)
			model.WithBlock(err, coord)
			return model.FrameReport{}, nil, err
		}
		var windowSSIMSum int64
		var windowSSIMCount int64
		for ch := 0; ch < m.Channels; ch++ {
			stats := pairStats{}
			for n := 0; n < p.WindowSizeSamples; n++ {
				t := start + n
				x := int64(buf.Original[ch][t-buf.Origin])
				y := int64(buf.Reconstructed[ch][t-buf.Origin])
				stats.add(x, y)
				windowResidual.add(y - x)
			}
			ssim := fixedSSIM(stats, peak, model.MetricScale)
			windowSSIMSum += ssim
			windowSSIMCount++
			ssimSum += ssim
			ssimCount++
		}
		noiseFixed := fixedStd(windowResidual, model.MetricScale)
		windows = append(windows, model.WindowMetric{
			WindowIndex: idx, Channel: -1, StartSample: start, EndSample: end, Frame: f,
			SampleCount:   p.WindowSizeSamples * m.Channels,
			SSIMFixed:     averageFixed(windowSSIMSum, windowSSIMCount),
			NoiseStdFixed: noiseFixed,
		})
	}
	ssimFixed := averageFixed(ssimSum, ssimCount)
	noiseFloor := fixedStd(windowResidual, model.MetricScale)
	frameStd := fixedStd(frameResidual, model.MetricScale)

	heat := make([]int, len(heatSums))
	for i := range heat {
		if heatCounts[i] > 0 {
			heat[i] = int(math.Round(float64(heatSums[i]) / float64(heatCounts[i])))
		}
	}

	pulseObserved := detectPulse(m, buf, a)
	rec := model.FrameReport{
		Frame: f, StartSample: a, PulseExpectedSample: -1, PulseObservedSample: pulseObserved,
		PulseMatch: false, SampleCount: m.FrameSize * m.Channels,
		PSNRDBFixed: psnrFixed, PSNRInfinite: psnrInf,
		SNRDBFixed: snrFixed, SNRInfinite: snrInf,
		SSIMFixed: ssimFixed, NoiseFloorStdFixed: noiseFloor, FrameResidualStdFixed: frameStd,
		WindowCount: len(windowIdxs), Heatmap: heat,
	}
	if f < len(quant) && quant[f].Frame == f {
		rec.Quant = quant[f]
	} else {
		rec.Quant = model.FrameQuantRecord{Frame: f, QuantStep: 0}
	}
	q := rec.Quant.QuantStep
	rec.Bound = bound.Build(bound.Input{Frame: f, QuantStep: q, FrameSize: m.FrameSize, WindowSize: p.WindowSizeSamples, Peak: peak, FracBits: model.CoefficientFracBits})
	return rec, windows, nil
}

func detectPulse(m model.Manifest, buf *RangeBuffer, start int) int {
	ch := 0
	bestSample := start
	bestScore := int64(-1)
	for n := 0; n < m.HopSize; n++ {
		t := start + n
		if t < buf.Origin || t >= buf.End {
			continue
		}
		x1 := int64(buf.Original[ch][t-buf.Origin])
		var score int64
		if n > 0 && t-1 >= buf.Origin {
			x0 := int64(buf.Original[ch][t-1-buf.Origin])
			score = x1 - x0
			if score < 0 {
				score = -score
			}
		} else {
			score = x1
			if score < 0 {
				score = -score
			}
		}
		if score > bestScore {
			bestScore = score
			bestSample = t
		}
	}
	return bestSample
}

func averageFixed(sum int64, count int64) int64 {
	if count == 0 {
		return 0
	}
	return int64(math.Round(float64(sum) / float64(count)))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

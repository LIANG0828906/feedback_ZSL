package forensics

import (
	"math"
	"sort"

	"integritykernel/internal/core"
	"integritykernel/internal/loader"
	"integritykernel/internal/watermark"
)

type LumaPlane struct {
	Width  int
	Height int
	Values []float64
	Mean   float64
}

func BuildLuma(frame *loader.Frame) *LumaPlane {
	img := frame.Image
	w, h := img.Rect.Dx(), img.Rect.Dy()
	plane := &LumaPlane{Width: w, Height: h, Values: make([]float64, w*h)}
	sum := 0.0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := watermark.LumaAt(img, x, y)
			plane.Values[y*w+x] = v
			sum += v
		}
	}
	plane.Mean = sum / float64(w*h)
	return plane
}

func AnalyzeResidual(frameIndex int, frames []*loader.Frame, planes []*LumaPlane, exclude core.Rect, params core.Params) core.ResidualEvidence {
	evidence := core.ResidualEvidence{Frame: frameIndex}
	if frameIndex <= 0 {
		evidence.AnomalyScore = 0
		return evidence
	}
	current := planes[frameIndex]
	start := frameIndex - params.ResidualWindow
	if start < 0 {
		start = 0
	}
	peakEnergies := []float64{}
	for other := frameIndex - 1; other >= start; other-- {
		ref := planes[other]
		var sumSq, sumAbs float64
		count := 0.0
		residualCell := 16
		gridCols := (current.Width + residualCell - 1) / residualCell
		gridRows := (current.Height + residualCell - 1) / residualCell
		blockSums := make([]float64, gridCols*gridRows)
		blockCounts := make([]int, gridCols*gridRows)
		for y := 0; y < current.Height; y++ {
			for x := 0; x < current.Width; x++ {
				if exclude.Contains(x, y) {
					continue
				}
				i := y*current.Width + x
				d := (current.Values[i] - current.Mean) - (ref.Values[i] - ref.Mean)
				sumSq += d * d
				sumAbs += math.Abs(d)
				count++
				bx := x / residualCell
				by := y / residualCell
				bi := by*gridCols + bx
				blockSums[bi] += d * d
				blockCounts[bi]++
			}
		}
		energy := sumSq / count
		mae := sumAbs / count
		blockEnergies := []float64{}
		for i, blockSum := range blockSums {
			if blockCounts[i] > 0 {
				blockEnergies = append(blockEnergies, blockSum/float64(blockCounts[i]))
			}
		}
		peak := quantile(blockEnergies, 0.9)
		peakEnergies = append(peakEnergies, peak)
		evidence.Lags = append(evidence.Lags, core.ResidualLag{
			Lag: frameIndex - other, OtherFrame: other, Energy: energy, MAE: mae,
		})
	}
	energies := make([]float64, 0, len(evidence.Lags))
	for _, lag := range evidence.Lags {
		energies = append(energies, lag.Energy)
	}
	evidence.MedianEnergy = median(energies)
	evidence.MaxEnergy = maxFloat(energies)
	evidence.PeakBlockEnergy = median(peakEnergies)
	rms := math.Sqrt(evidence.PeakBlockEnergy)
	denominator := 0.050 * params.ResidualZScale
	evidence.AnomalyScore = clamp01((rms - 0.012) / denominator)
	return evidence
}

func quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := int(math.Ceil(q*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func AnalyzeNoise(frameIndex int, planes []*LumaPlane, manifest core.Manifest, params core.Params) core.NoiseEvidence {
	plane := planes[frameIndex]
	w, h := plane.Width, plane.Height
	hp, gxPlane, gyPlane := highPassAndGradients(plane)
	cols := (w + params.BlockSize - 1) / params.BlockSize
	rows := (h + params.BlockSize - 1) / params.BlockSize
	evidence := core.NoiseEvidence{
		Frame: frameIndex, BlockSize: params.BlockSize, GridCols: cols, GridRows: rows,
		Blocks: make([]core.BlockStat, 0, cols*rows),
	}
	varianceByBlock := make([]float64, 0, cols*rows)
	historyFilters := priorFilters(frameIndex, planes, params.NoiseWindow)

	for by := 0; by < rows; by++ {
		y0 := by * params.BlockSize
		y1 := minInt(h, y0+params.BlockSize)
		for bx := 0; bx < cols; bx++ {
			x0 := bx * params.BlockSize
			x1 := minInt(w, x0+params.BlockSize)
			stat := blockStat(plane, hp, gxPlane, gyPlane, x0, y0, x1, y1, bx, by, params)
			evidence.Blocks = append(evidence.Blocks, stat)
			varianceByBlock = append(varianceByBlock, stat.NoiseVariance)
		}
	}

	evidence.MedianVariance = median(varianceByBlock)
	evidence.MADVariance = mad(varianceByBlock, evidence.MedianVariance)
	addTemporalDispersion(historyFilters, evidence.Blocks)
	logMedian := 0.0
	logMAD := 0.0
	if len(varianceByBlock) > 0 {
		logs := make([]float64, len(varianceByBlock))
		for i, v := range varianceByBlock {
			logs[i] = math.Log(v + 1e-8)
		}
		logMedian = median(logs)
		deviations := make([]float64, len(logs))
		for i, v := range logs {
			deviations[i] = math.Abs(v - logMedian)
		}
		logMAD = median(deviations)
	}
	scale := 1.4826*logMAD + 0.18
	anomalyScoreSum := 0.0
	maxZ := 0.0
	incoherentHighVariance := 0
	for i := range evidence.Blocks {
		logV := math.Log(evidence.Blocks[i].NoiseVariance + 1e-8)
		z := (logV - logMedian) / scale
		dispersion := math.Abs(z)
		evidence.Blocks[i].SpatialZ = z
		evidence.Blocks[i].Dispersion = dispersion
		temporalAnomaly := evidence.Blocks[i].TemporalZ >= params.NoiseZThreshold
		spatialAnomaly := dispersion >= params.NoiseZThreshold &&
			evidence.Blocks[i].EdgeConsistency < params.EdgeAnomalyThreshold
		evidence.Blocks[i].Anomalous = temporalAnomaly || spatialAnomaly
		if evidence.Blocks[i].Anomalous {
			evidence.AnomalousBlocks++
			anomalyScoreSum += minFloat(1, (dispersion-params.NoiseZThreshold)/2.0+0.55)
		}
		if spatialAnomaly {
			incoherentHighVariance++
		}
		blockMaxZ := maxAbsFloat(dispersion, math.Abs(evidence.Blocks[i].TemporalZ))
		if blockMaxZ > maxZ {
			maxZ = blockMaxZ
		}
	}
	evidence.AnomalyFraction = float64(evidence.AnomalousBlocks) / float64(len(evidence.Blocks))
	evidence.MaxTemporalZ = maxZ
	evidence.EdgeAnomaly = float64(incoherentHighVariance) / float64(len(evidence.Blocks))
	evidence.AnomalyScore = clamp01(anomalyScoreSum / math.Max(1, float64(evidence.AnomalousBlocks)+2.0))
	if evidence.AnomalousBlocks > 0 {
		fractionScore := clamp01(evidence.AnomalyFraction / 0.16)
		zScore := clamp01((maxZ - params.NoiseZThreshold) / 4.0)
		evidence.AnomalyScore = clamp01(0.55*fractionScore + 0.45*zScore)
	}
	evidence.PrimitiveEdges = primitiveEdgeStats(plane, gxPlane, gyPlane, manifest.Primitives, params)
	return evidence
}

type frameFilters struct {
	plane      *LumaPlane
	hp, gx, gy []float64
}

func priorFilters(frameIndex int, planes []*LumaPlane, window int) []frameFilters {
	start := frameIndex - window
	if start < 0 {
		start = 0
	}
	filters := make([]frameFilters, 0, frameIndex-start)
	for prior := frameIndex - 1; prior >= start; prior-- {
		hp, gx, gy := highPassAndGradients(planes[prior])
		filters = append(filters, frameFilters{plane: planes[prior], hp: hp, gx: gx, gy: gy})
	}
	return filters
}

func addTemporalDispersion(history []frameFilters, blocks []core.BlockStat) {
	for blockIndex := range blocks {
		block := &blocks[blockIndex]
		historyLogs := make([]float64, 0, len(history))
		for _, prior := range history {
			x1 := minInt(prior.plane.Width, block.PixelX+block.Width)
			y1 := minInt(prior.plane.Height, block.PixelY+block.Height)
			variance := maskedBlockVariance(prior.plane, prior.hp, prior.gx, prior.gy, block.PixelX, block.PixelY, x1, y1)
			historyLogs = append(historyLogs, math.Log(variance+1e-8))
		}
		if len(historyLogs) == 0 {
			block.TemporalZ = 0
			continue
		}
		mean := 0.0
		for _, v := range historyLogs {
			mean += v
		}
		mean /= float64(len(historyLogs))
		variance := 0.0
		for _, v := range historyLogs {
			variance += (v - mean) * (v - mean)
		}
		std := math.Sqrt(variance/float64(len(historyLogs))) + 0.22
		currentLog := math.Log(block.NoiseVariance + 1e-8)
		block.TemporalZ = (currentLog - mean) / std
	}
}

func highPassAndGradients(plane *LumaPlane) (hp, gxOut, gyOut []float64) {
	w, h := plane.Width, plane.Height
	hp = make([]float64, w*h)
	gxOut = make([]float64, w*h)
	gyOut = make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sum := 0.0
			count := 0.0
			for dy := -1; dy <= 1; dy++ {
				yy := y + dy
				if yy < 0 || yy >= h {
					continue
				}
				for dx := -1; dx <= 1; dx++ {
					xx := x + dx
					if xx < 0 || xx >= w {
						continue
					}
					sum += plane.Values[yy*w+xx]
					count++
				}
			}
			blur := sum / count
			i := y*w + x
			hp[i] = plane.Values[i] - blur

			if x > 0 && x < w-1 && y > 0 && y < h-1 {
				tl := plane.Values[(y-1)*w+x-1]
				tc := plane.Values[(y-1)*w+x]
				tr := plane.Values[(y-1)*w+x+1]
				ml := plane.Values[y*w+x-1]
				mr := plane.Values[y*w+x+1]
				bl := plane.Values[(y+1)*w+x-1]
				bc := plane.Values[(y+1)*w+x]
				br := plane.Values[(y+1)*w+x+1]
				gxOut[i] = -tl - 2*ml - bl + tr + 2*mr + br
				gyOut[i] = -tl - 2*tc - tr + bl + 2*bc + br
			}
		}
	}
	return hp, gxOut, gyOut
}

func blockStat(plane *LumaPlane, hp, gx, gy []float64, x0, y0, x1, y1, bx, by int, params core.Params) core.BlockStat {
	w := plane.Width
	binWeights := make([]float64, params.EdgeBins)
	totalWeight := 0.0
	gradientEnergy := 0.0
	dominantBin := 0
	var noiseSum, noiseSumSq float64
	noiseCount := 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			i := y*w + x
			gxv, gyv := gx[i], gy[i]
			mag := math.Hypot(gxv, gyv)
			gradientEnergy += mag * mag
			if mag < 0.18 {
				v := hp[i]
				noiseSum += v
				noiseSumSq += v * v
				noiseCount++
			}
			if mag > 0.025 {
				angle := math.Atan2(gyv, gxv)
				if angle < 0 {
					angle += math.Pi
				}
				if angle >= math.Pi {
					angle = math.Pi - 1e-12
				}
				bin := int(angle / math.Pi * float64(params.EdgeBins))
				if bin >= params.EdgeBins {
					bin = params.EdgeBins - 1
				}
				weight := mag
				binWeights[bin] += weight
				totalWeight += weight
				if binWeights[bin] > binWeights[dominantBin] {
					dominantBin = bin
				}
			}
		}
	}
	variance := 0.0
	if noiseCount >= 8 {
		meanNoise := noiseSum / float64(noiseCount)
		variance = math.Max(0, noiseSumSq/float64(noiseCount)-meanNoise*meanNoise)
	} else {
		variance = blockVariance(plane, hp, x0, y0, x1, y1)
	}
	strength := 0.0
	consistency := 1.0
	if totalWeight > 1e-9 {
		strength = binWeights[dominantBin] / totalWeight
		energyPerPixel := gradientEnergy / float64((x1-x0)*(y1-y0))
		if energyPerPixel > 0.006 {
			consistency = strength
		} else {
			consistency = 0.85 + 0.15*strength
		}
	}
	return core.BlockStat{
		BlockX: bx, BlockY: by, PixelX: x0, PixelY: y0,
		Width: x1 - x0, Height: y1 - y0,
		NoiseVariance: variance, OrientationBin: dominantBin,
		OrientationStrength: strength, EdgeConsistency: consistency,
	}
}

func blockVariance(plane *LumaPlane, hp []float64, x0, y0, x1, y1 int) float64 {
	w := plane.Width
	var sum, sumSq float64
	count := 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			v := hp[y*w+x]
			sum += v
			sumSq += v * v
			count++
		}
	}
	if count == 0 {
		return 0
	}
	mean := sum / float64(count)
	return math.Max(0, sumSq/float64(count)-mean*mean)
}

func maskedBlockVariance(plane *LumaPlane, hp, gx, gy []float64, x0, y0, x1, y1 int) float64 {
	w := plane.Width
	var sum, sumSq float64
	count := 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			i := y*w + x
			if math.Hypot(gx[i], gy[i]) >= 0.18 {
				continue
			}
			v := hp[i]
			sum += v
			sumSq += v * v
			count++
		}
	}
	if count < 8 {
		return blockVariance(plane, hp, x0, y0, x1, y1)
	}
	mean := sum / float64(count)
	return math.Max(0, sumSq/float64(count)-mean*mean)
}

func primitiveEdgeStats(plane *LumaPlane, gx, gy []float64, primitives []core.Primitive, params core.Params) []core.PrimitiveEdgeStat {
	out := make([]core.PrimitiveEdgeStat, 0, len(primitives))
	w := plane.Width
	for _, primitive := range primitives {
		r := primitive.BBox
		x0 := maxInt(0, r.X)
		y0 := maxInt(0, r.Y)
		x1 := minInt(plane.Width, r.MaxX())
		y1 := minInt(plane.Height, r.MaxY())
		weights := make([]float64, params.EdgeBins)
		total := 0.0
		energy := 0.0
		dominant := 0
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				i := y*w + x
				mag := math.Hypot(gx[i], gy[i])
				if mag <= 0.025 {
					continue
				}
				angle := math.Atan2(gy[i], gx[i])
				if angle < 0 {
					angle += math.Pi
				}
				if angle >= math.Pi {
					angle = math.Pi - 1e-12
				}
				bin := int(angle / math.Pi * float64(params.EdgeBins))
				if bin >= params.EdgeBins {
					bin = params.EdgeBins - 1
				}
				weights[bin] += mag
				total += mag
				energy += mag * mag
				if weights[bin] > weights[dominant] {
					dominant = bin
				}
			}
		}
		consistency := 1.0
		if total > 1e-9 {
			consistency = weights[dominant] / total
		}
		out = append(out, core.PrimitiveEdgeStat{
			ID: primitive.ID, DominantBin: dominant,
			Consistency: consistency, GradientEnergy: energy / math.Max(1, float64((x1-x0)*(y1-y0))),
		})
	}
	return out
}

func BuildTimeline(watermarks []*core.WatermarkEvidence) core.WatermarkTimeline {
	timeline := core.WatermarkTimeline{DecodedValues: make([]int, len(watermarks)), Confidences: make([]float64, len(watermarks))}
	for i, wm := range watermarks {
		timeline.DecodedValues[i] = wm.DecodedFrame
		timeline.Confidences[i] = wm.OverallConfidence
		if i == 0 {
			continue
		}
		prev := watermarks[i-1]
		reason := ""
		switch {
		case wm.Missing || prev.Missing:
			reason = "watermark_missing"
		case wm.Misaligned || prev.Misaligned:
			reason = "watermark_misaligned"
		case wm.DecodedFrame != prev.DecodedFrame+1:
			reason = "decoded_jump"
		}
		if reason != "" {
			timeline.Breaks = append(timeline.Breaks, core.TimelineBreak{
				AfterFrame: i - 1, FromValue: prev.DecodedFrame, ToValue: wm.DecodedFrame, Reason: reason,
			})
		}
	}
	return timeline
}

func Judge(frame int, wm *core.WatermarkEvidence, residual core.ResidualEvidence, noise core.NoiseEvidence, params core.Params) core.VerdictEvidence {
	verdict := core.VerdictEvidence{Frame: frame}
	if wm != nil {
		switch {
		case wm.Missing:
			verdict.WatermarkScore = 0.92
		case wm.Misaligned:
			verdict.WatermarkScore = 0.86
		case !wm.MatchesExpected:
			verdict.WatermarkScore = 1.0
		default:
			verdict.WatermarkScore = clamp01((1-wm.OverallConfidence)*0.35 + 0.015)
		}
		if verdict.WatermarkScore >= 0.45 {
			if !wm.MatchesExpected {
				verdict.Reasons = append(verdict.Reasons, "watermark:decoded_frame_mismatch")
			}
			if wm.Missing {
				verdict.Reasons = append(verdict.Reasons, "watermark:missing_modules")
			}
			if wm.Misaligned {
				verdict.Reasons = append(verdict.Reasons, "watermark:misaligned_modules")
			}
		}
	}
	verdict.ResidualScore = residual.AnomalyScore
	if verdict.ResidualScore >= 0.35 {
		verdict.Reasons = append(verdict.Reasons, "residual:elevated_interframe_energy")
	}
	verdict.NoiseScore = noise.AnomalyScore
	if verdict.NoiseScore >= 0.35 {
		verdict.Reasons = append(verdict.Reasons, "noise:block_dispersion_or_edge_incoherence")
	}
	weightSum := params.WatermarkWeight + params.ResidualWeight + params.NoiseWeight
	verdict.Score = (params.WatermarkWeight*verdict.WatermarkScore +
		params.ResidualWeight*verdict.ResidualScore +
		params.NoiseWeight*verdict.NoiseScore) / weightSum

	corroborating := 0
	if verdict.WatermarkScore >= 0.35 {
		corroborating++
	}
	if verdict.ResidualScore >= 0.35 {
		corroborating++
	}
	if verdict.NoiseScore >= 0.35 {
		corroborating++
	}
	verdict.Tampered = verdict.Score >= params.VerdictThreshold && corroborating >= 2
	if verdict.Tampered {
		verdict.Confidence = clamp01(0.55 + 0.45*verdict.Score)
	} else {
		verdict.Confidence = clamp01(0.55 + 0.45*(1-verdict.Score))
	}
	return verdict
}

func LocateIntervals(verdicts []core.VerdictEvidence) []core.Interval {
	var intervals []core.Interval
	for i := 0; i < len(verdicts); {
		if !verdicts[i].Tampered {
			i++
			continue
		}
		start := i
		confidenceSum := 0.0
		for i < len(verdicts) && verdicts[i].Tampered {
			confidenceSum += verdicts[i].Confidence
			i++
		}
		end := i - 1
		count := end - start + 1
		intervals = append(intervals, core.Interval{
			StartFrame: start, EndFrame: end, FrameCount: count,
			Confidence: confidenceSum / float64(count),
		})
	}
	return intervals
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func mad(values []float64, med float64) float64 {
	dev := make([]float64, len(values))
	for i, v := range values {
		dev[i] = math.Abs(v - med)
	}
	return median(dev)
}

func maxFloat(values []float64) float64 {
	best := 0.0
	for _, v := range values {
		if v > best {
			best = v
		}
	}
	return best
}

func maxAbsFloat(a, b float64) float64 {
	if math.Abs(a) > math.Abs(b) {
		return math.Abs(a)
	}
	return math.Abs(b)
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
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

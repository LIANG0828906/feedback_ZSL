package verify

import (
	"math"
	"sort"

	"screenverify/internal/model"
)

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func lumaAt(f *model.Frame, x, y int) float64 {
	if x < 0 {
		x = 0
	} else if x >= f.Width {
		x = f.Width - 1
	}
	if y < 0 {
		y = 0
	} else if y >= f.Height {
		y = f.Height - 1
	}
	i := (y*f.Width + x) * f.Channels
	p := f.Pixels
	return 0.299*float64(p[i]) + 0.587*float64(p[i+1]) + 0.114*float64(p[i+2])
}

func lumaPlane(f *model.Frame) []float64 {
	out := make([]float64, f.Width*f.Height)
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			out[y*f.Width+x] = lumaAt(f, x, y)
		}
	}
	return out
}

func decodeWatermark(f *model.Frame, region model.Rect, p Params) WatermarkEvidence {
	ev := WatermarkEvidence{
		Frame:     f.No,
		Region:    region,
		GridX:     p.GridX,
		GridY:     p.GridY,
		Threshold: p.WatermarkThreshold,
		Bits:      make([]BitEvidence, 0, p.GridX*p.GridY),
	}
	cellW := float64(region.Width) / float64(p.GridX)
	cellH := float64(region.Height) / float64(p.GridY)
	values := make([]int, 0, 32)
	confs := make([]float64, 0, 32)
	sumConf := 0.0
	minConf := 1.0
	for row := 0; row < p.GridY; row++ {
		for col := 0; col < p.GridX; col++ {
			idx := row*p.GridX + col
			x := region.X + int(math.Floor((float64(col)+0.5)*cellW))
			y := region.Y + int(math.Floor((float64(row)+0.5)*cellH))
			luma := averageCenterLuma(f, x, y, cellW, cellH)
			bit := 0
			if luma >= 128 {
				bit = 1
			}
			distance := math.Abs(luma - 128)
			conf := clamp01((distance - 8) / 78)
			be := BitEvidence{Index: idx, Row: row, Col: col, X: x, Y: y, Bit: bit, Luma: round6(luma), Confidence: round6(conf)}
			ev.Bits = append(ev.Bits, be)
			if idx < 32 {
				values = append(values, bit)
				confs = append(confs, conf)
				sumConf += conf
				if conf < minConf {
					minConf = conf
				}
			}
		}
	}
	value := 0
	for i, bit := range values {
		if i >= 32 {
			break
		}
		value = (value << 1) | bit
	}
	ev.DecodedFrame = value
	if len(confs) > 0 {
		ev.MeanConfidence = round6(sumConf / float64(len(confs)))
		ev.MinConfidence = round6(minConf)
	}
	accepted := 0
	for _, c := range confs {
		if c >= p.WatermarkThreshold {
			accepted++
		}
	}
	ev.Decoded = accepted == len(confs)
	ev.Missing = ev.MeanConfidence < p.WatermarkThreshold*0.45
	ev.Misaligned = !ev.Missing && (ev.MinConfidence < p.WatermarkThreshold || accepted < len(confs)-2)
	return ev
}

func averageCenterLuma(f *model.Frame, cx, cy int, cellW, cellH float64) float64 {
	radius := 0
	if cellW >= 6 && cellH >= 6 {
		radius = 1
	}
	sum := 0.0
	n := 0
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			sum += lumaAt(f, cx+dx, cy+dy)
			n++
		}
	}
	return sum / float64(n)
}

func buildTimeline(cur WatermarkEvidence, prev *WatermarkEvidence) TimelineEvidence {
	ev := TimelineEvidence{
		Frame:         cur.Frame,
		ExpectedFrame: cur.Frame,
		DecodedFrame:  cur.DecodedFrame,
		Missing:       cur.Missing || !cur.Decoded,
		Misaligned:    cur.Misaligned,
	}
	if cur.Decoded && cur.DecodedFrame != cur.Frame {
		ev.PositionMismatch = true
	}
	if prev != nil {
		ev.HasPrevious = true
		ev.PreviousDecoded = prev.DecodedFrame
		if prev.Decoded && cur.Decoded && cur.DecodedFrame != prev.DecodedFrame+1 {
			ev.Jump = true
		}
	}
	ev.Anomaly = ev.Jump || ev.PositionMismatch || cur.Missing || cur.Misaligned || !cur.Decoded
	switch {
	case cur.Missing:
		ev.Score = 0.97
	case ev.Jump:
		ev.Score = 0.94
	case ev.PositionMismatch:
		ev.Score = 0.92
	case cur.Misaligned || !cur.Decoded:
		ev.Score = 0.88
	default:
		ev.Score = 0
	}
	if !ev.Anomaly {
		ev.Score = 0
	}
	return ev
}

func energyBetween(a, b *model.Frame) float64 {
	var sum float64
	n := 0
	w := a.Width
	for y := 0; y < a.Height; y++ {
		for x := 0; x < w; x++ {
			d := lumaAt(b, x, y) - lumaAt(a, x, y)
			sum += d * d
			n++
		}
	}
	return sum / float64(n)
}

func residualEvidence(frames []*model.Frame, idx int, p Params) ResidualEvidence {
	f := frames[idx]
	ev := ResidualEvidence{Frame: f.No, Window: p.ResidualWindow}
	if idx == 0 {
		return ev
	}
	ev.Energy = round6(energyBetween(frames[idx-1], f))
	prior := make([]float64, 0, p.ResidualWindow)
	for k := 1; k <= p.ResidualWindow; k++ {
		end := idx - k
		if end >= 1 {
			prior = append(prior, energyBetween(frames[end-1], frames[end]))
		}
	}
	if len(prior) > 0 {
		mean, std := meanStd(prior)
		ev.PreviousMean = round6(mean)
		ev.PreviousStd = round6(std)
		denom := std + 0.45*mean + 18
		ev.ZScore = round6((ev.Energy - mean) / denom)
	}
	ev.Score = round6(clamp01((ev.ZScore - 1.5) / 5.5))
	return ev
}

func noiseEvidence(f *model.Frame, p Params) NoiseEvidence {
	ev := NoiseEvidence{Frame: f.No, BlockSize: p.BlockSize}
	luma := lumaPlane(f)
	cols := f.Width / p.BlockSize
	rows := f.Height / p.BlockSize
	variances := make([]float64, 0, cols*rows)
	for by := 0; by < rows; by++ {
		for bx := 0; bx < cols; bx++ {
			block := NoiseBlock{
				BlockX: bx, BlockY: by,
				X: bx * p.BlockSize, Y: by * p.BlockSize,
				Width: p.BlockSize, Height: p.BlockSize,
			}
			laps := make([]float64, 0, p.BlockSize*p.BlockSize)
			for y := block.Y; y < block.Y+p.BlockSize; y++ {
				for x := block.X; x < block.X+p.BlockSize; x++ {
					lap := 4*planeAt(luma, f.Width, f.Height, x, y) -
						planeAt(luma, f.Width, f.Height, x-1, y) - planeAt(luma, f.Width, f.Height, x+1, y) -
						planeAt(luma, f.Width, f.Height, x, y-1) - planeAt(luma, f.Width, f.Height, x, y+1)
					laps = append(laps, lap)
				}
			}
			block.RawVariance = round6(populationVariance(laps))
			med := median(laps)
			absDev := make([]float64, len(laps))
			for i, v := range laps {
				absDev[i] = math.Abs(v - med)
			}
			mad := median(absDev)
			sigma := 1.4826 * mad
			block.Variance = round6(sigma * sigma)
			ev.Blocks = append(ev.Blocks, block)
			variances = append(variances, block.Variance)
		}
	}

	ev.MedianVariance = round6(median(variances))
	devs := make([]float64, len(variances))
	for i, v := range variances {
		devs[i] = math.Abs(v - ev.MedianVariance)
	}
	ev.MAD = round6(median(devs))
	mean, std := meanStd(variances)
	ev.Dispersion = round6(std / (mean + 1e-6))
	denomZ := 1.4826*ev.MAD + 0.22*ev.MedianVariance + 1e-6
	outliers := 0
	for i := range ev.Blocks {
		b := &ev.Blocks[i]
		b.Ratio = round6(b.Variance / (ev.MedianVariance + 1e-6))
		b.ZScore = round6((b.Variance - ev.MedianVariance) / denomZ)
		if b.ZScore > 4.0 {
			outliers++
		}
		if b.ZScore > ev.MaxZScore {
			ev.MaxZScore = b.ZScore
		}
	}
	ev.MaxZScore = round6(ev.MaxZScore)
	ev.OutlierFraction = round6(float64(outliers) / float64(len(ev.Blocks)))
	ev.Score = round6(clamp01((ev.MaxZScore-3.0)/10.0 + ev.OutlierFraction*1.4))
	return ev
}

func planeAt(plane []float64, width, height, x, y int) float64 {
	if x < 0 {
		x = 0
	} else if x >= width {
		x = width - 1
	}
	if y < 0 {
		y = 0
	} else if y >= height {
		y = height - 1
	}
	return plane[y*width+x]
}

func edgeEvidence(frames []*model.Frame, truth *model.Truth, idx int) EdgeEvidence {
	f := frames[idx]
	ev := EdgeEvidence{Frame: f.No, Bins: 8, Primitives: make([]EdgePrimitive, 0, len(truth.Frames[idx].Primitives))}
	var sumCons, minCons float64
	for pi, prim := range truth.Frames[idx].Primitives {
		cur := orientationHistogram(f, prim.BBox, 8)
		ref := cur
		if idx > 0 {
			ref = orientationHistogram(frames[idx-1], prim.BBox, 8)
		}
		cons := histogramIntersection(cur, ref)
		if histogramEmpty(cur) && histogramEmpty(ref) {
			cons = 1
		}
		if idx == 0 {
			cons = 1
		}
		ep := EdgePrimitive{Label: prim.Label, BBox: prim.BBox, Consistency: round6(cons), Histogram: roundSlice(cur)}
		ev.Primitives = append(ev.Primitives, ep)
		sumCons += cons
		if pi == 0 || cons < minCons {
			minCons = cons
		}
	}
	if len(ev.Primitives) > 0 {
		ev.MeanConsistency = round6(sumCons / float64(len(ev.Primitives)))
		ev.MinConsistency = round6(minCons)
	} else {
		ev.MeanConsistency, ev.MinConsistency = 1, 1
	}
	ev.Score = round6(clamp01((0.86 - ev.MinConsistency) / 0.38))
	return ev
}

func orientationHistogram(f *model.Frame, r model.Rect, bins int) []float64 {
	hist := make([]float64, bins)
	total := 0.0
	for y := r.Y; y < r.Y+r.Height; y++ {
		for x := r.X; x < r.X+r.Width; x++ {
			gx := lumaAt(f, x+1, y) - lumaAt(f, x-1, y)
			gy := lumaAt(f, x, y+1) - lumaAt(f, x, y-1)
			mag := math.Hypot(gx, gy)
			if mag < 18 {
				continue
			}
			angle := math.Atan2(gy, gx)
			if angle < 0 {
				angle += math.Pi
			}
			if angle >= math.Pi {
				angle = math.Pi - 1e-9
			}
			bin := int(angle / math.Pi * float64(bins))
			if bin < 0 {
				bin = 0
			}
			if bin >= bins {
				bin = bins - 1
			}
			hist[bin] += mag
			total += mag
		}
	}
	if total > 0 {
		for i := range hist {
			hist[i] /= total
		}
	}
	return hist
}

func histogramIntersection(a, b []float64) float64 {
	sum := 0.0
	for i := range a {
		sum += math.Min(a[i], b[i])
	}
	return sum
}

func histogramEmpty(h []float64) bool {
	for _, v := range h {
		if v != 0 {
			return false
		}
	}
	return true
}

func decide(f *model.Frame, p Params, wm WatermarkEvidence, tl TimelineEvidence, res ResidualEvidence, noise NoiseEvidence, edge EdgeEvidence) DecisionEvidence {
	d := DecisionEvidence{
		Frame:          f.No,
		Threshold:      p.TamperThreshold,
		WatermarkScore: round6(tl.Score),
		ResidualScore:  res.Score,
		NoiseScore:     noise.Score,
		EdgeScore:      edge.Score,
	}
	d.Score = round6(math.Max(math.Max(tl.Score, res.Score), math.Max(noise.Score, edge.Score)))
	if tl.Anomaly {
		strong := tl.Jump || tl.PositionMismatch || wm.Missing || wm.MinConfidence < p.WatermarkThreshold
		if strong {
			d.Reasons = append(d.Reasons, "watermark_timeline_break")
		}
	}
	if res.ZScore >= p.ResidualZThreshold && res.Score > 0.15 {
		d.Reasons = append(d.Reasons, "residual_energy_spike")
	}
	noiseBlocks := 0
	for _, b := range noise.Blocks {
		if b.ZScore >= p.NoiseZThreshold {
			noiseBlocks++
		}
	}
	if noiseBlocks >= 2 {
		d.Reasons = append(d.Reasons, "block_noise_outlier_cluster")
	}
	if edge.MinConsistency < p.EdgeThreshold && edge.Score > 0.1 {
		d.Reasons = append(d.Reasons, "primitive_edge_direction_change")
	}
	d.Tampered = d.Score > p.TamperThreshold && len(d.Reasons) > 0
	denom := math.Max(1e-9, 1-p.TamperThreshold)
	d.Confidence = clamp01((d.Score - p.TamperThreshold) / denom)
	if !d.Tampered {
		d.Confidence = 0
	}
	d.Confidence = round6(d.Confidence)
	return d
}

func LocateIntervals(decisions []DecisionEvidence, tolerance int) []Interval {
	type run struct{ start, end int }
	runs := make([]run, 0)
	for i := 0; i < len(decisions); {
		if !decisions[i].Tampered {
			i++
			continue
		}
		start := decisions[i].Frame
		end := start
		j := i + 1
		for j < len(decisions) {
			if decisions[j].Tampered {
				end = decisions[j].Frame
				j++
				continue
			}
			gapEnd := j
			for gapEnd < len(decisions) && !decisions[gapEnd].Tampered && gapEnd-j < maxInt(1, tolerance) {
				gapEnd++
			}
			if gapEnd < len(decisions) && decisions[gapEnd].Tampered && gapEnd-j <= maxInt(1, tolerance) {
				end = decisions[gapEnd].Frame
				j = gapEnd + 1
				continue
			}
			break
		}
		runs = append(runs, run{start, end})
		i = j
	}
	out := make([]Interval, 0, len(runs))
	for _, r := range runs {
		iv := Interval{StartFrame: r.start, EndFrame: r.end, FrameCount: r.end - r.start + 1, Kind: "content_tamper"}
		types := map[EvidenceType]bool{}
		watermarkOnly := true
		for f := r.start; f <= r.end; f++ {
			if f-1 >= 0 && f-1 < len(decisions) {
				for _, reason := range decisions[f-1].Reasons {
					if reason == "watermark_timeline_break" {
						types[EvTimeline] = true
					} else {
						watermarkOnly = false
						types[EvResidual] = true
						types[EvNoise] = true
						types[EvEdge] = true
					}
				}
			}
		}
		if watermarkOnly && types[EvTimeline] {
			iv.Kind = "watermark_break"
			iv.EvidenceTypes = []EvidenceType{EvWatermark, EvTimeline, EvDecision}
		} else {
			iv.Kind = "content_tamper"
			iv.EvidenceTypes = []EvidenceType{EvResidual, EvNoise, EvEdge, EvDecision}
			if types[EvTimeline] {
				iv.EvidenceTypes = append([]EvidenceType{EvWatermark, EvTimeline}, iv.EvidenceTypes...)
			}
		}
		out = append(out, iv)
	}
	return out
}

func IntervalMatchesTruth(iv Interval, truth *model.Truth) bool {
	r := truth.Replacement
	if r.StartFrame < 0 {
		return false
	}
	tol := truth.FrameTolerance
	return absInt(iv.StartFrame-r.StartFrame) <= tol && absInt(iv.EndFrame-r.EndFrame) <= tol
}

func meanStd(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	ss := 0.0
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(ss / float64(len(xs)))
}

func populationVariance(xs []float64) float64 {
	mean, _ := meanStd(xs)
	ss := 0.0
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return ss / float64(len(xs))
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

func roundSlice(xs []float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = round6(x)
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func absInt(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

package forensic

import (
	"math"
)

// AnalyzeBlock computes statistics from one block only. Neighbour lookups are
// clamped to the block bounds, which keeps block results independent of
// scheduling, cache hits and other blocks.
func AnalyzeBlock(img *Image, chunk Chunk, p Params) (Stats, error) {
	if p.Window < 3 || p.Window%2 == 0 {
		return Stats{}, ParamError("window", "must be an odd integer of at least 3")
	}
	w, h := chunk.Width(), chunk.Height()
	window := p.Window
	if window > w {
		window = w
	}
	if window > h {
		window = h
	}
	if window%2 == 0 {
		window--
	}
	if window < 1 {
		window = 1
	}
	gray := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			gray[y*w+x] = float64(luma(img, chunk.X0+x, chunk.Y0+y))
		}
	}

	radius := window / 2
	var residualSS float64
	residualCount := 0
	highResidualCount := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < radius || y < radius || x >= w-radius || y >= h-radius {
				continue
			}
			var sum float64
			for wy := -radius; wy <= radius; wy++ {
				for wx := -radius; wx <= radius; wx++ {
					sum += gray[(y+wy)*w+x+wx]
				}
			}
			mean := sum / float64(window*window)
			d := gray[y*w+x] - mean
			residualSS += d * d
			if math.Abs(d) >= 9 {
				highResidualCount++
			}
			residualCount++
		}
	}
	variance := 0.0
	coverage := 0.0
	if residualCount > 0 {
		variance = residualSS / float64(residualCount)
		coverage = float64(highResidualCount) / float64(residualCount)
	}

	var mean float64
	for _, v := range gray {
		mean += v
	}
	mean /= float64(len(gray))
	var contrastSS float64
	for _, v := range gray {
		d := v - mean
		contrastSS += d * d
	}
	contrast := math.Sqrt(contrastSS/float64(len(gray))) / 255.0

	edgeConsistency := edgeDirectionConsistency(gray, w, h)
	noiseRMS := math.Sqrt(variance)
	noiseTerm := clampFloat(noiseRMS/32.0, 0, 1) * clampFloat(coverage/0.55, 0, 1)
	edgeTerm := clampFloat(1-edgeConsistency, 0, 1)
	contrastTerm := clampFloat(contrast/0.5, 0, 1)
	score := 0.72*noiseTerm + 0.16*edgeTerm + 0.12*contrastTerm
	return Stats{
		NoiseVariance: variance, NoiseRMS: noiseRMS, ResidualCoverage: coverage,
		EdgeConsistency: edgeConsistency, Contrast: contrast,
		TamperScore: clampFloat(score, 0, 1),
	}, nil
}

func edgeDirectionConsistency(gray []float64, w, h int) float64 {
	if w < 3 || h < 3 {
		return 1
	}
	const bins = 8
	magnitudes := make([]float64, bins)
	var total float64
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			gx := -gray[(y-1)*w+x-1] + gray[(y-1)*w+x+1] -
				2*gray[y*w+x-1] + 2*gray[y*w+x+1] -
				gray[(y+1)*w+x-1] + gray[(y+1)*w+x+1]
			gy := -gray[(y-1)*w+x-1] - 2*gray[(y-1)*w+x] - gray[(y-1)*w+x+1] +
				gray[(y+1)*w+x-1] + 2*gray[(y+1)*w+x] + gray[(y+1)*w+x+1]
			mag := math.Hypot(gx, gy)
			if mag == 0 {
				continue
			}
			angle := math.Atan2(gy, gx)
			if angle < 0 {
				angle += math.Pi
			}
			bin := int(angle/(math.Pi/float64(bins))) % bins
			magnitudes[bin] += mag
			total += mag
		}
	}
	if total == 0 {
		return 1
	}
	max := 0.0
	for _, v := range magnitudes {
		if v > max {
			max = v
		}
	}
	return clampFloat(max/total, 0, 1)
}

func TruthBlocks(img *Image, chunks []Chunk) []BlockTruth {
	out := make([]BlockTruth, len(chunks))
	for _, chunk := range chunks {
		count := 0
		for y := chunk.Y0; y < chunk.Y1; y++ {
			for x := chunk.X0; x < chunk.X1; x++ {
				if img.Mask[y*img.Width+x] > 0 {
					count++
				}
			}
		}
		out[chunk.Index] = BlockTruth{BlockIndex: chunk.Index, Tampered: count > 0, Pixels: count}
	}
	return out
}

func EvaluateTruth(states []BlockState, truth []BlockTruth) (TruthConfusion, float64) {
	var c TruthConfusion
	for _, s := range states {
		if s.BlockIndex < 0 || s.BlockIndex >= len(truth) {
			continue
		}
		want := truth[s.BlockIndex].Tampered
		switch {
		case s.Tampered && want:
			c.TruePositive++
		case !s.Tampered && !want:
			c.TrueNegative++
		case s.Tampered && !want:
			c.FalsePositive++
		default:
			c.FalseNegative++
		}
	}
	total := c.TruePositive + c.TrueNegative + c.FalsePositive + c.FalseNegative
	agreement := 1.0
	if total > 0 {
		agreement = float64(c.TruePositive+c.TrueNegative) / float64(total)
	}
	return c, agreement
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

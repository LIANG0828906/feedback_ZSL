package eval

import (
	"sort"

	"semanticshading/internal/colormetric"
	"semanticshading/internal/model"
	"semanticshading/internal/sample"
)

func paddedCopy(src []byte, width, channels int, pr model.BlockCoord) []byte {
	pw := pr.X1 - pr.X0
	ph := pr.Y1 - pr.Y0
	buf := make([]byte, pw*ph*channels)
	for y := pr.Y0; y < pr.Y1; y++ {
		srcStart := (y*width + pr.X0) * channels
		srcEnd := (y*width + pr.X1) * channels
		dstStart := ((y - pr.Y0) * pw) * channels
		copy(buf[dstStart:dstStart+(pr.X1-pr.X0)*channels], src[srcStart:srcEnd])
	}
	return buf
}

func paddedRect(m model.Manifest, b model.BlockCoord, band int) model.BlockCoord {
	x0 := b.X0 - band
	y0 := b.Y0 - band
	x1 := b.X1 + band
	y1 := b.Y1 + band
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > m.Width {
		x1 = m.Width
	}
	if y1 > m.Height {
		y1 = m.Height
	}
	return model.BlockCoord{X0: x0, Y0: y0, X1: x1, Y1: y1}
}

func inRect(x, y int, r model.BlockCoord) bool {
	return x >= r.X0 && x < r.X1 && y >= r.Y0 && y < r.Y1
}

func summaryCore(s *sample.Sample, b model.BlockCoord) model.PixelSummary {
	ch := s.Manifest.Channels
	width := s.Manifest.Width
	summary := model.PixelSummary{PixelCount: (b.X1 - b.X0) * (b.Y1 - b.Y0), MinLuma: 1 << 30}
	core := make([]byte, 0, summary.PixelCount*ch)
	for y := b.Y0; y < b.Y1; y++ {
		start := (y*width + b.X0) * ch
		end := (y*width + b.X1) * ch
		core = append(core, s.Recon[start:end]...)
		for x := b.X0; x < b.X1; x++ {
			off := (y*width + x) * ch
			for c := 0; c < ch; c++ {
				summary.ChannelSums[c] += int64(s.Recon[off+c])
			}
			luma := (int(s.Recon[off]) + 2*int(s.Recon[off+1]) + int(s.Recon[off+2])) / 4
			if luma < summary.MinLuma {
				summary.MinLuma = luma
			}
			if luma > summary.MaxLuma {
				summary.MaxLuma = luma
			}
		}
	}
	summary.SHA256 = model.BytesHash(core)
	if summary.MinLuma == 1<<30 {
		summary.MinLuma = 0
	}
	return summary
}

func EvaluateBlock(s *sample.Sample, blockIndex, band int) model.BlockRecord {
	m := s.Manifest
	b := m.Block(blockIndex)
	radius := m.WindowRadius
	requiredBand := colormetric.RequiredBand(radius)
	pr := paddedRect(m, b, band)
	pw := pr.X1 - pr.X0
	refBuf := paddedCopy(s.Ref, m.Width, m.Channels, pr)
	reconBuf := paddedCopy(s.Recon, m.Width, m.Channels, pr)

	accByLabel := make(map[int]*model.Accumulator)
	seamByLabel := make(map[int]*model.RegionSeamRecord)
	getAcc := func(label int) *model.Accumulator {
		if acc, ok := accByLabel[label]; ok {
			return acc
		}
		acc := &model.Accumulator{Label: label}
		accByLabel[label] = acc
		return acc
	}
	regionSeam := func(label int) *model.RegionSeamRecord {
		if seam, ok := seamByLabel[label]; ok {
			return seam
		}
		seam := &model.RegionSeamRecord{Label: label}
		seamByLabel[label] = seam
		return seam
	}

	quant := model.QuantRecord{
		Gear:           m.QuantGear,
		Bits:           m.QuantBits,
		Step:           sample.QuantStep(m.QuantBits),
		TruncationBits: model.FixedFractionBits,
	}
	seams := model.SeamRecord{}
	var missingEdges int
	var windowRemainderSum int64

	windowDistance := func(p model.Point) int64 {
		lx := p.X - pr.X0
		ly := p.Y - pr.Y0
		d, mark := colormetric.WindowDistanceQ8(refBuf, reconBuf, pw, pr.Y1-pr.Y0, m.Channels, lx, ly, radius, m.Metric)
		windowRemainderSum += mark.Remainder
		return d
	}
	jump := func(p1, p2 model.Point) int64 {
		d1 := windowDistance(p1)
		d2 := windowDistance(p2)
		v := d1 - d2
		if v < 0 {
			return -v
		}
		return v
	}

	for y := b.Y0; y < b.Y1; y++ {
		for x := b.X0; x < b.X1; x++ {
			globalIdx := y*m.Width + x
			label := int(s.Labels[globalIdx])
			acc := getAcc(label)
			d := windowDistance(model.Point{X: x, Y: y})
			acc.Area++
			acc.SumDistanceQ8 += d
			acc.SumSquaredQ16 += d * d
			quant.AfterMetricSumQ8 += d
			if d > quant.AfterMetricMaxQ8 {
				quant.AfterMetricMaxQ8 = d
			}

			off := globalIdx * m.Channels
			for c := 0; c < m.Channels; c++ {
				rv := int(s.Ref[off+c])
				vv := int(s.Recon[off+c])
				delta := rv - vv
				if delta < 0 {
					delta = -delta
				}
				quant.ErrorChannelSum += int64(delta)
				if delta > quant.ErrorChannelMax {
					quant.ErrorChannelMax = delta
				}
				qv := int(sample.QuantizeChannel(s.Ref[off+c], quant.Step))
				trunc := rv - qv
				if trunc < 0 {
					trunc = -trunc
				}
				quant.QuantTruncationSum += int64(trunc)
				quant.QuantTruncationMax = maxInt(quant.QuantTruncationMax, trunc)
			}
		}
	}
	quant.BeforeMetricSumQ8 = quant.ErrorChannelSum * model.FixedScale

	considerMaskEdge := func(p, n model.Point, vertical bool) {
		if !inRect(n.X, n.Y, pr) {
			missingEdges++
			return
		}
		lp := int(s.Labels[p.Y*m.Width+p.X])
		ln := int(s.Labels[n.Y*m.Width+n.X])
		if lp == ln {
			return
		}
		v := jump(p, n)
		acc := getAcc(lp)
		acc.BoundaryEdges++
		acc.BoundarySumQ8 += v
	}

	for y := b.Y0; y < b.Y1; y++ {
		for x := b.X0; x < b.X1; x++ {
			p := model.Point{X: x, Y: y}
			if x+1 < m.Width {
				considerMaskEdge(p, model.Point{X: x + 1, Y: y}, true)
			}
			if y+1 < m.Height {
				considerMaskEdge(p, model.Point{X: x, Y: y + 1}, false)
			}

			if x == b.X1-1 && b.X1 < m.Width {
				n := model.Point{X: x + 1, Y: y}
				if inRect(n.X, n.Y, pr) {
					v := jump(p, n)
					ownerLabel := int(s.Labels[y*m.Width+x])
					seams.VerticalEdges++
					seams.JumpSumQ8 += v
					if v > seams.JumpMaxQ8 {
						seams.JumpMaxQ8 = v
					}
					seams.VerticalJumpSumQ8 += v
					regionSeam(ownerLabel).VerticalEdges++
					regionSeam(ownerLabel).VerticalJumpSumQ8 += v
				} else {
					missingEdges++
				}
			}
			if y == b.Y1-1 && b.Y1 < m.Height {
				n := model.Point{X: x, Y: y + 1}
				if inRect(n.X, n.Y, pr) {
					v := jump(p, n)
					ownerLabel := int(s.Labels[y*m.Width+x])
					seams.HorizontalEdges++
					seams.JumpSumQ8 += v
					if v > seams.JumpMaxQ8 {
						seams.JumpMaxQ8 = v
					}
					seams.HorizontalJumpSumQ8 += v
					regionSeam(ownerLabel).HorizontalEdges++
					regionSeam(ownerLabel).HorizontalJumpSumQ8 += v
				} else {
					missingEdges++
				}
			}
		}
	}

	labels := make([]int, 0, len(accByLabel))
	accs := make([]model.Accumulator, 0, len(accByLabel))
	for label, acc := range accByLabel {
		labels = append(labels, label)
		accs = append(accs, *acc)
	}
	sort.Ints(labels)
	sort.Slice(accs, func(i, j int) bool { return accs[i].Label < accs[j].Label })
	regionSeams := make([]model.RegionSeamRecord, 0, len(seamByLabel))
	for _, label := range labels {
		if seam, ok := seamByLabel[label]; ok {
			regionSeams = append(regionSeams, *seam)
		}
	}

	truncations := []model.TruncationMark{
		{Stage: "color_distance_sqrt_q8_floor", Position: "colormetric.PixelDistanceQ8"},
		{Stage: "window_mean_div_count", Position: "colormetric.WindowDistanceQ8", Denominator: int64((2*radius + 1) * (2*radius + 1)), Remainder: windowRemainderSum},
	}
	needX0 := b.X0 - radius
	needY0 := b.Y0 - radius
	needX1 := b.X1 + radius + 1
	needY1 := b.Y1 + radius + 1
	if needX0 < 0 {
		needX0 = 0
	}
	if needY0 < 0 {
		needY0 = 0
	}
	if needX1 > m.Width {
		needX1 = m.Width
	}
	if needY1 > m.Height {
		needY1 = m.Height
	}
	bandSufficient := pr.X0 <= needX0 && pr.Y0 <= needY0 && pr.X1 >= needX1 && pr.Y1 >= needY1

	return model.BlockRecord{
		Block:          b,
		InputHash:      s.InputHash,
		ManifestHash:   s.ManifestHash,
		ParamVersion:   m.ParamVersion,
		AlgoVersion:    m.AlgoVersion,
		BandWidth:      band,
		RequiredBand:   requiredBand,
		Accumulators:   accs,
		RegionLabels:   labels,
		Quant:          quant,
		Seams:          seams,
		RegionSeams:    regionSeams,
		ReconSummary:   summaryCore(s, b),
		Truncations:    truncations,
		MissingEdges:   missingEdges,
		BandSufficient: bandSufficient,
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

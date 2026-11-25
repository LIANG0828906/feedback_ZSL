// Package reference is an independent, simple whole-plane reference
// implementation. It must not call the scheduler, engine chunk executor or
// checkpoint replay path; the review tool uses it to recompute evidence.
package reference

import (
	"fmt"

	ct "colortrace"
)

// Recompute evaluates every level using straightforward whole-plane loops.
func Recompute(project *ct.Project) ([]*ct.Sample, error) {
	samples := make([]*ct.Sample, 0, len(project.Stages))
	for stageIdx := range project.Stages {
		sample, err := evaluateStage(project, stageIdx, samples)
		if err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	return samples, nil
}

func evaluateStage(project *ct.Project, stageIdx int, prior []*ct.Sample) (*ct.Sample, error) {
	cfg := project.Stages[stageIdx]
	out := &ct.Sample{Stage: stageIdx, Planes: map[string]*ct.PlaneSample{}}
	for _, desc := range project.Planes {
		w, h := dims(project, desc.Name, stageIdx)
		ps := &ct.PlaneSample{
			Name: desc.Name, Width: w, Height: h,
			Values:  make([]int64, w*h),
			Codes:   make([]int64, w*h),
			Ledgers: make([]ct.PointLedger, w*h),
		}
		inW, inH := desc.Width, desc.Height
		amplitudes := append([]int64{}, desc.Values...)
		if stageIdx > 0 {
			inW, inH = dims(project, desc.Name, stageIdx-1)
			prev := prior[stageIdx-1].Planes[desc.Name]
			shift := uint(project.BaseBits - project.Stages[stageIdx-1].Bits)
			amplitudes = make([]int64, len(prev.Codes))
			for i, code := range prev.Codes {
				amplitudes[i] = code << shift
			}
		}
		grid := int64(1) << uint(project.BaseBits-cfg.Bits) * cfg.AccumScale
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				var priorSample *ct.Sample
				if stageIdx > 0 {
					priorSample = prior[stageIdx-1]
				}
				ledger := evaluatePoint(project, cfg, desc, amplitudes, inW, inH, x, y, grid, priorSample)
				idx := y*w + x
				ps.Values[idx] = ledger.OutputFixed
				ps.Codes[idx] = ledger.Code
				ps.Ledgers[idx] = ledger
			}
		}
		out.Planes[desc.Name] = ps
	}
	return out, nil
}

func evaluatePoint(project *ct.Project, cfg ct.StageConfig, desc *ct.Plane, amp []int64, inW, inH, x, y int, grid int64, priorSample *ct.Sample) ct.PointLedger {
	anchor := ct.Coord{X: x*cfg.FactorX + cfg.PhaseX, Y: y*cfg.FactorY + cfg.PhaseY}
	accC, accR := int64(0), int64(0)
	truncs := []ct.TruncationRecord{}
	coords := make([]ct.Coord, 0, len(cfg.Kernel.Taps))
	for _, tap := range cfg.Kernel.Taps {
		ix := clamp(anchor.X+tap.DX, 0, inW-1)
		iy := clamp(anchor.Y+tap.DY, 0, inH-1)
		coords = append(coords, ct.Coord{X: ix, Y: iy})
		raw := amp[iy*inW+ix] * cfg.AccumScale
		chroma := raw
		if desc.Chroma && cfg.ChromaKernel != nil {
			cAcc := int64(0)
			for _, ctap := range cfg.ChromaKernel.Taps {
				cx := clamp(ix+ctap.DX, 0, inW-1)
				cy := clamp(iy+ctap.DY, 0, inH-1)
				cAcc += ctap.W * amp[cy*inW+cx] * cfg.AccumScale
			}
			cq := cAcc / cfg.ChromaKernel.NormScale
			cr := cAcc % cfg.ChromaKernel.NormScale
			chroma = cq
			if cr != 0 {
				truncs = append(truncs, ct.TruncationRecord{
					Stage: cfg.Index, Plane: desc.Name, Coord: ct.Coord{X: x, Y: y},
					Kind: "chroma_resample", RawValue: cAcc, Divisor: cfg.ChromaKernel.NormScale,
					Quotient: cq, Remainder: cr, Discarded: ct.Rat(cr, cfg.ChromaKernel.NormScale),
					RoundingDelta: cq - raw,
				})
			}
		}
		accC += tap.W * chroma
		accR += tap.W * raw
	}
	ws := cfg.Kernel.WeightSum()
	ns := cfg.Kernel.NormScale
	rawRef := ct.Rat(accR, ws)
	bd := ct.OperatorBreakdown{
		Chroma:       ct.SubFrac(ct.Rat(accC, ws), rawRef),
		ResampleNorm: ct.SubFrac(ct.Rat(accC, ns), ct.Rat(accC, ws)),
	}
	q := accC / ns
	r := accC % ns
	bd.AccumTrunc = ct.Rat(-r, ns)
	code := (q + grid/2) / grid
	maxCode := int64(1)<<uint(cfg.Bits) - 1
	if code < 0 {
		code = 0
	}
	if code > maxCode {
		code = maxCode
	}
	output := code * grid
	bd.BitRound = ct.I64(output - q)
	propagated := ct.I64(0)
	if cfg.Index > 0 && priorSample != nil {
		prevPlane := priorSample.Planes[desc.Name]
		prevScale := ct.Rat(cfg.AccumScale, project.Stages[cfg.Index-1].AccumScale)
		for i, tap := range cfg.Kernel.Taps {
			prevLedger := prevPlane.LedgerAt(coords[i].X, coords[i].Y)
			scaled := ct.MulFrac(prevLedger.CumulativeDeviation, prevScale)
			propagated = ct.AddFrac(propagated, ct.MulFrac(scaled, ct.Rat(tap.W, ws)))
		}
	}
	cumulative := ct.AddFrac(propagated, bd.Sum())
	chunk := ct.ChunkCoordAt(x, y, project.ChunkSize)
	return ct.PointLedger{
		Stage: cfg.Index, Plane: desc.Name, Coord: ct.Coord{X: x, Y: y},
		Chunk: chunk, ChunkID: ct.ChunkID(chunk), Anchor: anchor,
		AccumScale: cfg.AccumScale, FactorX: cfg.FactorX, FactorY: cfg.FactorY,
		Bits: cfg.Bits, KernelRadiusX: cfg.Kernel.RadiusX, KernelRadiusY: cfg.Kernel.RadiusY,
		WeightSum: ws, NormScale: ns, RawWeightedSum: accC, Remainder: r,
		PreQuantFixed: q, QuantGrid: grid, Code: code, OutputFixed: output,
		RawReferenceFixed: rawRef, TotalDeviation: bd.Sum(), Breakdown: bd,
		CumulativeDeviation: cumulative,
		Truncations:         truncs,
	}
}

// FindFirstBand independently scans horizontal then vertical adjacent pairs.
func FindFirstBand(project *ct.Project, samples []*ct.Sample) (*ct.BandReport, error) {
	for stageIdx, sample := range samples {
		for _, desc := range project.Planes {
			plane := sample.Planes[desc.Name]
			for y := 0; y < plane.Height; y++ {
				for x := 0; x+1 < plane.Width; x++ {
					if jump := abs(plane.Values[y*plane.Width+x] - plane.Values[y*plane.Width+x+1]); jump > project.BandTolerance {
						return bandReport(project, stageIdx, plane, x, y, x+1, y, jump), nil
					}
				}
			}
			for y := 0; y+1 < plane.Height; y++ {
				for x := 0; x < plane.Width; x++ {
					if jump := abs(plane.Values[y*plane.Width+x] - plane.Values[(y+1)*plane.Width+x]); jump > project.BandTolerance {
						return bandReport(project, stageIdx, plane, x, y, x, y+1, jump), nil
					}
				}
			}
		}
	}
	return nil, nil
}

func bandReport(project *ct.Project, stage int, plane *ct.PlaneSample, x, y, nx, ny int, jump int64) *ct.BandReport {
	ledger := plane.Ledgers[y*plane.Width+x]
	shares := OperatorShares(ledger.Breakdown)
	dominant, ambiguous := Dominant(shares, project.DominantShareThreshold)
	return &ct.BandReport{
		Stage: stage, Plane: plane.Name, Coord: ct.Coord{X: x, Y: y},
		Neighbor: ct.Coord{X: nx, Y: ny}, Chunk: ledger.Chunk, ChunkID: ledger.ChunkID,
		Jump: jump, Tolerance: project.BandTolerance, DominantOperator: dominant,
		Breakdown: ledger.Breakdown, Shares: shares, Ambiguous: ambiguous,
	}
}

// OperatorShares normalizes absolute operator contributions.
func OperatorShares(b ct.OperatorBreakdown) []ct.Share {
	items := []struct {
		name string
		f    ct.Fraction
	}{
		{"resample_norm", b.ResampleNorm},
		{"accum_trunc", b.AccumTrunc},
		{"bit_round", b.BitRound},
		{"chroma", b.Chroma},
	}
	total := ct.I64(0)
	abs := make([]ct.Fraction, 4)
	for i, item := range items {
		abs[i] = ct.AbsFrac(item.f)
		total = ct.AddFrac(total, abs[i])
	}
	shares := make([]ct.Share, 0, 4)
	for i, item := range items {
		share := 0.0
		if total.Num != 0 {
			share = float64(abs[i].Num*total.Den) / float64(abs[i].Den*total.Num)
		}
		shares = append(shares, ct.Share{
			Operator: item.name, Share: share,
			AbsNum: item.f.Num, AbsDen: item.f.Den,
		})
	}
	return shares
}

// Dominant returns the leading operator only if its share reaches threshold.
func Dominant(shares []ct.Share, threshold float64) (string, bool) {
	top := shares[0]
	for _, share := range shares[1:] {
		if share.Share > top.Share {
			top = share
		}
	}
	if top.Share+1e-12 >= threshold {
		return top.Operator, false
	}
	return top.Operator, true
}

// BuildTruth constructs and fingerprints the analytic truth manifest.
func BuildTruth(project *ct.Project) (*ct.TruthManifest, []*ct.Sample, error) {
	samples, err := Recompute(project)
	if err != nil {
		return nil, nil, err
	}
	band, err := FindFirstBand(project, samples)
	if err != nil {
		return nil, nil, err
	}
	truth := &ct.TruthManifest{Seed: project.Seed, Points: []ct.TruthPoint{}}
	for _, query := range project.TracePoints {
		plane := project.Plane(query.Plane)
		if plane == nil {
			return nil, nil, &ct.Error{Code: ct.ESchema, Path: "$.trace_points", Message: "trace plane missing"}
		}
		point := ct.TruthPoint{Plane: query.Plane, Source: ct.Coord{X: query.X, Y: query.Y}}
		cx, cy := query.X, query.Y
		for stageIdx := range project.Stages {
			cx /= project.Stages[stageIdx].FactorX
			cy /= project.Stages[stageIdx].FactorY
			ps := samples[stageIdx].Planes[query.Plane]
			if cx < 0 || cx >= ps.Width || cy < 0 || cy >= ps.Height {
				return nil, nil, fmt.Errorf("truth point maps outside stage %d", stageIdx)
			}
			ledger := ps.LedgerAt(cx, cy)
			point.Stages = append(point.Stages, ct.TruthStageSample{
				Stage: stageIdx, Coord: ct.Coord{X: cx, Y: cy}, Chunk: ledger.Chunk,
				ExpectedCode: ledger.Code, ExpectedFixed: ledger.OutputFixed,
				ExpectedDeviation: ledger.TotalDeviation, Breakdown: ledger.Breakdown,
				ExpectedCumulative: ledger.CumulativeDeviation,
			})
		}
		truth.Points = append(truth.Points, point)
	}
	if band != nil {
		truth.Band = ct.TruthBand{
			Stage: band.Stage, Plane: band.Plane, Coord: band.Coord, Neighbor: band.Neighbor,
			Chunk: band.Chunk, DominantOperator: band.DominantOperator,
		}
	}
	truth.Fingerprint = ct.HashCanonical("truth-manifest-v1", truth)
	return truth, samples, nil
}

func dims(project *ct.Project, plane string, stageIdx int) (int, int) {
	desc := project.Plane(plane)
	w, h := desc.Width, desc.Height
	for i := 0; i <= stageIdx; i++ {
		w /= project.Stages[i].FactorX
		h /= project.Stages[i].FactorY
	}
	return w, h
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

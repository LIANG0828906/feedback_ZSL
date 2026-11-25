package colortrace

// OperatorShares computes normalized absolute contribution shares.
func OperatorShares(breakdown OperatorBreakdown) []Share {
	items := []struct {
		name string
		f    Fraction
	}{
		{"resample_norm", breakdown.ResampleNorm},
		{"accum_trunc", breakdown.AccumTrunc},
		{"bit_round", breakdown.BitRound},
		{"chroma", breakdown.Chroma},
	}
	total := I64(0)
	absValues := make([]Fraction, len(items))
	for i, item := range items {
		absValues[i] = AbsFrac(item.f)
		total = AddFrac(total, absValues[i])
	}
	shares := make([]Share, 0, len(items))
	for i, item := range items {
		share := 0.0
		if total.Num != 0 {
			share = float64(absValues[i].Num*total.Den) / float64(absValues[i].Den*total.Num)
		}
		shares = append(shares, Share{
			Operator: item.name, Share: share,
			AbsNum: item.f.Num, AbsDen: item.f.Den,
		})
	}
	return shares
}

// DominantOperator returns the leading operator when its share is decisive.
func DominantOperator(shares []Share, threshold float64) (name string, ambiguous bool) {
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

// FindFirstBand locates the first stage/plane/row-major adjacent jump over
// tolerance. Horizontal pairs are scanned before vertical pairs.
func FindFirstBand(project *Project, samples []*Sample) (*BandReport, error) {
	if project == nil {
		return nil, schemaError("$", "nil project")
	}
	for stageIdx, sample := range samples {
		if sample == nil {
			return nil, schemaError("samples", "missing stage %d sample", stageIdx)
		}
		for _, desc := range project.Planes {
			plane := sample.Planes[desc.Name]
			if plane == nil {
				return nil, schemaError("samples", "plane %q missing at stage %d", desc.Name, stageIdx)
			}
			for y := 0; y < plane.Height; y++ {
				for x := 0; x+1 < plane.Width; x++ {
					idx := y*plane.Width + x
					jump := abs64Int(plane.Values[idx] - plane.Values[idx+1])
					if jump > project.BandTolerance {
						return newBandReport(project, stageIdx, plane, x, y, x+1, y, jump), nil
					}
				}
			}
			for y := 0; y+1 < plane.Height; y++ {
				for x := 0; x < plane.Width; x++ {
					idx := y*plane.Width + x
					below := (y+1)*plane.Width + x
					jump := abs64Int(plane.Values[idx] - plane.Values[below])
					if jump > project.BandTolerance {
						return newBandReport(project, stageIdx, plane, x, y, x, y+1, jump), nil
					}
				}
			}
		}
	}
	return nil, nil
}

// AnalyzeFirstBand finds and classifies the first band. A non-decisive
// attribution returns both report and an E_AMBIG error with observed shares.
func AnalyzeFirstBand(project *Project, samples []*Sample) (*BandReport, error) {
	report, err := FindFirstBand(project, samples)
	if err != nil || report == nil {
		return report, err
	}
	if report.Ambiguous {
		return report, &Error{
			Code: EAmbig, Stage: report.Stage, HasStage: true, Chunk: report.ChunkID,
			Coord: &report.Coord, Shares: report.Shares,
			Message: "competing operator shares prevent a single-operator attribution",
		}
	}
	return report, nil
}

// ClassifyAttribution builds a report from an explicit breakdown and returns
// E_AMBIG when no operator share is decisive.
func ClassifyAttribution(stage int, plane, chunkID string, coord Coord, jump, tolerance int64, breakdown OperatorBreakdown, threshold float64) (*BandReport, error) {
	shares := OperatorShares(breakdown)
	dominant, ambiguous := DominantOperator(shares, threshold)
	report := &BandReport{
		Stage: stage, Plane: plane, Coord: coord, ChunkID: chunkID,
		Jump: jump, Tolerance: tolerance, DominantOperator: dominant,
		Breakdown: breakdown, Shares: shares, Ambiguous: ambiguous,
	}
	if ambiguous {
		return report, &Error{
			Code: EAmbig, Stage: stage, HasStage: true, Chunk: chunkID, Coord: &coord,
			Shares: shares, Message: "competing operator shares prevent a single-operator attribution",
		}
	}
	return report, nil
}

func newBandReport(project *Project, stage int, plane *PlaneSample, x, y, nx, ny int, jump int64) *BandReport {
	ledger := plane.LedgerAt(x, y)
	breakdown := OperatorBreakdown{}
	chunk := ChunkCoordAt(x, y, project.ChunkSize)
	chunkID := ChunkID(chunk)
	if ledger != nil {
		breakdown = ledger.Breakdown
		chunk = ledger.Chunk
		chunkID = ledger.ChunkID
	}
	shares := OperatorShares(breakdown)
	dominant, ambiguous := DominantOperator(shares, project.DominantShareThreshold)
	return &BandReport{
		Stage: stage, Plane: plane.Name, Coord: Coord{X: x, Y: y},
		Neighbor: Coord{X: nx, Y: ny}, Chunk: chunk, ChunkID: chunkID,
		Jump: jump, Tolerance: project.BandTolerance,
		DominantOperator: dominant, Breakdown: breakdown, Shares: shares,
		Ambiguous: ambiguous,
	}
}

func abs64Int(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// EqualShareForTest is a small helper used by ambiguity coverage.

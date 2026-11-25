package colortrace

import "fmt"

// BoundCell is a coordinate's cumulative signed error envelope.
type BoundCell struct {
	Detail StageBound `json:"detail"`
	Low    Fraction   `json:"low"`
	High   Fraction   `json:"high"`
}

// BoundTable indexes stage -> plane -> row-major bound cells.
type BoundTable map[int]map[string][]BoundCell

// At returns a cell.
func (t BoundTable) At(stage int, plane string, x, y, width int) *BoundCell {
	planeGrid := t[stage]
	if planeGrid == nil {
		return nil
	}
	cells := planeGrid[plane]
	idx := y*width + x
	if idx < 0 || idx >= len(cells) {
		return nil
	}
	return &cells[idx]
}

// ComputeBoundTable derives and accumulates every point's envelope from
// observed configuration and values only.
func ComputeBoundTable(project *Project, samples []*Sample) (BoundTable, error) {
	table := BoundTable{}
	for stageIdx, sample := range samples {
		cfg := project.Stages[stageIdx]
		table[stageIdx] = map[string][]BoundCell{}
		var input *Sample
		if stageIdx > 0 {
			input = samples[stageIdx-1]
		}
		for _, desc := range project.Planes {
			plane := sample.Planes[desc.Name]
			if plane == nil {
				return nil, schemaError("samples", "missing plane %s at stage %d", desc.Name, stageIdx)
			}
			amplitudes, err := baseAmplitudes(project, stageIdx, desc.Name, input)
			if err != nil {
				return nil, err
			}
			inW, inH := desc.Width, desc.Height
			if stageIdx > 0 {
				inW, inH, err = StagePlaneDimensions(project, stageIdx-1, desc.Name)
				if err != nil {
					return nil, err
				}
			}
			cells := make([]BoundCell, plane.Width*plane.Height)
			for y := 0; y < plane.Height; y++ {
				for x := 0; x < plane.Width; x++ {
					ledger := plane.LedgerAt(x, y)
					if ledger == nil {
						return nil, schemaError("ledger", "missing ledger stage=%d coord=(%d,%d)", stageIdx, x, y)
					}
					cell, err := computeCell(project, cfg, desc, ledger, amplitudes, inW, inH, table, stageIdx, x, y)
					if err != nil {
						return nil, err
					}
					cells[y*plane.Width+x] = cell
					if err := requireInside(ledger.CumulativeDeviation, cell.Low, cell.High, stageIdx, Coord{X: x, Y: y}, "measured cumulative deviation"); err != nil {
						return nil, err
					}
				}
			}
			table[stageIdx][desc.Name] = cells
		}
	}
	return table, nil
}

func computeCell(project *Project, cfg StageConfig, desc *Plane, ledger *PointLedger, amplitudes []int64, inW, inH int, table BoundTable, stageIdx, x, y int) (BoundCell, error) {
	weightSum := cfg.Kernel.WeightSum()
	weightDiff := weightSum - cfg.Kernel.NormScale
	normBound := I64(0)
	if weightDiff != 0 {
		normBound = MulFrac(I64(abs64Int(ledger.RawWeightedSum)), Rat(abs64Int(weightDiff), cfg.Kernel.NormScale*weightSum))
	}
	accumBound := Rat(cfg.Kernel.NormScale-1, cfg.Kernel.NormScale)
	bitBound := Rat(ledger.QuantGrid, 2)
	chromaBound := observedChromaBound(cfg, desc, ledger.Anchor, amplitudes, inW, inH)
	local := SumFracs(normBound, accumBound, bitBound, chromaBound)

	propLow, propHigh := I64(0), I64(0)
	if stageIdx > 0 {
		prevCfg := project.Stages[stageIdx-1]
		prevPlane := table[stageIdx-1][desc.Name]
		prevW, _, err := StagePlaneDimensions(project, stageIdx-1, desc.Name)
		if err != nil {
			return BoundCell{}, err
		}
		scale := Rat(cfg.AccumScale, prevCfg.AccumScale)
		for _, tap := range cfg.Kernel.Taps {
			ix := clampInt(ledger.Anchor.X+tap.DX, 0, inW-1)
			iy := clampInt(ledger.Anchor.Y+tap.DY, 0, inH-1)
			prev := &prevPlane[iy*prevW+ix]
			weight := Rat(tap.W, weightSum)
			propLow = AddFrac(propLow, MulFrac(MulFrac(prev.Low, scale), weight))
			propHigh = AddFrac(propHigh, MulFrac(MulFrac(prev.High, scale), weight))
		}
	}
	low := SubFrac(propLow, local)
	high := AddFrac(propHigh, local)
	return BoundCell{
		Low: low, High: high,
		Detail: StageBound{
			Stage: stageIdx, FactorX: cfg.FactorX, FactorY: cfg.FactorY,
			AccumScale: cfg.AccumScale, Bits: cfg.Bits,
			RadiusX: cfg.Kernel.RadiusX, RadiusY: cfg.Kernel.RadiusY,
			ResampleNorm: normBound, AccumTrunc: accumBound, BitRound: bitBound,
			Chroma: chromaBound, LocalMagnitude: local,
			PropagatedLow: propLow, PropagatedHigh: propHigh,
			Low: low, High: high,
		},
	}, nil
}

func observedChromaBound(cfg StageConfig, desc *Plane, anchor Coord, amplitudes []int64, inW, inH int) Fraction {
	if !desc.Chroma || cfg.ChromaKernel == nil {
		return I64(0)
	}
	rx := cfg.Kernel.RadiusX + cfg.ChromaKernel.RadiusX
	ry := cfg.Kernel.RadiusY + cfg.ChromaKernel.RadiusY
	minX := clampInt(anchor.X-rx, 0, inW-1)
	maxX := clampInt(anchor.X+rx, 0, inW-1)
	minY := clampInt(anchor.Y-ry, 0, inH-1)
	maxY := clampInt(anchor.Y+ry, 0, inH-1)
	minValue, maxValue := amplitudes[minY*inW+minX], amplitudes[minY*inW+minX]
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			value := amplitudes[y*inW+x]
			if value < minValue {
				minValue = value
			}
			if value > maxValue {
				maxValue = value
			}
		}
	}
	return I64((maxValue - minValue) * cfg.AccumScale)
}

// VerifyTruthBounds checks truth expected cumulative deviations and returns
// one report per truth point/stage.
func VerifyTruthBounds(project *Project, samples []*Sample, truth *TruthManifest) ([]*BoundReport, error) {
	table, err := ComputeBoundTable(project, samples)
	if err != nil {
		return nil, err
	}
	if truth == nil {
		return nil, &Error{Code: EBound, Message: "truth manifest is nil"}
	}
	reports := []*BoundReport{}
	for _, point := range truth.Points {
		desc := project.Plane(point.Plane)
		if desc == nil {
			return nil, schemaError("$.truth.points", "unknown plane %q", point.Plane)
		}
		for _, expected := range point.Stages {
			w, _, err := StagePlaneDimensions(project, expected.Stage, point.Plane)
			if err != nil {
				return nil, err
			}
			cell := table.At(expected.Stage, point.Plane, expected.Coord.X, expected.Coord.Y, w)
			if cell == nil {
				return nil, &Error{Code: EBound, Stage: expected.Stage, HasStage: true, Coord: &expected.Coord, Message: "bound cell missing"}
			}
			measured := expected.ExpectedCumulative
			if sample := samples[expected.Stage].Planes[point.Plane]; sample != nil {
				if ledger := sample.LedgerAt(expected.Coord.X, expected.Coord.Y); ledger != nil {
					measured = ledger.CumulativeDeviation
				}
			}
			if err := requireInside(measured, cell.Low, cell.High, expected.Stage, expected.Coord, "measured cumulative deviation"); err != nil {
				return nil, err
			}
			if err := requireInside(expected.ExpectedCumulative, cell.Low, cell.High, expected.Stage, expected.Coord, "truth expected cumulative deviation"); err != nil {
				return nil, err
			}
			if (expected.ExpectedCumulative.Num > 0 && CmpFrac(cell.High, I64(0)) < 0) ||
				(expected.ExpectedCumulative.Num < 0 && CmpFrac(cell.Low, I64(0)) > 0) {
				return nil, &Error{
					Code: EBound, Stage: expected.Stage, HasStage: true, Coord: &expected.Coord,
					Message: fmt.Sprintf("bound sign conflicts with truth deviation %s", expected.ExpectedCumulative),
				}
			}
			stageBounds := []StageBound{cell.Detail}
			reports = append(reports, &BoundReport{
				Stage: expected.Stage, Plane: point.Plane, Coord: expected.Coord,
				Measured: measured, TruthExpected: expected.ExpectedCumulative,
				Low: cell.Low, High: cell.High, CumulativeMagnitude: cell.High,
				StageBounds: stageBounds, OK: true,
			})
		}
	}
	return reports, nil
}

func requireInside(value, low, high Fraction, stage int, coord Coord, label string) error {
	if CmpFrac(value, low) < 0 || CmpFrac(value, high) > 0 {
		return &Error{
			Code: EBound, Stage: stage, HasStage: true, Coord: &coord,
			Message: fmt.Sprintf("%s=%s outside observed bound [%s,%s]", label, value, low, high),
		}
	}
	return nil
}

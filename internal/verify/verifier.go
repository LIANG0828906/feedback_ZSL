package verify

import (
	"context"
	"math/bits"

	"semanticshading/internal/checkpoint"
	"semanticshading/internal/model"
)

type Mismatch struct {
	Code       string           `json:"code"`
	RegionID   int              `json:"region_id"`
	BlockIndex int              `json:"block_index"`
	Block      model.BlockCoord `json:"block"`
	Field      string           `json:"field"`
	Expected   any              `json:"expected,omitempty"`
	Actual     any              `json:"actual,omitempty"`
	Message    string           `json:"message"`
}

type RegionResult struct {
	Metrics             model.RegionMetrics `json:"metrics"`
	Bound               model.ErrorBound    `json:"bound"`
	MeasuredPropagation int64               `json:"measured_propagation_q8"`
}

type Report struct {
	OK               bool           `json:"ok"`
	TruthFingerprint string         `json:"truth_fingerprint"`
	BlockCount       int            `json:"block_count"`
	CommitSequence   []int          `json:"commit_sequence"`
	Regions          []RegionResult `json:"regions"`
	Mismatches       []Mismatch     `json:"mismatches,omitempty"`
	First            *Mismatch      `json:"first,omitempty"`
}

type independentAcc struct {
	area          int64
	sumD          int64
	sumD2         int64
	boundaryEdges int64
	boundarySum   int64
	blocks        []int
}

func isqrt(n int64) int64 {
	if n <= 0 {
		return 0
	}
	x := int64(1) << uint((bits.Len64(uint64(n))+1)/2)
	for {
		y := (x + n/x) / 2
		if y >= x {
			return x
		}
		x = y
	}
}

func stdQ8(sumD, sumD2, area int64) int64 {
	if area <= 0 {
		return 0
	}
	mean := sumD / area
	second := sumD2 / area
	variance := second - mean*mean
	if variance < 0 {
		variance = 0
	}
	return isqrt(variance)
}

func boundFor(gear, bits, tile, fracBits, label int) model.ErrorBound {
	scale := int64(1) << fracBits
	if bits < 1 {
		bits = 1
	}
	if tile <= 0 {
		tile = 1
	}
	step := 256 / (1 << bits)
	if step < 1 {
		step = 1
	}
	maxSq := int64(3*255*255) * scale * scale
	maxD := isqrt(maxSq)
	quant := int64(step) * scale
	gearTerm := int64(gear) * scale / 8
	tileTerm := (256 / int64(tile)) * scale / 16
	trunc := int64(1) << (fracBits / 2)
	return model.ErrorBound{
		RegionLabel:        label,
		MeanUpperQ8:        maxD + quant/2 + gearTerm + trunc,
		StdUpperQ8:         maxD + quant/4 + gearTerm + trunc*4,
		BoundaryUpperQ8:    maxD + quant + trunc*2,
		PropagationUpperQ8: maxD + quant + tileTerm + trunc*4,
		QuantGear:          gear,
		QuantBits:          bits,
		TileSize:           tile,
		FixedFractionBits:  fracBits,
	}
}

func blockCoord(truth model.Truth, index int) model.BlockCoord {
	cols := truth.Width / truth.TileSize
	bx := index % cols
	by := index / cols
	t := truth.TileSize
	return model.BlockCoord{Index: index, BX: bx, BY: by, X0: bx * t, Y0: by * t, X1: (bx + 1) * t, Y1: (by + 1) * t}
}

func expectedBlockArea(region model.RegionTruth, block model.BlockCoord) int64 {
	var area int64
	for _, run := range region.MaskRuns {
		if run.Y < block.Y0 || run.Y >= block.Y1 {
			continue
		}
		x0 := maxInt(run.X0, block.X0)
		x1 := minInt(run.X1, block.X1)
		if x1 > x0 {
			area += int64(x1 - x0)
		}
	}
	return area
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func Verify(ctx context.Context, store *checkpoint.Store, truth model.Truth) (*Report, error) {
	checkpoints, err := store.LoadCheckpoints()
	if err != nil {
		return nil, err
	}
	sequence, err := store.CommitSequence()
	if err != nil {
		return nil, err
	}
	report := &Report{OK: true, TruthFingerprint: truth.TruthFingerprint, CommitSequence: sequence}
	addMismatch := func(m Mismatch) {
		report.OK = false
		report.Mismatches = append(report.Mismatches, m)
		if report.First == nil {
			copyM := m
			report.First = &copyM
		}
	}

	if id := store.Identity(); id.TruthFingerprint != truth.TruthFingerprint {
		addMismatch(Mismatch{Code: "E_SCHEMA", RegionID: -1, Field: "truth_fingerprint", Expected: truth.TruthFingerprint, Actual: id.TruthFingerprint, Message: "persisted truth fingerprint does not match supplied truth"})
	}
	expectedBlocks := truth.Width / truth.TileSize * truth.Height / truth.TileSize
	report.BlockCount = len(checkpoints)
	if len(checkpoints) != expectedBlocks {
		idx := len(checkpoints)
		addMismatch(Mismatch{Code: "E_STATE", RegionID: -1, BlockIndex: idx, Block: blockCoord(truth, minInt(idx, expectedBlocks-1)), Field: "block_count", Expected: expectedBlocks, Actual: len(checkpoints), Message: "persisted block count does not match truth grid"})
	}
	for i, cp := range checkpoints {
		if cp.Block.Block.Index != i || cp.CommitOrder != i {
			bc := blockCoord(truth, i)
			addMismatch(Mismatch{Code: "E_STATE", RegionID: -1, BlockIndex: i, Block: bc, Field: "commit_order", Expected: i, Actual: cp.CommitOrder, Message: "block commit order is not stable"})
		}
		if cp.InputHash != store.Identity().InputHash || cp.ParamVersion != model.ParamVersion || cp.AlgoVersion != model.AlgorithmVersion {
			addMismatch(Mismatch{Code: "E_SCHEMA", RegionID: -1, BlockIndex: cp.Block.Block.Index, Block: cp.Block.Block, Field: "versions", Expected: store.Identity(), Actual: map[string]any{"input_hash": cp.InputHash, "param_version": cp.ParamVersion, "algo_version": cp.AlgoVersion}, Message: "checkpoint identity or version mismatch"})
		}
	}

	accs := map[int]*independentAcc{}
	propagation := map[int]int64{}
	truthByLabel := map[int]model.RegionTruth{}
	for _, region := range truth.Regions {
		truthByLabel[region.Label] = region
	}
	recordByIndex := map[int]model.BlockRecord{}
	for _, cp := range checkpoints {
		recordByIndex[cp.Block.Block.Index] = cp.Block
	}

	for _, cp := range checkpoints {
		record := cp.Block
		for _, persisted := range record.Accumulators {
			regionTruth, ok := truthByLabel[persisted.Label]
			if !ok {
				addMismatch(Mismatch{Code: "E_LABEL", RegionID: persisted.Label, BlockIndex: record.Block.Index, Block: record.Block, Field: "label", Message: "persisted label is absent from truth enum"})
				continue
			}
			expectedArea := expectedBlockArea(regionTruth, record.Block)
			if expectedArea != persisted.Area {
				addMismatch(Mismatch{Code: "E_DECODE", RegionID: persisted.Label, BlockIndex: record.Block.Index, Block: record.Block, Field: "area", Expected: expectedArea, Actual: persisted.Area, Message: "persisted block area does not match truth mask"})
			}
			acc := accs[persisted.Label]
			if acc == nil {
				acc = &independentAcc{}
				accs[persisted.Label] = acc
			}
			acc.area += persisted.Area
			acc.sumD += persisted.SumDistanceQ8
			acc.sumD2 += persisted.SumSquaredQ16
			acc.boundaryEdges += persisted.BoundaryEdges
			acc.boundarySum += persisted.BoundarySumQ8
			acc.blocks = append(acc.blocks, record.Block.Index)
		}

		for _, bound := range cp.Bounds {
			expected := boundFor(record.Quant.Gear, record.Quant.Bits, record.Block.X1-record.Block.X0, record.Quant.TruncationBits, bound.RegionLabel)
			if bound != expected {
				addMismatch(Mismatch{Code: "E_BOUND", RegionID: bound.RegionLabel, BlockIndex: record.Block.Index, Block: record.Block, Field: "error_bound", Expected: expected, Actual: bound, Message: "persisted error bound does not match independent derivation"})
			}
		}

		cols := truth.Width / truth.TileSize
		for _, regionSeam := range record.RegionSeams {
			if regionSeam.VerticalEdges > 0 {
				next := record.Block.Index + 1
				if other, ok := recordByIndex[next]; ok && other.Block.BY == record.Block.BY && containsInt(other.RegionLabels, regionSeam.Label) {
					avg := regionSeam.VerticalJumpSumQ8 / regionSeam.VerticalEdges
					if avg > propagation[regionSeam.Label] {
						propagation[regionSeam.Label] = avg
					}
				}
			}
			if regionSeam.HorizontalEdges > 0 {
				next := record.Block.Index + cols
				if other, ok := recordByIndex[next]; ok && other.Block.BX == record.Block.BX && containsInt(other.RegionLabels, regionSeam.Label) {
					avg := regionSeam.HorizontalJumpSumQ8 / regionSeam.HorizontalEdges
					if avg > propagation[regionSeam.Label] {
						propagation[regionSeam.Label] = avg
					}
				}
			}
		}
	}

	labels := make([]int, 0, len(accs))
	for label := range accs {
		labels = append(labels, label)
	}
	insertionSortInts(labels)

	for _, label := range labels {
		acc := accs[label]
		mean := int64(0)
		if acc.area > 0 {
			mean = acc.sumD / acc.area
		}
		boundary := int64(0)
		if acc.boundaryEdges > 0 {
			boundary = acc.boundarySum / acc.boundaryEdges
		}
		std := stdQ8(acc.sumD, acc.sumD2, acc.area)
		firstBlock := 0
		if len(acc.blocks) > 0 {
			firstBlock = acc.blocks[0]
		}
		bc := blockCoord(truth, firstBlock)
		truthRegion, hasTruth := truthByLabel[label]
		metrics := model.RegionMetrics{Label: label, Area: acc.area, MeanDistanceQ8: mean, StdDistanceQ8: std, BoundaryJumpQ8: boundary, BoundaryEdges: acc.boundaryEdges, BlockIndices: acc.blocks}
		bound := boundFor(truth.QuantGear, truth.QuantBits, truth.TileSize, model.FixedFractionBits, label)
		result := RegionResult{Metrics: metrics, Bound: bound, MeasuredPropagation: propagation[label]}
		report.Regions = append(report.Regions, result)

		if !hasTruth {
			addMismatch(Mismatch{Code: "E_SCHEMA", RegionID: label, BlockIndex: firstBlock, Block: bc, Field: "region_truth", Message: "truth region missing"})
			continue
		}
		if diffInt64(acc.area, int64(truthRegion.Area)) > int64(truth.Tolerance.AreaAbs) {
			addMismatch(Mismatch{Code: "E_DECODE", RegionID: label, BlockIndex: firstBlock, Block: bc, Field: "area", Expected: truthRegion.Area, Actual: acc.area, Message: "region area exceeds tolerance"})
		}
		if diffInt64(mean, truthRegion.ExpectedMeanQ8) > truth.Tolerance.MeanAbsQ8 {
			addMismatch(Mismatch{Code: "E_DECODE", RegionID: label, BlockIndex: firstBlock, Block: bc, Field: "mean_distance_q8", Expected: truthRegion.ExpectedMeanQ8, Actual: mean, Message: "mean color difference exceeds tolerance"})
		}
		if diffInt64(std, truthRegion.ExpectedStdQ8) > truth.Tolerance.StdAbsQ8 {
			addMismatch(Mismatch{Code: "E_DECODE", RegionID: label, BlockIndex: firstBlock, Block: bc, Field: "std_distance_q8", Expected: truthRegion.ExpectedStdQ8, Actual: std, Message: "standard deviation exceeds tolerance"})
		}
		if diffInt64(boundary, truthRegion.ExpectedBoundaryQ8) > truth.Tolerance.BoundaryAbsQ8 {
			addMismatch(Mismatch{Code: "E_BAND", RegionID: label, BlockIndex: firstBlock, Block: bc, Field: "boundary_jump_q8", Expected: truthRegion.ExpectedBoundaryQ8, Actual: boundary, Message: "mask boundary jump exceeds tolerance"})
		}
		measured := [4]int64{mean, std, boundary, propagation[label]}
		upper := [4]int64{bound.MeanUpperQ8, bound.StdUpperQ8, bound.BoundaryUpperQ8, bound.PropagationUpperQ8}
		fields := [4]string{"mean_q8", "std_q8", "boundary_jump_q8", "propagation_q8"}
		for i := range measured {
			if measured[i] > upper[i] {
				addMismatch(Mismatch{Code: "E_BOUND", RegionID: label, BlockIndex: firstBlock, Block: bc, Field: fields[i], Expected: upper[i], Actual: measured[i], Message: "measured value exceeds independently derived bound"})
			}
		}
	}
	return report, ctx.Err()
}

func diffInt64(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

func intersect(a, b []int) []int {
	set := map[int]bool{}
	for _, v := range a {
		set[v] = true
	}
	out := []int{}
	for _, v := range b {
		if set[v] {
			out = append(out, v)
		}
	}
	insertionSortInts(out)
	return out
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func insertionSortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}

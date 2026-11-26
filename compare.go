package quantforensics

import "fmt"

type Inconsistency struct {
	BlockID    string          `json:"blockId"`
	Coordinate BlockCoordinate `json:"coordinate"`
	Field      string          `json:"field"`
	Expected   any             `json:"expected,omitempty"`
	Actual     any             `json:"actual,omitempty"`
}

type ConsistencyReport struct {
	Consistent            bool           `json:"consistent"`
	CheckedBlocks         int            `json:"checkedBlocks"`
	FirstInconsistency    *Inconsistency `json:"firstInconsistency,omitempty"`
	CommitSequencesEqual  bool           `json:"commitSequencesEqual"`
	CommitSequenceLengths []int          `json:"commitSequenceLengths"`
	Details               []string       `json:"details,omitempty"`
}

func CompareRunReports(base *RunReport, others ...*RunReport) ConsistencyReport {
	rep := ConsistencyReport{Consistent: true, CommitSequencesEqual: true}
	if base == nil {
		rep.Consistent = false
		rep.Details = append(rep.Details, "base run is nil")
		return rep
	}
	baseByBlock := map[string]BlockOutcome{}
	for _, o := range base.Outcomes {
		baseByBlock[o.Block.ID] = o
	}
	rep.CheckedBlocks = len(baseByBlock)

	for _, other := range others {
		if other == nil {
			rep.Consistent = false
			rep.Details = append(rep.Details, "comparison run is nil")
			continue
		}
		otherByBlock := map[string]BlockOutcome{}
		for _, o := range other.Outcomes {
			otherByBlock[o.Block.ID] = o
		}
		for id, bo := range baseByBlock {
			oo, ok := otherByBlock[id]
			if !ok {
				rep.Consistent = false
				rep.FirstInconsistency = &Inconsistency{BlockID: id, Coordinate: bo.Block.Coordinate(), Field: "outcome.missing", Expected: "present", Actual: "missing in " + other.Mode}
				return rep
			}
			if inc := compareOutcome(bo, oo); inc != nil {
				rep.Consistent = false
				rep.FirstInconsistency = inc
				rep.FirstInconsistency.Field = other.Mode + ":" + inc.Field
				return rep
			}
		}
		if len(otherByBlock) != len(baseByBlock) {
			rep.Consistent = false
			rep.FirstInconsistency = &Inconsistency{Field: "outcome.count", Expected: len(baseByBlock), Actual: len(otherByBlock)}
			return rep
		}
		if !commitSequencesEqual(base.CommitSequence, other.CommitSequence) {
			rep.CommitSequencesEqual = false
			rep.Consistent = false
			rep.CommitSequenceLengths = append(rep.CommitSequenceLengths, len(base.CommitSequence), len(other.CommitSequence))
			if rep.FirstInconsistency == nil {
				rep.FirstInconsistency = firstCommitDifference(base.CommitSequence, other.CommitSequence)
			}
			return rep
		}
	}
	for _, r := range append([]*RunReport{base}, others...) {
		rep.CommitSequenceLengths = append(rep.CommitSequenceLengths, len(r.CommitSequence))
	}
	return rep
}

func compareOutcome(a, b BlockOutcome) *Inconsistency {
	coord := a.Block.Coordinate()
	if len(a.Quant.Indices) != len(b.Quant.Indices) {
		return &Inconsistency{BlockID: a.Block.ID, Coordinate: coord, Field: "quant.indices.length", Expected: len(a.Quant.Indices), Actual: len(b.Quant.Indices)}
	}
	for i := range a.Quant.Indices {
		if a.Quant.Indices[i] != b.Quant.Indices[i] {
			return &Inconsistency{BlockID: a.Block.ID, Coordinate: coord, Field: fmt.Sprintf("quant.indices[%d]", i), Expected: a.Quant.Indices[i], Actual: b.Quant.Indices[i]}
		}
	}
	for i := range a.Quant.PixelErrors {
		for c := 0; c < 3; c++ {
			if a.Quant.PixelErrors[i][c] != b.Quant.PixelErrors[i][c] {
				return &Inconsistency{BlockID: a.Block.ID, Coordinate: coord, Field: fmt.Sprintf("quant.pixelErrors[%d].%s", i, componentName(c)), Expected: a.Quant.PixelErrors[i][c], Actual: b.Quant.PixelErrors[i][c]}
			}
		}
	}
	if len(a.Reconstruction.Pixels) != len(b.Reconstruction.Pixels) {
		return &Inconsistency{BlockID: a.Block.ID, Coordinate: coord, Field: "reconstruction.pixels.length", Expected: len(a.Reconstruction.Pixels), Actual: len(b.Reconstruction.Pixels)}
	}
	for i := range a.Reconstruction.Pixels {
		if a.Reconstruction.Pixels[i] != b.Reconstruction.Pixels[i] {
			return &Inconsistency{BlockID: a.Block.ID, Coordinate: coord, Field: fmt.Sprintf("reconstruction.pixels[%d]", i), Expected: a.Reconstruction.Pixels[i], Actual: b.Reconstruction.Pixels[i]}
		}
	}
	for c := 0; c < 3; c++ {
		if a.Evaluation.MaxDeviation[c] != b.Evaluation.MaxDeviation[c] {
			return &Inconsistency{BlockID: a.Block.ID, Coordinate: coord, Field: "evaluation.maxDeviation." + componentName(c), Expected: a.Evaluation.MaxDeviation[c], Actual: b.Evaluation.MaxDeviation[c]}
		}
	}
	if a.Evaluation.WeightedColorDiff != b.Evaluation.WeightedColorDiff {
		return &Inconsistency{BlockID: a.Block.ID, Coordinate: coord, Field: "evaluation.weightedColorDiff", Expected: a.Evaluation.WeightedColorDiff, Actual: b.Evaluation.WeightedColorDiff}
	}
	if a.Evaluation.Passed != b.Evaluation.Passed {
		return &Inconsistency{BlockID: a.Block.ID, Coordinate: coord, Field: "evaluation.passed", Expected: a.Evaluation.Passed, Actual: b.Evaluation.Passed}
	}
	return nil
}

func commitSequencesEqual(a, b []CommitRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].BlockID != b[i].BlockID || a[i].Stage != b[i].Stage || a[i].OrderKey != b[i].OrderKey || a[i].CacheKey != b[i].CacheKey {
			return false
		}
	}
	return true
}

func firstCommitDifference(a, b []CommitRef) *Inconsistency {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return &Inconsistency{Field: fmt.Sprintf("commitSequence[%d]", i), Expected: a[i], Actual: b[i]}
		}
	}
	return &Inconsistency{Field: "commitSequence.length", Expected: len(a), Actual: len(b)}
}

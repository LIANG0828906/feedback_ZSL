// Package report assembles the error-propagation ledger from per-caption
// measured error terms and validates every term against cumulative upper
// bounds derived only from observed parameters (frame hop, sample-rate
// quantisation step, character weight denominators) - never from ground
// truth. A measured term exceeding its bound yields E_BOUND naming the
// caption, the term and the measured value.
package report

import (
	"fmt"

	"captionx/internal/errs"
	"captionx/internal/model"
)

// Error term names used in E_BOUND failures.
const (
	TermBoundaryQuant = "boundaryQuant"
	TermWindowDev     = "windowDeviation"
	TermSchedResidual = "schedResidual"
	TermDurationDev   = "durationDeviation"
	TermTotalDuration = "totalDuration"
)

// PerCaptionBound returns the observed-parameter upper bounds for one
// caption with nChars characters, given the frame hop:
//   - boundary quantisation: two boundaries, each quantised to the hop
//     grid with error <= hop/2, so the sum is <= hop.
//   - window deviation: window endpoints move only with quantised
//     boundaries, so |dStart|+|dEnd| <= hop.
//   - schedule residual: each of the n characters rounds its rational
//     position down by < 1 sample (residual numerator < weight
//     denominator W), so the sum is < n samples.
//   - duration deviation: |dEnd - dStart| <= |dEnd|+|dStart| <= hop.
func PerCaptionBound(hop, nChars int64) (boundary, window, sched, duration int64) {
	return hop, hop, nChars, hop
}

// Assemble builds the cumulative ledger over per-caption measured terms
// (in caption order). The window and duration bounds are tightened to
// the declared tolerance when that is smaller than the hop-derived
// bound. When enforce is true every term is checked against its bound
// and a violation yields E_BOUND; enforcement is meant for runs against
// the manifest's own parameters, since ground truth only applies there.
func Assemble(terms []model.CaptionErrorTerms, charCounts []int, hop, tolerance int64,
	totalDerived, truthTotal int64, enforce bool) (*model.ErrorReport, error) {
	if len(terms) != len(charCounts) {
		return nil, errs.Schema(-1, "errorTerms", "terms not parallel to captions")
	}
	winBound := hop
	if tolerance >= 0 && tolerance < winBound {
		winBound = tolerance
	}
	rep := &model.ErrorReport{Captions: terms}
	var cumB, cumW, cumS, cumD int64
	for i, t := range terms {
		bB, bW, bS, bD := PerCaptionBound(hop, int64(charCounts[i]))
		if bW > winBound {
			bW = winBound
		}
		if bD > winBound {
			bD = winBound
		}
		if enforce && t.BoundaryQuant > bB {
			return nil, errs.Bound(t.CaptionID, TermBoundaryQuant, t.BoundaryQuant,
				fmt.Sprintf("bound %d", bB))
		}
		if enforce && t.WindowDeviation > bW {
			return nil, errs.Bound(t.CaptionID, TermWindowDev, t.WindowDeviation,
				fmt.Sprintf("bound %d", bW))
		}
		if enforce && t.SchedResidual > bS {
			return nil, errs.Bound(t.CaptionID, TermSchedResidual, t.SchedResidual,
				fmt.Sprintf("bound %d", bS))
		}
		if enforce && t.DurationDeviation > bD {
			return nil, errs.Bound(t.CaptionID, TermDurationDev, t.DurationDeviation,
				fmt.Sprintf("bound %d", bD))
		}
		cumB += bB
		cumW += bW
		cumS += bS
		cumD += bD
		rep.BoundaryQuantBound = append(rep.BoundaryQuantBound, cumB)
		rep.WindowDeviationBound = append(rep.WindowDeviationBound, cumW)
		rep.SchedResidualBound = append(rep.SchedResidualBound, cumS)
		rep.DurationBound = append(rep.DurationBound, cumD)
	}
	rep.TotalDurationDeviation = totalDerived - truthTotal
	if rep.TotalDurationDeviation < 0 {
		rep.TotalDurationDeviation = -rep.TotalDurationDeviation
	}
	rep.TotalDurationBound = cumD
	if enforce && rep.TotalDurationDeviation > rep.TotalDurationBound {
		return nil, errs.Bound(-1, TermTotalDuration, rep.TotalDurationDeviation,
			fmt.Sprintf("bound %d", rep.TotalDurationBound))
	}
	return rep, nil
}

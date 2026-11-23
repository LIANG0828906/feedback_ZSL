// Package schedule splits a caption display window into per-character
// highlight intervals using exact integer rational arithmetic. The
// deterministic floor-cumulative rule guarantees contiguous,
// non-overlapping intervals whose total equals the window length.
package schedule

import (
	"fmt"

	"captionx/internal/errs"
	"captionx/internal/model"
)

// Result is the schedule for one caption.
type Result struct {
	Starts []int64
	Ends   []int64
	// ResidualNum/ResidualDen is the summed rounding residual of the
	// integer normalisation versus the exact rational positions,
	// expressed as a rational number of samples.
	ResidualNum int64
	ResidualDen int64
}

// Schedule computes per-character highlight intervals for a window of
// length L = win.Len() and nChars characters with the given weights.
//
// Position of character i: win.Start + floor(L * cumW_i / W), where cumW_i
// is the prefix weight sum and W the total weight (the denominator). The
// floor rule distributes the indivisible remainder deterministically:
// earlier characters never receive more than their exact rational share,
// and the accumulated remainder is absorbed by later characters.
//
// Errors: E_SCHED when the weight denominator is zero (all weights
// non-positive) or the window is shorter than the character count.
func Schedule(captionID int, win model.Window, weights []int64) (*Result, error) {
	n := len(weights)
	if n == 0 {
		return nil, errs.Sched(captionID, -1, "no characters to schedule")
	}
	L := win.Len()
	if L < int64(n) {
		return nil, errs.Sched(captionID, 0,
			fmt.Sprintf("window length %d shorter than character count %d", L, n))
	}
	var wsum int64
	for i, w := range weights {
		if w <= 0 {
			return nil, errs.Sched(captionID, i,
				fmt.Sprintf("weight denominator contribution non-positive: %d", w))
		}
		wsum += w
	}
	if wsum == 0 {
		return nil, errs.Sched(captionID, 0, "weight denominator is zero")
	}

	res := &Result{
		Starts:      make([]int64, n),
		Ends:        make([]int64, n),
		ResidualDen: wsum,
	}
	var cum int64
	for i := 0; i < n; i++ {
		exact := L * cum // numerator over wsum
		res.Starts[i] = win.Start + exact/wsum
		res.ResidualNum += exact % wsum
		cum += weights[i]
	}
	for i := 0; i+1 < n; i++ {
		res.Ends[i] = res.Starts[i+1]
	}
	res.Ends[n-1] = win.End
	return res, nil
}

// LinearWeights returns the unit weights used by the linear highlight mode.
func LinearWeights(n int) []int64 {
	w := make([]int64, n)
	for i := range w {
		w[i] = 1
	}
	return w
}

package planopt

import "sort"

type BindingSet struct {
	// Global maps canonical absolute parameter index to a bound value.
	Global map[int]int64 `json:"global,omitempty"`
	// Relative maps plan sequence to local position -> value.
	Relative map[int]map[int]int64 `json:"relative,omitempty"`
}

func DefaultBindings(project Project) BindingSet {
	out := BindingSet{Global: map[int]int64{}, Relative: map[int]map[int]int64{}}
	for _, plan := range project.Plans {
		out.Relative[plan.Seq] = map[int]int64{}
		for _, param := range plan.Params {
			out.Global[param.GlobalIndex] = param.DefaultValue
			out.Relative[plan.Seq][param.LocalIndex] = param.DefaultValue
		}
	}
	return out
}

func (b BindingSet) clone() BindingSet {
	out := BindingSet{Global: map[int]int64{}, Relative: map[int]map[int]int64{}}
	for k, v := range b.Global {
		out.Global[k] = v
	}
	for plan, vals := range b.Relative {
		out.Relative[plan] = map[int]int64{}
		for k, v := range vals {
			out.Relative[plan][k] = v
		}
	}
	return out
}

func (b BindingSet) valueFor(plan Plan, globalIndex, localIndex int) (int64, bool) {
	if len(b.Global) > 0 {
		if v, ok := b.Global[globalIndex]; ok {
			return v, true
		}
	}
	if vals, ok := b.Relative[plan.Seq]; ok {
		if v, ok := vals[localIndex]; ok {
			return v, true
		}
	}
	// Defaults are part of the deterministic engineering bundle.
	for _, p := range plan.Params {
		if p.GlobalIndex == globalIndex || p.LocalIndex == localIndex {
			return p.DefaultValue, true
		}
	}
	return 0, false
}

type PredicateEstimate struct {
	PredicateID       string `json:"predicate_id"`
	SelectivityPPM    int64  `json:"selectivity_ppm"`
	Permyriad         int64  `json:"selectivity_permyriad"`
	BucketIndex       int    `json:"bucket_index"`
	BucketBoundary    bool   `json:"bucket_boundary"`
	DeclaredPermyriad int64  `json:"declared_permyriad"`
	AbsoluteErrorPPM  int64  `json:"absolute_error_ppm"`
	WithinTolerance   bool   `json:"within_tolerance"`
}

func EstimatePredicates(project Project, plan Plan, bindings BindingSet) ([]PredicateEstimate, error) {
	tableByName := map[string]Table{}
	for _, t := range project.Tables {
		tableByName[t.Name] = t
	}
	preds := append([]Predicate(nil), plan.Predicates...)
	sort.Slice(preds, func(i, j int) bool { return preds[i].Ordinal < preds[j].Ordinal })

	out := make([]PredicateEstimate, 0, len(preds))
	for _, pred := range preds {
		table, ok := tableByName[pred.Table]
		if !ok {
			return nil, Errorf(CodeFieldCombination, "selectivity.table", "unknown table %q", pred.Table)
		}
		col, ok := table.Column(pred.Column)
		if !ok {
			return nil, Errorf(CodeFieldCombination, "selectivity.column", "unknown column %q.%q", pred.Table, pred.Column)
		}
		sel, bucketIdx, boundary, err := EstimatePredicate(table, col, pred, plan, bindings)
		if err != nil {
			return nil, err
		}
		declared := pred.DeclaredSelPermyriad
		truthPPM := declared * (SelBasis / ScalePermyriad)
		absErr := abs64(sel - truthPPM)
		out = append(out, PredicateEstimate{
			PredicateID: pred.ID, SelectivityPPM: sel,
			Permyriad: (sel + 50) / 100, BucketIndex: bucketIdx, BucketBoundary: boundary,
			DeclaredPermyriad: declared, AbsoluteErrorPPM: absErr,
			WithinTolerance: absErr <= SelTolerancePPM,
		})
	}
	return out, nil
}

func EstimatePredicate(table Table, col ColumnStat, pred Predicate, plan Plan, bindings BindingSet) (selPPM int64, bucketIndex int, boundary bool, err error) {
	bucketIndex = -1
	switch pred.Op {
	case "EQ":
		v, ok := bindings.valueFor(plan, pred.GlobalParamIndex, pred.LocalParamIndex)
		if pred.HasLiteral {
			v = pred.LiteralValue
			ok = true
		}
		if !ok {
			return 0, -1, false, Errorf(CodePlaceholderOutOfRange, "binding.eq", "predicate %s has no bound value", pred.ID)
		}
		idx, isBoundary := bucketAt(col.HistogramBuckets, v)
		bucketIndex, boundary = idx, isBoundary
		sel := estimateEQPPM(table.RowCount, col, v, idx)
		return applyNullFactor(sel, col.NullPermyriad), idx, boundary, nil
	case "GT", "GTE", "LT", "LTE":
		v, ok := bindings.valueFor(plan, pred.GlobalParamIndex, pred.LocalParamIndex)
		if pred.HasLiteral {
			v = pred.LiteralValue
			ok = true
		}
		if !ok {
			return 0, -1, false, Errorf(CodePlaceholderOutOfRange, "binding.range", "predicate %s has no bound value", pred.ID)
		}
		lo, hi := col.MinValue, col.MaxValue
		switch pred.Op {
		case "GT":
			lo = v + 1
		case "GTE":
			lo = v
		case "LT":
			hi = v - 1
		case "LTE":
			hi = v
		}
		boundary = endpointBoundary(col.HistogramBuckets, v)
		idx, _ := bucketAt(col.HistogramBuckets, clamp64(v, col.MinValue, col.MaxValue))
		bucketIndex = idx
		sel := estimateRangePPM(table.RowCount, col, lo, hi)
		return applyNullFactor(sel, col.NullPermyriad), idx, boundary, nil
	case "BETWEEN":
		lo, ok1 := bindings.valueFor(plan, pred.GlobalParamIndex, pred.LocalParamIndex)
		hi, ok2 := bindings.valueFor(plan, pred.SecondGlobalParamIndex, pred.SecondLocalParamIndex)
		if pred.HasLiteral {
			lo = pred.LiteralValue
			ok1 = true
		}
		if pred.HasSecondLiteral {
			hi = pred.SecondLiteralValue
			ok2 = true
		}
		if !ok1 || !ok2 {
			return 0, -1, false, Errorf(CodePlaceholderOutOfRange, "binding.between", "predicate %s requires two values", pred.ID)
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		boundary = endpointBoundary(col.HistogramBuckets, lo) || endpointBoundary(col.HistogramBuckets, hi)
		idx, _ := bucketAt(col.HistogramBuckets, lo)
		bucketIndex = idx
		sel := estimateRangePPM(table.RowCount, col, lo, hi)
		return applyNullFactor(sel, col.NullPermyriad), idx, boundary, nil
	default:
		// Conservative unknown operator: 1/NDV with a floor. This is still
		// integer-only and deterministic.
		ndv := max64(1, col.NDV)
		sel := SelBasis / ndv
		return applyNullFactor(sel, col.NullPermyriad), -1, false, nil
	}
}

func estimateEQPPM(tableRows int64, col ColumnStat, v int64, bucketIdx int) int64 {
	if tableRows <= 0 {
		return SelBasis
	}
	if bucketIdx >= 0 && bucketIdx < len(col.HistogramBuckets) {
		bucket := col.HistogramBuckets[bucketIdx]
		distinct := max64(1, bucket.DistinctCount)
		// bucket_rows * basis / distinct / table_rows keeps all declared
		// fixed-point arithmetic integral.
		sel := bucket.RowCount * SelBasis / distinct
		return clamp64(sel/tableRows, 1, SelBasis)
	}
	ndv := max64(1, col.NDV)
	return clamp64(SelBasis/ndv, 1, SelBasis)
}

func estimateRangePPM(tableRows int64, col ColumnStat, lo, hi int64) int64 {
	if lo > hi || hi < col.MinValue || lo > col.MaxValue || tableRows <= 0 {
		return 0
	}
	lo = max64(lo, col.MinValue)
	hi = min64(hi, col.MaxValue)
	buckets := col.HistogramBuckets
	if len(buckets) == 0 {
		span := max64(1, col.MaxValue-col.MinValue+1)
		overlap := min64(hi, col.MaxValue) - max64(lo, col.MinValue) + 1
		return clamp64(SelBasis*overlap/span, 0, SelBasis)
	}

	var weighted int64
	var prevUpper int64
	for _, bucket := range buckets {
		firstValue := prevUpper + 1
		width := bucket.UpperBound - prevUpper
		if width <= 0 {
			width = 1
			firstValue = bucket.UpperBound
		}
		overlapLeft := max64(lo, firstValue)
		overlapRight := min64(hi, bucket.UpperBound)
		if overlapLeft <= overlapRight {
			values := overlapRight - overlapLeft + 1
			if values >= width {
				weighted += bucket.RowCount * SelBasis
			} else {
				weighted += bucket.RowCount * SelBasis * values / width
			}
		}
		prevUpper = bucket.UpperBound
	}
	return clamp64(weighted/tableRows, 0, SelBasis)
}

func applyNullFactor(selPPM, nullPermyriad int64) int64 {
	if nullPermyriad <= 0 {
		return clamp64(selPPM, 0, SelBasis)
	}
	nonNull := ScalePermyriad - clamp64(nullPermyriad, 0, ScalePermyriad)
	return selPPM * nonNull / ScalePermyriad
}

func bucketAt(buckets []Bucket, v int64) (int, bool) {
	prev := int64(0)
	for i, bk := range buckets {
		first := prev + 1
		if bk.UpperBound-prev <= 0 {
			first = bk.UpperBound
		}
		if v >= first && v <= bk.UpperBound {
			return i, v == bk.UpperBound
		}
		prev = bk.UpperBound
	}
	// Values at or below the first lower edge are treated as first bucket.
	if len(buckets) > 0 && v <= buckets[0].UpperBound {
		return 0, v == buckets[0].UpperBound
	}
	return -1, false
}

func endpointBoundary(buckets []Bucket, v int64) bool {
	for _, bk := range buckets {
		if v == bk.UpperBound {
			return true
		}
	}
	return false
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func clamp64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

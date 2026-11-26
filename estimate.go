package queryopt

type PredicateEstimate struct {
	PredicateID        string `json:"predicate_id"`
	QueryID            string `json:"query_id"`
	Kind               string `json:"kind"`
	SelectivityRaw     int64  `json:"selectivity_raw"`
	Selectivity        Fixed  `json:"selectivity"`
	EstimatedRows      int64  `json:"estimated_rows"`
	Saturated          bool   `json:"saturated"`
	BoundaryHit        bool   `json:"boundary_hit,omitempty"`
	BoundaryBucketID   string `json:"boundary_bucket_id,omitempty"`
	BoundaryAlignments int    `json:"boundary_alignments,omitempty"`
}

type EstimateReport struct {
	Predicates map[string]PredicateEstimate `json:"predicates"`
}

func (r *EstimateReport) get(id string) PredicateEstimate {
	if r == nil || r.Predicates == nil {
		return PredicateEstimate{}
	}
	return r.Predicates[id]
}

func resolveArg(a PredicateArg, bindings BindingSet, predID, field string) (int64, error) {
	if a.Literal != nil {
		return *a.Literal, nil
	}
	if a.PlaceholderGlobal <= 0 {
		return 0, newError(CodeInputInvalid, "predicate argument has neither literal nor placeholder").
			with("predicate_id", predID).with("field", field)
	}
	v, ok := bindings.Global[a.PlaceholderGlobal]
	if !ok {
		return 0, newError(CodePlaceholderUnbound, "placeholder is not bound").
			with("predicate_id", predID).with("field", field).
			with("placeholder_global", a.PlaceholderGlobal)
	}
	return v, nil
}

func bucketContains(b *Bucket, v int64) bool {
	if v < b.Lower {
		return false
	}
	if v < b.Upper {
		return true
	}
	// The final declared bucket is closed on its upper edge. Earlier equal
	// upper edges belong to the next bucket, preventing double counting.
	return false
}

func findBucket(buckets []*Bucket, v int64) *Bucket {
	for _, b := range buckets {
		if v == b.Upper {
			// Exact upper boundary is reported for boundary probes, but normal
			// containment uses the following bucket when one exists.
			continue
		}
		if bucketContains(b, v) {
			return b
		}
	}
	// Last bucket closed upper endpoint.
	for i := len(buckets) - 1; i >= 0; i-- {
		if v == buckets[i].Upper {
			return buckets[i]
		}
	}
	return nil
}

func EstimateProject(p *Project, bindings BindingSet) (*EstimateReport, error) {
	idx := p.Index()
	report := &EstimateReport{Predicates: map[string]PredicateEstimate{}}
	state := map[string]int{} // 0=visiting,1=done

	var estimate func(id string) (PredicateEstimate, error)
	estimate = func(id string) (PredicateEstimate, error) {
		if cached, ok := report.Predicates[id]; ok {
			return cached, nil
		}
		if state[id] == 1 {
			return PredicateEstimate{}, newError(CodeInputInvalid, "cyclic predicate expression").
				with("predicate_id", id)
		}
		state[id] = 1
		pred := idx.Predicates[id]
		if pred == nil {
			return PredicateEstimate{}, newError(CodeInputInvalid, "missing predicate").with("predicate_id", id)
		}
		out := PredicateEstimate{PredicateID: id, QueryID: pred.QueryID, Kind: pred.Kind}
		var sel Fixed
		var saturated bool
		var err error

		switch pred.Kind {
		case PredEquality:
			sel, saturated, err = estimateEquality(p, idx, pred, bindings, &out)
		case PredRange:
			sel, saturated, err = estimateRange(p, idx, pred, bindings, &out)
		case PredBoundary:
			sel, saturated, err = estimateBoundary(p, idx, pred, bindings, &out)
		case PredJoin:
			sel, saturated, err = estimateJoin(idx, pred)
		case PredAnd, PredOr:
			sel, saturated, err = estimateCompound(pred, estimate)
		default:
			err = newError(CodeInputInvalid, "unknown predicate kind").
				with("predicate_id", id).with("kind", pred.Kind)
		}
		if err != nil {
			return PredicateEstimate{}, err
		}
		rows, rowSat := predicateBaseRows(p, idx, pred, estimate)
		estimatedRows, mulSat := RoundedRows(rows, sel)
		out.Selectivity = sel
		out.SelectivityRaw = int64(sel)
		out.EstimatedRows = estimatedRows
		out.Saturated = saturated || rowSat || mulSat
		if out.Saturated {
			out.EstimatedRows = estimatedRows
		}
		report.Predicates[id] = out
		state[id] = 2
		return out, nil
	}

	for _, q := range p.Queries {
		for _, id := range q.RootPredicateIDs {
			if _, err := estimate(id); err != nil {
				return nil, err
			}
		}
		for _, pred := range p.Predicates {
			if pred.QueryID == q.ID {
				if _, err := estimate(pred.ID); err != nil {
					return nil, err
				}
			}
		}
	}
	return report, nil
}

func estimateEquality(p *Project, idx *ProjectIndex, pred *Predicate, bindings BindingSet, out *PredicateEstimate) (Fixed, bool, error) {
	v, err := resolveArg(pred.Value, bindings, pred.ID, "value")
	if err != nil {
		return 0, false, err
	}
	col := idx.Columns[pred.ColumnID]
	table := idx.Tables[col.TableID]
	buckets := idx.BucketsByColumn[col.ID]
	b := findBucket(buckets, v)
	if b != nil {
		if v == b.Lower || v == b.Upper {
			out.BoundaryHit = true
			out.BoundaryBucketID = b.ID
		}
		rowFrac, sat1 := FixedFromRatio(b.Rows, table.Rows)
		distFrac, sat2 := FixedFromRatio(1, b.Distinct)
		sel, sat3 := rowFrac.Mul(distFrac)
		return sel, sat1 || sat2 || sat3, nil
	}
	sel, sat := FixedFromRatio(1, col.NDV)
	return sel, sat, nil
}

func estimateBoundary(p *Project, idx *ProjectIndex, pred *Predicate, bindings BindingSet, out *PredicateEstimate) (Fixed, bool, error) {
	v, err := resolveArg(pred.Value, bindings, pred.ID, "value")
	if err != nil {
		return 0, false, err
	}
	col := idx.Columns[pred.ColumnID]
	table := idx.Tables[col.TableID]
	buckets := idx.BucketsByColumn[col.ID]
	var hit *Bucket
	for _, b := range buckets {
		if v == b.Lower || v == b.Upper {
			hit = b
			break
		}
	}
	if hit == nil {
		return 0, false, newError(CodeInputInvalid, "boundary predicate value does not equal a histogram boundary").
			with("predicate_id", pred.ID).with("column_id", col.ID).with("value", v)
	}
	out.BoundaryHit = true
	out.BoundaryBucketID = hit.ID
	rowFrac, sat1 := FixedFromRatio(hit.Rows, table.Rows)
	distFrac, sat2 := FixedFromRatio(1, hit.Distinct)
	sel, sat3 := rowFrac.Mul(distFrac)
	return sel, sat1 || sat2 || sat3, nil
}

func estimateRange(p *Project, idx *ProjectIndex, pred *Predicate, bindings BindingSet, out *PredicateEstimate) (Fixed, bool, error) {
	low, err := resolveArg(pred.Low, bindings, pred.ID, "low")
	if err != nil {
		return 0, false, err
	}
	high, err := resolveArg(pred.High, bindings, pred.ID, "high")
	if err != nil {
		return 0, false, err
	}
	lo := low
	if !pred.InclusiveLow {
		lo++
	}
	hi := high
	if pred.InclusiveHigh {
		hi++
	}
	if hi <= lo {
		return 0, false, newError(CodeInputInvalid, "range predicate has empty effective interval").
			with("predicate_id", pred.ID).with("effective_low", lo).with("effective_high", hi)
	}
	col := idx.Columns[pred.ColumnID]
	table := idx.Tables[col.TableID]
	buckets := idx.BucketsByColumn[col.ID]
	var total Fixed
	saturated := false
	for _, b := range buckets {
		overlapLo := max64(lo, b.Lower)
		overlapHi := min64(hi, b.Upper)
		if overlapHi <= overlapLo {
			continue
		}
		width := b.Upper - b.Lower
		overlap := overlapHi - overlapLo
		if lo == b.Lower || lo == b.Upper || hi == b.Lower || hi == b.Upper {
			out.BoundaryAlignments++
		}
		rowFrac, sat1 := FixedFromRatio(b.Rows, table.Rows)
		widthFrac, sat2 := FixedFromRatio(overlap, width)
		part, sat3 := rowFrac.Mul(widthFrac)
		var addSat bool
		total, addSat = total.Add(part)
		saturated = saturated || sat1 || sat2 || sat3 || addSat
	}
	if total > FixedOne {
		total = FixedOne
		saturated = true
	}
	return total, saturated, nil
}

func estimateJoin(idx *ProjectIndex, pred *Predicate) (Fixed, bool, error) {
	lc := idx.Columns[pred.LeftColumnID]
	rc := idx.Columns[pred.RightColumnID]
	if lc == nil || rc == nil {
		return 0, false, newError(CodeInputInvalid, "join predicate references missing column").
			with("predicate_id", pred.ID).with("left_column_id", pred.LeftColumnID).
			with("right_column_id", pred.RightColumnID)
	}
	ndv := max64(lc.NDV, rc.NDV)
	sel, saturated := FixedFromRatio(1, ndv)
	return sel, saturated, nil
}

func estimateCompound(pred *Predicate, estimate func(string) (PredicateEstimate, error)) (Fixed, bool, error) {
	if len(pred.Children) == 0 {
		return FixedOne, false, nil
	}
	saturated := false
	if pred.Logic == PredOr {
		nonMatch := FixedOne
		for _, childID := range pred.Children {
			child, err := estimate(childID)
			if err != nil {
				return 0, false, err
			}
			comp := child.Selectivity.Complement()
			var sat bool
			nonMatch, sat = nonMatch.Mul(comp)
			saturated = saturated || sat || child.Saturated
		}
		return nonMatch.Complement(), saturated, nil
	}
	sel := FixedOne
	for _, childID := range pred.Children {
		child, err := estimate(childID)
		if err != nil {
			return 0, false, err
		}
		var sat bool
		sel, sat = sel.Mul(child.Selectivity)
		saturated = saturated || sat || child.Saturated
	}
	return sel, saturated, nil
}

func predicateBaseRows(p *Project, idx *ProjectIndex, pred *Predicate, estimate func(string) (PredicateEstimate, error)) (int64, bool) {
	tableIDs := map[string]bool{}
	var walk func(*Predicate)
	walk = func(d *Predicate) {
		if d == nil {
			return
		}
		if d.TableID != "" {
			tableIDs[d.TableID] = true
		}
		if d.LeftColumnID != "" {
			if c := idx.Columns[d.LeftColumnID]; c != nil {
				tableIDs[c.TableID] = true
			}
		}
		if d.RightColumnID != "" {
			if c := idx.Columns[d.RightColumnID]; c != nil {
				tableIDs[c.TableID] = true
			}
		}
		for _, id := range d.Children {
			if child := idx.Predicates[id]; child != nil {
				walk(child)
			}
		}
	}
	walk(pred)
	rows := int64(1)
	sat := false
	for id := range tableIDs {
		if t := idx.Tables[id]; t != nil {
			rows, sat = CheckedMul(rows, t.Rows)
		}
	}
	if rows == 1 && pred.TableID != "" {
		if t := idx.Tables[pred.TableID]; t != nil {
			rows = t.Rows
		}
	}
	return rows, sat
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

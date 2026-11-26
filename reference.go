package queryopt

import (
	"math/big"
)

type ReferenceValue struct {
	Raw      int64
	Rows     int64
	BaseRows int64
}

func ReferenceSelectivities(p *Project, bindings BindingSet) (map[string]ReferenceValue, error) {
	idx := p.Index()
	out := map[string]ReferenceValue{}
	state := map[string]bool{}
	var ref func(id string) (*big.Rat, []string, error)

	ref = func(id string) (*big.Rat, []string, error) {
		if rv, ok := out[id]; ok {
			return big.NewRat(rv.Raw, FixedScale), nil, nil
		}
		if state[id] {
			return nil, nil, newError(CodeInputInvalid, "cyclic predicate in reference truth").with("predicate_id", id)
		}
		state[id] = true
		pred := idx.Predicates[id]
		if pred == nil {
			return nil, nil, newError(CodeInputInvalid, "missing predicate").with("predicate_id", id)
		}
		sel := big.NewRat(1, 1)
		tableIDs := map[string]bool{}

		addColTable := func(cid string) {
			if c := idx.Columns[cid]; c != nil {
				tableIDs[c.TableID] = true
			}
		}
		addColTable(pred.ColumnID)
		addColTable(pred.LeftColumnID)
		addColTable(pred.RightColumnID)
		if pred.TableID != "" {
			tableIDs[pred.TableID] = true
		}

		switch pred.Kind {
		case PredEquality:
			v, err := resolveArg(pred.Value, bindings, id, "value")
			if err != nil {
				return nil, nil, err
			}
			c := idx.Columns[pred.ColumnID]
			t := idx.Tables[c.TableID]
			addColTable(c.ID)
			b := findBucket(idx.BucketsByColumn[c.ID], v)
			if b != nil {
				sel = big.NewRat(b.Rows, b.Distinct*t.Rows)
			} else {
				sel = big.NewRat(1, c.NDV)
			}
		case PredBoundary:
			v, err := resolveArg(pred.Value, bindings, id, "value")
			if err != nil {
				return nil, nil, err
			}
			c := idx.Columns[pred.ColumnID]
			t := idx.Tables[c.TableID]
			for _, b := range idx.BucketsByColumn[c.ID] {
				if v == b.Lower || v == b.Upper {
					sel = big.NewRat(b.Rows, b.Distinct*t.Rows)
					break
				}
			}
		case PredRange:
			loV, err := resolveArg(pred.Low, bindings, id, "low")
			if err != nil {
				return nil, nil, err
			}
			hiV, err := resolveArg(pred.High, bindings, id, "high")
			if err != nil {
				return nil, nil, err
			}
			lo := loV
			if !pred.InclusiveLow {
				lo++
			}
			hi := hiV
			if pred.InclusiveHigh {
				hi++
			}
			c := idx.Columns[pred.ColumnID]
			t := idx.Tables[c.TableID]
			sel = big.NewRat(0, 1)
			for _, b := range idx.BucketsByColumn[c.ID] {
				olo := max64(lo, b.Lower)
				ohi := min64(hi, b.Upper)
				if ohi <= olo {
					continue
				}
				part := big.NewRat(b.Rows, t.Rows)
				part.Mul(part, big.NewRat(ohi-olo, b.Upper-b.Lower))
				sel.Add(sel, part)
			}
		case PredJoin:
			lc := idx.Columns[pred.LeftColumnID]
			rc := idx.Columns[pred.RightColumnID]
			sel = big.NewRat(1, max64(lc.NDV, rc.NDV))
		case PredAnd, PredOr:
			if pred.Logic == PredOr {
				nonMatch := big.NewRat(1, 1)
				for _, cid := range pred.Children {
					cs, _, err := ref(cid)
					if err != nil {
						return nil, nil, err
					}
					oneMinus := new(big.Rat).Sub(big.NewRat(1, 1), cs)
					nonMatch.Mul(nonMatch, oneMinus)
					for tid := range referencePredicateTables(idx, idx.Predicates[cid]) {
						tableIDs[tid] = true
					}
				}
				sel.Sub(big.NewRat(1, 1), nonMatch)
			} else {
				for _, cid := range pred.Children {
					cs, _, err := ref(cid)
					if err != nil {
						return nil, nil, err
					}
					sel.Mul(sel, cs)
					for tid := range referencePredicateTables(idx, idx.Predicates[cid]) {
						tableIDs[tid] = true
					}
				}
			}
		}

		raw := ratToFixedRaw(sel)
		var baseRows int64 = 1
		tids := make([]string, 0, len(tableIDs))
		for tid := range tableIDs {
			tids = append(tids, tid)
		}
		for _, tid := range tids {
			if t := idx.Tables[tid]; t != nil {
				baseRows *= t.Rows
			}
		}
		rowRat := new(big.Rat).Mul(sel, big.NewRat(baseRows, 1))
		rows := ratRoundInt(rowRat)
		out[id] = ReferenceValue{Raw: raw, Rows: rows, BaseRows: baseRows}
		state[id] = false
		return sel, tids, nil
	}

	for _, pred := range p.Predicates {
		if _, _, err := ref(pred.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func referencePredicateTables(idx *ProjectIndex, pred *Predicate) map[string]bool {
	out := map[string]bool{}
	if pred == nil {
		return out
	}
	var walk func(*Predicate)
	walk = func(d *Predicate) {
		if d.TableID != "" {
			out[d.TableID] = true
		}
		if c := idx.Columns[d.LeftColumnID]; c != nil {
			out[c.TableID] = true
		}
		if c := idx.Columns[d.RightColumnID]; c != nil {
			out[c.TableID] = true
		}
		for _, id := range d.Children {
			walk(idx.Predicates[id])
		}
	}
	walk(pred)
	return out
}

func ratToFixedRaw(v *big.Rat) int64 {
	scaled := new(big.Rat).Mul(v, big.NewRat(FixedScale, 1))
	return ratRoundInt(scaled)
}

func ratRoundInt(v *big.Rat) int64 {
	num := new(big.Int).Set(v.Num())
	den := v.Denom()
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Sign() >= 0 && new(big.Int).Mul(r, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return MaxInt64Value
	}
	return q.Int64()
}

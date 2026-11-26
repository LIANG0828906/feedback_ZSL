package queryopt

import "sort"

// BindingSet uses 1-based global absolute placeholder numbers.
type BindingSet struct {
	Global map[int]int64 `json:"global"`
}

// RelativeBindings maps query ID -> 0-based per-statement placeholder index.
type RelativeBindings map[string]map[int]int64

func GlobalFromRelative(p *Project, rel RelativeBindings) (BindingSet, error) {
	idx := p.Index()
	out := BindingSet{Global: map[int]int64{}}
	for queryID, vals := range rel {
		q := idx.Queries[queryID]
		if q == nil {
			return BindingSet{}, newError(CodeInputInvalid, "binding for missing query").with("query_id", queryID)
		}
		for relativeIndex, v := range vals {
			if relativeIndex < 0 || relativeIndex >= q.PlaceholderCount {
				return BindingSet{}, newError(CodeInputInvalid, "relative placeholder index out of range").
					with("query_id", queryID).with("relative_index", relativeIndex).
					with("placeholder_count", q.PlaceholderCount)
			}
			global := q.PlaceholderBase + relativeIndex + 1
			out.Global[global] = v
		}
	}
	return out, nil
}

func RelativeFromGlobal(p *Project, global BindingSet) (RelativeBindings, error) {
	idx := p.Index()
	out := RelativeBindings{}
	for gid := range global.Global {
		var owner *Query
		for i := range p.Queries {
			q := &p.Queries[i]
			first := q.PlaceholderBase + 1
			last := q.PlaceholderBase + q.PlaceholderCount
			if gid >= first && gid <= last {
				owner = q
				break
			}
		}
		if owner == nil {
			return nil, newError(CodeInputInvalid, "global placeholder does not belong to any query").
				with("placeholder_global", gid)
		}
		rel := gid - owner.PlaceholderBase - 1
		if out[owner.ID] == nil {
			out[owner.ID] = map[int]int64{}
		}
		out[owner.ID][rel] = global.Global[gid]
		_ = idx
	}
	return out, nil
}

func (b BindingSet) sortedIDs() []int {
	ids := make([]int, 0, len(b.Global))
	for id := range b.Global {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func sameBindings(a, b BindingSet) bool {
	if len(a.Global) != len(b.Global) {
		return false
	}
	for k, v := range a.Global {
		if b.Global[k] != v {
			return false
		}
	}
	return true
}

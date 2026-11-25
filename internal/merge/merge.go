package merge

import (
	"sort"

	"samplingkernel/internal/errs"
	"samplingkernel/internal/model"
)

type uniqueSpan struct {
	span       model.Span
	duplicates int
	conflict   bool
}

func MergeTrace(traceID string, reports []model.Span, policy string) *model.MergeResult {
	byID := map[string]*uniqueSpan{}
	var duplicateIDs []string
	for _, reported := range reports {
		existing, ok := byID[reported.SpanID]
		if !ok {
			sp := reported
			byID[sp.SpanID] = &uniqueSpan{span: sp}
			continue
		}
		duplicateIDs = append(duplicateIDs, reported.SpanID)
		existing.duplicates++
		if !sameSpan(existing.span, reported) {
			existing.conflict = true
		}
	}
	sort.Strings(duplicateIDs)
	duplicateIDs = compactStrings(duplicateIDs)

	result := &model.MergeResult{
		TraceID:          traceID,
		ParentRefs:       map[string]string{},
		DuplicateSpanIDs: duplicateIDs,
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
		result.ParentRefs[id] = byID[id].span.ParentSpanID
	}
	sort.Strings(ids)

	validity := map[string]bool{}
	visiting := map[string]bool{}
	var hasValidChain func(id string) bool
	hasValidChain = func(id string) bool {
		if valid, ok := validity[id]; ok {
			return valid
		}
		if visiting[id] {
			return false
		}
		sp := byID[id].span
		if sp.ParentSpanID == "" {
			validity[id] = true
			return true
		}
		parent, ok := byID[sp.ParentSpanID]
		if !ok {
			validity[id] = false
			return false
		}
		visiting[id] = true
		valid := hasValidChain(parent.span.SpanID)
		visiting[id] = false
		validity[id] = valid
		return valid
	}

	var connected []model.Span
	var orphans []model.Span
	for _, id := range ids {
		sp := byID[id].span
		if hasValidChain(id) {
			connected = append(connected, sp)
		} else {
			orphans = append(orphans, sp)
		}
	}

	ordered := topologicalSort(connected)
	for _, sp := range ordered {
		result.Spans = append(result.Spans, model.MergedSpan{
			Span:                  sp,
			EffectiveParentSpanID: sp.ParentSpanID,
			MergeState:            "merged",
		})
		result.OrderedSpanIDs = append(result.OrderedSpanIDs, sp.SpanID)
	}

	sort.Slice(orphans, func(i, j int) bool {
		if orphans[i].CausalOrder != orphans[j].CausalOrder {
			return orphans[i].CausalOrder < orphans[j].CausalOrder
		}
		return orphans[i].SpanID < orphans[j].SpanID
	})

	for _, sp := range orphans {
		code := errs.OrphanSpan
		reason := orphanReason(sp, byID)
		orphan := model.UnmergedSpan{
			SpanID:       sp.SpanID,
			TraceID:      traceID,
			ParentSpanID: sp.ParentSpanID,
			Code:         code,
			Reason:       reason,
		}
		switch policy {
		case model.OrphanAttach:
			result.Spans = append(result.Spans, model.MergedSpan{
				Span:                  sp,
				EffectiveParentSpanID: "",
				MergeState:            "orphan_attached_to_trace_root",
				OrphanCode:            code,
				OrphanReason:          reason,
			})
			result.OrderedSpanIDs = append(result.OrderedSpanIDs, sp.SpanID)
			result.Reasons = append(result.Reasons, "attached orphan "+sp.SpanID+": "+reason)
			orphan.Action = "attached_to_trace_root"
		default:
			orphan.Action = "quarantined_pending_parent_or_manual_replay"
			result.UnmergedSpans = append(result.UnmergedSpans, orphan)
			result.Reasons = append(result.Reasons, "quarantined orphan "+sp.SpanID+": "+reason)
		}
	}

	if len(result.UnmergedSpans) > 0 {
		result.Status = "quarantined_orphans"
	} else if len(orphans) > 0 {
		result.Status = "merged_with_attached_orphans"
	} else {
		result.Status = "merged"
	}
	return result
}

func topologicalSort(spans []model.Span) []model.Span {
	byID := map[string]model.Span{}
	indegree := map[string]int{}
	children := map[string][]string{}
	for _, sp := range spans {
		byID[sp.SpanID] = sp
		indegree[sp.SpanID] = 0
	}
	for _, sp := range spans {
		if sp.ParentSpanID == "" {
			continue
		}
		if _, ok := byID[sp.ParentSpanID]; !ok {
			continue
		}
		indegree[sp.SpanID]++
		children[sp.ParentSpanID] = append(children[sp.ParentSpanID], sp.SpanID)
	}

	ready := make([]string, 0)
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	less := func(a, b string) bool {
		sa, sb := byID[a], byID[b]
		if sa.CausalOrder != sb.CausalOrder {
			return sa.CausalOrder < sb.CausalOrder
		}
		if sa.ServiceID != sb.ServiceID {
			return sa.ServiceID < sb.ServiceID
		}
		return a < b
	}
	sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })

	var out []model.Span
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, byID[id])
		kids := append([]string{}, children[id]...)
		sort.Slice(kids, func(i, j int) bool { return less(kids[i], kids[j]) })
		for _, child := range kids {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
			}
		}
	}

	if len(out) != len(spans) {
		remaining := make([]model.Span, 0, len(spans)-len(out))
		seen := map[string]struct{}{}
		for _, sp := range out {
			seen[sp.SpanID] = struct{}{}
		}
		for _, sp := range spans {
			if _, ok := seen[sp.SpanID]; !ok {
				remaining = append(remaining, sp)
			}
		}
		sort.Slice(remaining, func(i, j int) bool { return less(remaining[i].SpanID, remaining[j].SpanID) })
		out = append(out, remaining...)
	}
	return out
}

func sameSpan(a, b model.Span) bool {
	return a.TraceID == b.TraceID && a.ParentSpanID == b.ParentSpanID && a.ServiceID == b.ServiceID &&
		a.EdgeID == b.EdgeID && a.CausalOrder == b.CausalOrder && a.Name == b.Name &&
		a.Status == b.Status && a.LatencyMS == b.LatencyMS
}

func orphanReason(sp model.Span, byID map[string]*uniqueSpan) string {
	if sp.ParentSpanID == "" {
		return "causal parent chain contains a cycle"
	}
	if _, ok := byID[sp.ParentSpanID]; !ok {
		return "parent span " + sp.ParentSpanID + " has not been reported"
	}
	return "parent span " + sp.ParentSpanID + " is detached because an ancestor is missing or cyclic"
}

func compactStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

package incremental

import (
	"fmt"
	"reflect"
	"sort"

	"chaosdiag/engine"
	"chaosdiag/model"
)

func buildClosure(oldP, newP *model.Project, mut Mutation, objectID, field, change string, oldSched, newSched []model.TickSchedule) (*Closure, map[string][]InvalidationSource) {
	closure := &Closure{ChangedObject: objectID, FirstAffectedHop: map[string]int{}}
	sources := map[string][]InvalidationSource{}
	addNode(closure, ClosureNode{ID: objectID, Kind: mut.Kind})

	intervals := []model.Interval{}

	switch mut.Kind {
	case "edge_latency", "dependency_edge":
		oldEdge, _ := oldP.EdgeByID(objectID)
		newEdge, _ := newP.EdgeByID(objectID)
		addNode(closure, ClosureNode{ID: "edge:" + objectID, Kind: "edge"})
		addEdge(closure, objectID, "edge:"+objectID, "mutated dependency edge")
		intervals = append(intervals, model.Interval{StartTick: 1, EndTick: maxRequestTick(newP)})
		for _, req := range newP.Requests {
			for i := 0; i+1 < len(req.Path); i++ {
				target, ok := newP.HopTarget(req, i)
				if !ok {
					continue
				}
				if target.EdgeID == objectID || (oldEdge.From != "" && target.From == oldEdge.From && target.To == oldEdge.To) ||
					(newEdge.From != "" && target.From == newEdge.From && target.To == newEdge.To) {
					addRequestClosure(closure, sources, req.ID, i, objectID, "edge:"+objectID, field, change,
						fmt.Sprintf("request path uses mutated edge %s at hop %d", objectID, i), model.Interval{StartTick: req.Tick, EndTick: req.Tick})
				}
			}
		}

	default:
		oldIns := engine.FindInstruction(oldP, objectID)
		newIns := engine.FindInstruction(newP, objectID)
		if oldIns != nil && newIns != nil {
			start := minInt(oldIns.StartTick, newIns.StartTick)
			end := maxInt(oldIns.EndTick, newIns.EndTick)
			interval := model.Interval{StartTick: start, EndTick: end}
			intervals = append(intervals, interval)
			targetNode := targetNodeForInstruction(newP, *newIns)
			addNode(closure, targetNode)
			addEdge(closure, objectID, targetNode.ID, "instruction selector target")

			for _, req := range newP.Requests {
				if req.Tick < start || req.Tick > end {
					continue
				}
				for i := 0; i+1 < len(req.Path); i++ {
					target, ok := newP.HopTarget(req, i)
					if !ok {
						continue
					}
					oldMatch := model.SelectorMatches(oldIns.Selector, target)
					newMatch := model.SelectorMatches(newIns.Selector, target)
					if oldMatch || newMatch {
						reason := fmt.Sprintf("hop %d intersects mutated instruction selector/window", i)
						if oldMatch != newMatch {
							reason = fmt.Sprintf("selector mutation changes hop %d membership (old=%v new=%v)", i, oldMatch, newMatch)
						}
						addRequestClosure(closure, sources, req.ID, i, objectID, targetNode.ID, field, change, reason, interval)
					}
				}
			}
		}
	}

	oldActive := engine.ActiveMap(oldSched)
	newActive := engine.ActiveMap(newSched)
	for tick := 1; tick <= maxTickSchedules(oldSched, newSched); tick++ {
		oldIDs := activeIDs(oldActive[tick])
		newIDs := activeIDs(newActive[tick])
		if equalStringSlices(oldIDs, newIDs) {
			continue
		}
		tickNode := fmt.Sprintf("scheduler-tick:%d", tick)
		addNode(closure, ClosureNode{ID: tickNode, Kind: "scheduler_tick"})
		addEdge(closure, objectID, tickNode, "guardrail/budget propagation may change admission")
		interval := model.Interval{StartTick: tick, EndTick: tick}
		intervals = append(intervals, interval)
		for _, req := range newP.Requests {
			if req.Tick != tick {
				continue
			}
			addRequestClosure(closure, sources, req.ID, firstPotentialHop(newP, req), objectID, tickNode,
				"scheduling_active_set", "schedule_changed",
				fmt.Sprintf("admitted instruction set at tick %d changed: old=%v new=%v", tick, oldIDs, newIDs), interval)
		}
	}

	closure.Intervals = mergeIntervals(intervals)
	closure.RequestIDs = keysOfSources(sources)
	closure.CacheKeys = make([]string, 0, len(closure.RequestIDs))
	for _, id := range closure.RequestIDs {
		req := findRequest(newP, id)
		closure.CacheKeys = append(closure.CacheKeys, cacheKey(newP.ProjectVersion, model.RequestResult{RequestID: id, Tick: req.Tick}))
	}
	sort.Strings(closure.RequestIDs)
	sort.Strings(closure.CacheKeys)
	sortClosure(closure)
	return closure, sources
}

func addRequestClosure(closure *Closure, sources map[string][]InvalidationSource, requestID string, hop int, objectID, parentNode, field, change, reason string, interval model.Interval) {
	reqNode := "request:" + requestID
	hopNode := fmt.Sprintf("%s:hop:%d", reqNode, hop)
	addNode(closure, ClosureNode{ID: reqNode, Kind: "request"})
	addNode(closure, ClosureNode{ID: hopNode, Kind: "hop"})
	addEdge(closure, parentNode, hopNode, reason)
	addEdge(closure, hopNode, reqNode, "delay accumulates and timeout may truncate all later hops")
	if _, exists := closure.FirstAffectedHop[requestID]; !exists || hop < closure.FirstAffectedHop[requestID] {
		closure.FirstAffectedHop[requestID] = hop
	}
	sources[requestID] = append(sources[requestID], InvalidationSource{
		ObjectID: objectID, ObjectKind: "chaos_object", Change: change, Field: field,
		Reason: reason, Interval: interval,
	})
}

func targetNodeForInstruction(p *model.Project, ins model.Instruction) ClosureNode {
	if ins.Selector.Kind == model.TargetService {
		return ClosureNode{ID: "service:" + ins.Selector.Service, Kind: "service"}
	}
	if ins.Selector.EdgeID != "" {
		return ClosureNode{ID: "edge:" + ins.Selector.EdgeID, Kind: "edge"}
	}
	return ClosureNode{ID: fmt.Sprintf("edge:%s->%s", ins.Selector.From, ins.Selector.To), Kind: "edge"}
}

func buildDiff(kind string, before, after *model.Report) *Diff {
	diff := &Diff{MutationKind: kind}
	beforeByID := map[string]model.RequestResult{}
	for _, rr := range before.Results {
		beforeByID[rr.RequestID] = rr
	}
	for _, afterRR := range after.Results {
		beforeRR := beforeByID[afterRR.RequestID]
		rd := RequestDiff{
			RequestID: afterRR.RequestID, BeforeStatus: beforeRR.Status, AfterStatus: afterRR.Status,
			BeforeTotalMS: beforeRR.TotalMS, AfterTotalMS: afterRR.TotalMS,
			TotalDeltaMS:   afterRR.TotalMS - beforeRR.TotalMS,
			BeforeHopCount: len(beforeRR.Hops), AfterHopCount: len(afterRR.Hops),
			HitAdded:   difference(afterRR.HitInstructionIDs, beforeRR.HitInstructionIDs),
			HitRemoved: difference(beforeRR.HitInstructionIDs, afterRR.HitInstructionIDs),
		}
		rd.Changed = !reflect.DeepEqual(beforeRR, afterRR) ||
			rd.BeforeStatus != rd.AfterStatus || rd.TotalDeltaMS != 0 ||
			rd.BeforeHopCount != rd.AfterHopCount || len(rd.HitAdded) > 0 || len(rd.HitRemoved) > 0
		if rd.Changed {
			diff.ChangedCount++
		}
		diff.Requests = append(diff.Requests, rd)
	}
	return diff
}

func difference(a, b []string) []string {
	bSet := map[string]bool{}
	for _, x := range b {
		bSet[x] = true
	}
	out := []string{}
	for _, x := range a {
		if !bSet[x] {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func addNode(c *Closure, node ClosureNode) {
	for _, n := range c.Nodes {
		if n.ID == node.ID {
			return
		}
	}
	c.Nodes = append(c.Nodes, node)
}

func addEdge(c *Closure, from, to, reason string) {
	for _, e := range c.Edges {
		if e.From == from && e.To == to && e.Reason == reason {
			return
		}
	}
	c.Edges = append(c.Edges, ClosureEdge{From: from, To: to, Reason: reason})
}

func sortClosure(c *Closure) {
	sort.Slice(c.Nodes, func(i, j int) bool { return c.Nodes[i].ID < c.Nodes[j].ID })
	sort.Slice(c.Edges, func(i, j int) bool {
		if c.Edges[i].From != c.Edges[j].From {
			return c.Edges[i].From < c.Edges[j].From
		}
		return c.Edges[i].To < c.Edges[j].To
	})
}

func mergeIntervals(in []model.Interval) []model.Interval {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool { return in[i].StartTick < in[j].StartTick })
	out := []model.Interval{in[0]}
	for _, iv := range in[1:] {
		last := &out[len(out)-1]
		if iv.StartTick <= last.EndTick+1 {
			if iv.EndTick > last.EndTick {
				last.EndTick = iv.EndTick
			}
		} else {
			out = append(out, iv)
		}
	}
	return out
}

func keysOfSources(m map[string][]InvalidationSource) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func findRequest(p *model.Project, id string) model.Request {
	for _, r := range p.Requests {
		if r.ID == id {
			return r
		}
	}
	return model.Request{ID: id}
}

func maxRequestTick(p *model.Project) int {
	max := 0
	for _, r := range p.Requests {
		if r.Tick > max {
			max = r.Tick
		}
	}
	return max
}

func maxTickSchedules(a, b []model.TickSchedule) int {
	max := 0
	for _, s := range a {
		if s.Tick > max {
			max = s.Tick
		}
	}
	for _, s := range b {
		if s.Tick > max {
			max = s.Tick
		}
	}
	return max
}

func activeIDs(m map[string]bool) []string {
	out := []string{}
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstPotentialHop(p *model.Project, req model.Request) int {
	if len(req.Path) >= 2 {
		return 0
	}
	return 0
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

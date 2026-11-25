// Package oracle is the single-threaded reference driver of the shared
// fncore state machines. Its output is the only truth the parallel engine,
// incremental recomputation and checkpoint recovery are checked against.
package oracle

import (
	"sort"

	"coldkernel/internal/fncore"
	"coldkernel/internal/model"
)

// Simulate runs the full reference simulation and returns every per-tick
// verdict plus the per-function observable state.
func Simulate(p *model.Project) *model.RunResult {
	cores := map[string]*fncore.Core{}
	for _, fn := range p.Functions {
		cores[fn.ID] = fncore.New(fn, p.Seed)
	}

	arrivals := map[int64][]*model.Request{}
	for _, r := range p.Requests {
		arrivals[r.ArriveTick] = append(arrivals[r.ArriveTick], r)
	}

	out := &model.RunResult{
		ByRequest:   map[string]*model.RequestOutcome{},
		ToleranceOK: true,
	}

	for tick := int64(0); tick < p.HorizonTicks; tick++ {
		tr := &model.TickResult{Tick: tick, Functions: map[string]*model.FunctionTickState{}}
		for _, fn := range p.Functions {
			c := cores[fn.ID]
			c.BeginTick(tick)
			tr.Rejected = append(tr.Rejected, c.ExpireQueue(tick)...)
			adm, rej := c.DrainQueue(tick)
			tr.Admitted = append(tr.Admitted, adm...)
			tr.Rejected = append(tr.Rejected, rej...)
			for _, r := range arrivals[tick] {
				if r.FunctionID != fn.ID {
					continue
				}
				oc := c.HandleArrival(r, tick)
				record(tr, oc)
			}
		}
		for _, fn := range p.Functions {
			tr.Functions[fn.ID] = cores[fn.ID].Snapshot(tick)
		}
		SortOutcomes(tr.Admitted)
		SortOutcomes(tr.Queued)
		SortOutcomes(tr.Rejected)
		for _, oc := range allOutcomes(tr) {
			out.ByRequest[oc.RequestID] = oc
		}
		out.Ticks = append(out.Ticks, tr)
	}
	return out
}

func record(tr *model.TickResult, oc *model.RequestOutcome) {
	switch oc.Decision {
	case model.DecisionAdmit:
		tr.Admitted = append(tr.Admitted, oc)
	case model.DecisionQueue:
		tr.Queued = append(tr.Queued, oc)
	default:
		tr.Rejected = append(tr.Rejected, oc)
	}
}

func allOutcomes(tr *model.TickResult) []*model.RequestOutcome {
	var all []*model.RequestOutcome
	all = append(all, tr.Admitted...)
	all = append(all, tr.Queued...)
	all = append(all, tr.Rejected...)
	return all
}

// SortOutcomes canonicalises verdict ordering so comparisons are stable.
func SortOutcomes(os []*model.RequestOutcome) {
	sort.SliceStable(os, func(i, j int) bool { return os[i].RequestID < os[j].RequestID })
}

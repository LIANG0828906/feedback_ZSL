package budget

import (
	"fmt"
	"sort"

	"samplingkernel/internal/model"
)

const (
	RejectGlobal  = "E_BUDGET_GLOBAL_EXHAUSTED"
	RejectService = "E_BUDGET_SERVICE_EXHAUSTED"
)

type ServiceState struct {
	LimitSpans   uint64 `json:"limit_spans"`
	ChargedSpans uint64 `json:"charged_spans"`
	ForcedSpans  uint64 `json:"forced_spans"`
}

type WindowState struct {
	WindowStartMS int64                    `json:"window_start_ms"`
	GlobalUsed    uint64                   `json:"global_used"`
	Services      map[string]*ServiceState `json:"services"`
}

type Ledger struct {
	project *model.Project
	windows map[int64]*WindowState
	current int64
}

func NewLedger(project *model.Project) *Ledger {
	return &Ledger{project: project, windows: map[int64]*WindowState{}}
}

func (l *Ledger) Clone() *Ledger {
	cp := NewLedger(l.project)
	cp.current = l.current
	for start, window := range l.windows {
		wcp := &WindowState{WindowStartMS: window.WindowStartMS, GlobalUsed: window.GlobalUsed, Services: map[string]*ServiceState{}}
		for id, svc := range window.Services {
			svcCopy := *svc
			wcp.Services[id] = &svcCopy
		}
		cp.windows[start] = wcp
	}
	return cp
}

func (l *Ledger) windowStart(timestampMS int64) int64 {
	w := l.project.Budget.WindowMS
	if w <= 0 {
		w = 1000
	}
	return timestampMS - (timestampMS % w)
}

func (l *Ledger) Window(start int64) *WindowState {
	return l.windows[start]
}

func CloneWindow(window *WindowState) *WindowState {
	if window == nil {
		return nil
	}
	cp := &WindowState{WindowStartMS: window.WindowStartMS, GlobalUsed: window.GlobalUsed, Services: map[string]*ServiceState{}}
	for id, state := range window.Services {
		stateCopy := *state
		cp.Services[id] = &stateCopy
	}
	return cp
}

func (l *Ledger) SnapshotWindow(start int64) *WindowState {
	return CloneWindow(l.windows[start])
}

func (l *Ledger) AdoptWindow(window *WindowState) {
	if window == nil {
		return
	}
	l.windows[window.WindowStartMS] = CloneWindow(window)
	l.current = window.WindowStartMS
}

func (l *Ledger) States() map[int64]*WindowState {
	return l.windows
}

func (l *Ledger) Decide(eventSeq int, timestampMS int64, traceID, serviceID string, spanCounts map[string]int, totalSpans int, forced bool) model.BudgetDecision {
	start := l.windowStart(timestampMS)
	end := start + l.project.Budget.WindowMS
	basis := "window_initialized"
	if l.current != 0 || len(l.windows) > 0 {
		if previous, ok := l.windows[l.current]; ok && l.current != start {
			basis = fmt.Sprintf("rollover floor(%d/%d): %d -> %d; prior_used=%d", timestampMS, l.project.Budget.WindowMS, previous.WindowStartMS, start, previous.GlobalUsed)
		} else if l.current == start {
			basis = fmt.Sprintf("same_window floor(%d/%d)=%d", timestampMS, l.project.Budget.WindowMS, start)
		}
	}
	l.current = start
	window, ok := l.windows[start]
	if !ok {
		window = &WindowState{WindowStartMS: start, Services: map[string]*ServiceState{}}
		for _, allocation := range l.project.Allocations {
			limit := l.project.Budget.GlobalLimitSpans * uint64(allocation.WeightPermyriad) / 10000
			window.Services[allocation.ServiceID] = &ServiceState{LimitSpans: limit}
		}
		l.windows[start] = window
	}

	decision := model.BudgetDecision{
		EventSeq:         eventSeq,
		TraceID:          traceID,
		ServiceID:        serviceID,
		WindowStartMS:    start,
		WindowEndMS:      end,
		WindowBasis:      basis,
		CostSpans:        totalSpans,
		GlobalLimitSpans: l.project.Budget.GlobalLimitSpans,
		GlobalUsedSpans:  window.GlobalUsed,
		GlobalRemaining:  int64(l.project.Budget.GlobalLimitSpans) - int64(window.GlobalUsed),
		Accepted:         true,
	}

	serviceOrder := map[string]int{}
	for i, service := range l.project.Services {
		serviceOrder[service.ID] = i
	}

	globalShortage := totalSpans > 0 && window.GlobalUsed+uint64(totalSpans) > l.project.Budget.GlobalLimitSpans
	if !forced {
		if globalShortage && l.project.Budget.GlobalExhaustionRule != model.GlobalExhaustAllowOvershoot {
			decision.Accepted = false
			decision.RejectCode = RejectGlobal
			decision.RejectReason = fmt.Sprintf("global budget requires %d spans but %d remain in window %d", totalSpans, decision.GlobalRemaining, start)
		}

		type shortage struct {
			serviceID string
			remaining int64
			needed    int
			order     int
		}
		var shortages []shortage
		for svcID, count := range spanCounts {
			state := window.Services[svcID]
			if state == nil {
				continue
			}
			remaining := int64(state.LimitSpans) - int64(state.ChargedSpans)
			if int64(count) > remaining {
				shortages = append(shortages, shortage{serviceID: svcID, remaining: remaining, needed: count, order: serviceOrder[svcID]})
			}
		}
		sort.Slice(shortages, func(i, j int) bool {
			if shortages[i].order != shortages[j].order {
				return shortages[i].order < shortages[j].order
			}
			return shortages[i].serviceID < shortages[j].serviceID
		})
		if decision.Accepted && len(shortages) > 0 {
			first := shortages[0]
			decision.Accepted = false
			decision.RejectCode = RejectService
			decision.ServiceID = first.serviceID
			decision.RejectReason = fmt.Sprintf("service allocation requires %d spans but %d remain", first.needed, first.remaining)
		}
	}

	if decision.Accepted {
		window.GlobalUsed += uint64(totalSpans)
		decision.Charged = true
		if forced {
			for svcID, count := range spanCounts {
				state := window.Services[svcID]
				if state == nil {
					continue
				}
				state.ForcedSpans += uint64(count)
			}
			if decision.GlobalRemaining < int64(totalSpans) {
				decision.Overshot = true
			}
		} else {
			for svcID, count := range spanCounts {
				state := window.Services[svcID]
				if state == nil {
					continue
				}
				state.ChargedSpans += uint64(count)
			}
			if l.project.Budget.GlobalExhaustionRule == model.GlobalExhaustAllowOvershoot && globalShortage {
				decision.Overshot = true
			}
		}
	}

	decision.GlobalUsedSpans = window.GlobalUsed
	decision.GlobalRemaining = int64(l.project.Budget.GlobalLimitSpans) - int64(window.GlobalUsed)
	decision.Services = l.snapshots(window, decision)
	return decision
}

func (l *Ledger) snapshots(window *WindowState, decision model.BudgetDecision) []model.ServiceBudgetSnapshot {
	out := make([]model.ServiceBudgetSnapshot, 0, len(l.project.Services))
	for _, service := range l.project.Services {
		state := window.Services[service.ID]
		if state == nil {
			limit := l.project.Budget.GlobalLimitSpans
			for _, allocation := range l.project.Allocations {
				if allocation.ServiceID == service.ID {
					limit = l.project.Budget.GlobalLimitSpans * uint64(allocation.WeightPermyriad) / 10000
				}
			}
			state = &ServiceState{LimitSpans: limit}
		}
		snapshot := model.ServiceBudgetSnapshot{
			ServiceID:      service.ID,
			LimitSpans:     state.LimitSpans,
			ChargedSpans:   state.ChargedSpans,
			ForcedSpans:    state.ForcedSpans,
			RemainingSpans: int64(state.LimitSpans) - int64(state.ChargedSpans),
		}
		if !decision.Accepted && decision.RejectCode == RejectService && decision.ServiceID == service.ID {
			snapshot.RejectedTraceID = decision.TraceID
			snapshot.RejectReason = decision.RejectReason
		}
		out = append(out, snapshot)
	}
	return out
}

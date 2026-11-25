package app

import (
	"fmt"

	"maintenanceorchestrator/internal/domain"
	"maintenanceorchestrator/internal/gate"
	"maintenanceorchestrator/internal/lease"
	"maintenanceorchestrator/internal/parallel"
	"maintenanceorchestrator/internal/solver"
)

type Analysis struct {
	Project         domain.Project
	Feasibility     []solver.TickReport
	ParallelReports []solver.TickReport
	Plan            solver.WavePlan
	FreedCapacity   map[string]int64
	LeaseTimeline   *lease.Timeline
	Gate            gate.Result
}

func Analyze(p domain.Project) (*Analysis, error) {
	if err := domain.ValidateProject(p); err != nil {
		return nil, err
	}
	a := &Analysis{Project: p, FreedCapacity: map[string]int64{}}
	for _, tick := range p.ProbeTicks {
		report := solver.EvaluateTick(solver.FeasibilityOptions{Project: p, Tick: tick})
		a.Feasibility = append(a.Feasibility, report)
	}
	parallelReports, errs := FeasibilityParallel(p)
	if len(errs) > 0 {
		return nil, errs[0]
	}
	a.ParallelReports = parallelReports

	a.Plan = solver.PlanWaves(p)
	a.FreedCapacity = gate.FreedCapacity(p, a.Plan)
	tl, err := lease.Settle(lease.Options{
		Project: p, Placement: a.Plan.Placement,
		Plan:          &a.Plan,
		FreedCapacity: a.FreedCapacity, Ticks: p.ProbeTicks,
	})
	if err != nil {
		return nil, err
	}
	preliminaryGate := gate.Plan(p, a.Plan, tl)
	returns := returnCompletions(p, a.Plan, preliminaryGate)
	tl, err = lease.Settle(lease.Options{
		Project: p, Placement: a.Plan.Placement, Plan: &a.Plan, Returns: returns,
		FreedCapacity: a.FreedCapacity, Ticks: p.ProbeTicks,
	})
	if err != nil {
		return nil, err
	}
	a.LeaseTimeline = tl
	a.Gate = gate.Plan(p, a.Plan, tl)
	return a, nil
}

func returnCompletions(p domain.Project, plan solver.WavePlan, result gate.Result) []lease.ReturnCompletion {
	targetByInstance := map[string]string{}
	for _, m := range plan.Migrations {
		targetByInstance[m.InstanceID] = m.TargetNodeID
	}
	sizeByInstance := map[string]int64{}
	for _, in := range p.Instances {
		sizeByInstance[in.ID] = in.MilliCores
	}
	var out []lease.ReturnCompletion
	for _, step := range result.Steps {
		if step.Kind != gate.StepReturnComplete {
			continue
		}
		out = append(out, lease.ReturnCompletion{
			Tick: step.Tick, InstanceID: step.InstanceID, HomeNodeID: step.NodeID,
			FromNodeID: targetByInstance[step.InstanceID], MilliCores: sizeByInstance[step.InstanceID],
		})
	}
	return out
}

func FeasibilityParallel(p domain.Project) ([]solver.TickReport, []error) {
	return parallel.MapIndexed(p.ProbeTicks, p.Policies.ThreadBudget, p.Policies.PerNodeQueueLimit,
		func(t domain.Tick) string { return fmt.Sprintf("tick-%d", t) },
		func(t domain.Tick) (solver.TickReport, error) {
			return solver.EvaluateTick(solver.FeasibilityOptions{Project: p, Tick: t}), nil
		})
}

func EventPendingAt(p domain.Project, tick domain.Tick) []string {
	active := map[string]bool{}
	for _, e := range p.Events {
		switch e.Kind {
		case domain.EventMarkPending:
			if e.Tick <= tick {
				active[e.NodeID] = true
			}
		case domain.EventReturned:
			if e.Tick <= tick {
				delete(active, e.NodeID)
			}
		}
	}
	out := make([]string, 0, len(active))
	for id := range active {
		out = append(out, id)
	}
	return out
}

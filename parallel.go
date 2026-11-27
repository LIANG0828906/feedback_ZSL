package planopt

import (
	"context"
	"sort"
	"sync"
)

type SchedulerOptions struct {
	WorkerBudget        int `json:"worker_budget"`
	MaxOperatorsPerPlan int `json:"max_operators_per_plan"`
}

type Scheduler struct {
	opts  SchedulerOptions
	slots chan struct{}
}

type PlanOutcome struct {
	PlanSeq int           `json:"plan_seq"`
	PlanID  string        `json:"plan_id"`
	Result  OptimizedPlan `json:"result,omitempty"`
	Err     *Error        `json:"error,omitempty"`
}

func NewScheduler(workers, maxOperatorsPerPlan int) *Scheduler {
	if workers <= 0 {
		workers = 1
	}
	if maxOperatorsPerPlan <= 0 {
		maxOperatorsPerPlan = 1
	}
	return &Scheduler{
		opts:  SchedulerOptions{WorkerBudget: workers, MaxOperatorsPerPlan: maxOperatorsPerPlan},
		slots: make(chan struct{}, workers),
	}
}

func (s *Scheduler) Options() SchedulerOptions { return s.opts }

// AcquireWorker occupies one worker slot. The returned release function is
// idempotent. Tests and embedders can use it to simulate a saturated pool.
func (s *Scheduler) AcquireWorker() (func(), error) {
	select {
	case s.slots <- struct{}{}:
		released := false
		return func() {
			if !released {
				released = true
				<-s.slots
			}
		}, nil
	default:
		return nil, NewError(CodeBackpressureWorkers, "scheduler.worker_budget", "all declared worker slots are occupied")
	}
}

// TryProcessPlan applies immediate backpressure instead of queueing forever
// when no worker is available or the plan exceeds its operator buffer cap.
func (s *Scheduler) TryProcessPlan(project Project, plan Plan, bindings BindingSet) (OptimizedPlan, error) {
	if len(plan.Nodes) > s.opts.MaxOperatorsPerPlan {
		return OptimizedPlan{}, Errorf(CodeBackpressureBuffer, "scheduler.operator_buffer", "plan %s has %d nodes > buffer limit %d", plan.ID, len(plan.Nodes), s.opts.MaxOperatorsPerPlan)
	}
	release, err := s.AcquireWorker()
	if err != nil {
		return OptimizedPlan{}, err
	}
	defer release()
	normalized, err := NormalizePlan(project, plan)
	if err != nil {
		return OptimizedPlan{}, err
	}
	if len(normalized.Operators) > s.opts.MaxOperatorsPerPlan {
		return OptimizedPlan{}, Errorf(CodeBackpressureBuffer, "scheduler.normalized_buffer", "plan %s has %d operators > buffer limit %d", plan.ID, len(normalized.Operators), s.opts.MaxOperatorsPerPlan)
	}
	return optimizePlan(project, plan, normalized, bindings)
}

// RunAll executes plans with at most WorkerBudget active optimizations. Outcomes
// are always returned in plan_seq order regardless of goroutine scheduling.
func (s *Scheduler) RunAll(ctx context.Context, project Project, bindings BindingSet) []PlanOutcome {
	plans := project.SortedPlans()
	outcomes := make([]PlanOutcome, len(plans))
	sem := make(chan struct{}, s.opts.WorkerBudget)
	var wg sync.WaitGroup

	for i, plan := range plans {
		if len(plan.Nodes) > s.opts.MaxOperatorsPerPlan {
			e := Errorf(CodeBackpressureBuffer, "scheduler.operator_buffer", "plan %s has %d nodes > buffer limit %d", plan.ID, len(plan.Nodes), s.opts.MaxOperatorsPerPlan)
			outcomes[i] = PlanOutcome{PlanSeq: plan.Seq, PlanID: plan.ID, Err: AsPlanError(e)}
			continue
		}
		select {
		case <-ctx.Done():
			e := Errorf(CodeBackpressureWorkers, "scheduler.context", "%v", ctx.Err())
			outcomes[i] = PlanOutcome{PlanSeq: plan.Seq, PlanID: plan.ID, Err: AsPlanError(e)}
			continue
		default:
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			e := Errorf(CodeBackpressureWorkers, "scheduler.context", "%v", ctx.Err())
			outcomes[i] = PlanOutcome{PlanSeq: plan.Seq, PlanID: plan.ID, Err: AsPlanError(e)}
			continue
		}

		wg.Add(1)
		go func(index int, current Plan) {
			defer wg.Done()
			defer func() { <-sem }()
			outcome := PlanOutcome{PlanSeq: current.Seq, PlanID: current.ID}
			normalized, err := NormalizePlan(project, current)
			if err == nil && len(normalized.Operators) > s.opts.MaxOperatorsPerPlan {
				err = Errorf(CodeBackpressureBuffer, "scheduler.normalized_buffer", "plan %s has %d operators > buffer limit %d", current.ID, len(normalized.Operators), s.opts.MaxOperatorsPerPlan)
			}
			if err == nil {
				var opt OptimizedPlan
				opt, err = optimizePlan(project, current, normalized, bindings)
				outcome.Result = opt
			}
			if err != nil {
				outcome.Err = AsPlanError(err)
			}
			outcomes[index] = outcome
		}(i, plan)
	}
	wg.Wait()

	sort.SliceStable(outcomes, func(i, j int) bool { return outcomes[i].PlanSeq < outcomes[j].PlanSeq })
	return outcomes
}

func AsPlanError(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	return NewError(CodeInvariant, "scheduler.unknown", err.Error())
}

// SuccessfulResults returns successful outcomes sorted by sequence.
func SuccessfulResults(outcomes []PlanOutcome) []OptimizedPlan {
	out := []OptimizedPlan{}
	for _, o := range outcomes {
		if o.Err == nil {
			out = append(out, o.Result)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PlanSeq < out[j].PlanSeq })
	return out
}

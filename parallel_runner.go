package hckernel

import "context"

type ParallelRunReport struct {
	Result  *Result        `json:"result"`
	Batches []*BatchResult `json:"batches"`
}

// RunParallel executes the deterministic round planner and fans out each
// round's probes through the bounded executor. Health/event reduction remains
// canonical and ordered; probe completion order cannot change the Result.
func RunParallel(
	ctx context.Context,
	project *Project,
	events []Event,
	handler func(context.Context, Probe) (ProbeOutput, error),
) (*ParallelRunReport, error) {
	engine, err := NewEngine(project, events)
	if err != nil {
		return nil, err
	}
	executor, err := NewParallelExecutor(project.Workers, project.BufferCapacity)
	if err != nil {
		return nil, err
	}
	out := &ParallelRunReport{Batches: []*BatchResult{}}
	for round := 1; round <= project.Rounds; round++ {
		now := int64(round-1) * project.StepMs
		if err := engine.Advance(now, round); err != nil {
			return nil, err
		}
		planned := engine.reports[len(engine.reports)-1].Dispatched
		batch, err := executor.ExecuteBatch(ctx, round, planned, project.Workers, handler)
		if err != nil {
			return nil, err
		}
		out.Batches = append(out.Batches, batch)
	}
	out.Result = engine.Result()
	return out, nil
}

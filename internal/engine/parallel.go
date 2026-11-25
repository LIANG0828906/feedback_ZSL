package engine

import (
	"context"
	"sort"

	"credrot/internal/bizerr"
	"credrot/internal/hashutil"
	"credrot/internal/model"
	"credrot/internal/project"
)

type ParallelOptions struct {
	Workers                      int `json:"workers"`
	PendingBufferPerSubscription int `json:"pending_buffer_per_subscription"`
}

type BackpressureEvent struct {
	Kind           string `json:"kind"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	Required       int    `json:"required,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	QueuedJobs     int    `json:"queued_jobs,omitempty"`
	Action         string `json:"action"`
}

type ParallelResult struct {
	Validity     []SubscriptionValidity `json:"validity"`
	Propagation  *PropagationResult     `json:"propagation"`
	Backpressure []BackpressureEvent    `json:"backpressure"`
	WorkersUsed  int                    `json:"workers_used"`
	Digest       string                 `json:"digest"`
}

type validityJob struct {
	index int
	sub   model.Subscription
}

type validityJobResult struct {
	index  int
	values []SubscriptionValidity
	events []BackpressureEvent
	err    error
}

type propagationJob struct {
	index    int
	rotation model.Rotation
}

type propagationJobResult struct {
	index     int
	suspended SuspendedCredential
	err       error
}

func SolveParallel(ctx context.Context, p *model.Project, opts ParallelOptions) (*ParallelResult, error) {
	if p == nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project is nil", nil)
	}
	if opts.Workers <= 0 {
		return nil, bizerr.New(bizerr.CodeWorkerBudget, "worker budget must be positive", map[string]any{
			"workers": opts.Workers,
		})
	}
	if opts.PendingBufferPerSubscription <= 0 {
		return nil, bizerr.New(bizerr.CodeBufferBudget, "pending credential buffer must be positive", map[string]any{
			"pending_buffer_per_subscription": opts.PendingBufferPerSubscription,
		})
	}
	if err := project.Validate(p); err != nil {
		return nil, err
	}
	solver, err := NewSolver(p)
	if err != nil {
		return nil, err
	}

	validity, events, err := runValidityJobs(ctx, solver, opts)
	if err != nil {
		return nil, err
	}
	propagation, err := runPropagationJobs(ctx, p, opts, &events)
	if err != nil {
		return nil, err
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Kind != events[j].Kind {
			return events[i].Kind < events[j].Kind
		}
		if events[i].SubscriptionID != events[j].SubscriptionID {
			return events[i].SubscriptionID < events[j].SubscriptionID
		}
		if events[i].Required != events[j].Required {
			return events[i].Required < events[j].Required
		}
		return events[i].QueuedJobs < events[j].QueuedJobs
	})
	result := &ParallelResult{
		Validity:     validity,
		Propagation:  propagation,
		Backpressure: events,
		WorkersUsed:  opts.Workers,
	}
	digest, err := hashutil.JSONSHA256(struct {
		Validity    []SubscriptionValidity `json:"validity"`
		Propagation *PropagationResult     `json:"propagation"`
	}{validity, propagation})
	if err != nil {
		return nil, bizerr.Wrap(bizerr.CodeInternal, "cannot calculate deterministic result digest", err, nil)
	}
	result.Digest = digest
	return result, nil
}

func runValidityJobs(ctx context.Context, solver *Solver, opts ParallelOptions) ([]SubscriptionValidity, []BackpressureEvent, error) {
	subs := append([]model.Subscription(nil), solver.project.Subscriptions...)
	sort.Slice(subs, func(i, j int) bool { return subs[i].ID < subs[j].ID })
	jobs := make([]validityJob, len(subs))
	for i, sub := range subs {
		jobs[i] = validityJob{index: i, sub: sub}
	}
	workers := minInt(opts.Workers, len(jobs))
	if workers == 0 {
		return []SubscriptionValidity{}, []BackpressureEvent{}, nil
	}
	done := make(chan validityJobResult, workers)
	results := make([][]SubscriptionValidity, len(jobs))
	events := make([]BackpressureEvent, 0)
	inFlight := 0
	next := 0
	for next < len(jobs) || inFlight > 0 {
		for next < len(jobs) && inFlight < workers {
			job := jobs[next]
			go func(j validityJob) {
				done <- runValidityJob(ctx, solver, j, opts.PendingBufferPerSubscription)
			}(job)
			next++
			inFlight++
		}
		if inFlight == workers && next < len(jobs) {
			events = append(events, BackpressureEvent{
				Kind:       "worker_budget",
				QueuedJobs: len(jobs) - next,
				Limit:      workers,
				Action:     "wait_for_worker_slot",
			})
		}
		if inFlight == 0 {
			continue
		}
		res := <-done
		inFlight--
		if res.err != nil {
			return nil, nil, res.err
		}
		results[res.index] = res.values
		events = append(events, res.events...)
	}
	out := make([]SubscriptionValidity, 0, solver.project.TickCount()*int64(len(jobs)))
	for _, values := range results {
		out = append(out, values...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tick != out[j].Tick {
			return out[i].Tick < out[j].Tick
		}
		return out[i].SubscriptionID < out[j].SubscriptionID
	})
	return out, events, nil
}

func runValidityJob(ctx context.Context, solver *Solver, job validityJob, bufferLimit int) validityJobResult {
	result := validityJobResult{index: job.index}
	if len(job.sub.Refs) <= bufferLimit {
		for tick := int64(0); tick <= solver.project.HorizonTicks; tick++ {
			if err := ctx.Err(); err != nil {
				result.err = err
				return result
			}
			value, err := solver.validityForSubscriptionObject(tick, job.sub)
			if err != nil {
				result.err = err
				return result
			}
			result.values = append(result.values, value)
		}
		return result
	}
	result.events = append(result.events, BackpressureEvent{
		Kind:           "credential_buffer",
		SubscriptionID: job.sub.ID,
		Required:       len(job.sub.Refs),
		Limit:          bufferLimit,
		Action:         "chunk_processed_without_overflow",
	})
	chunks := make([][]model.CredentialRef, 0, (len(job.sub.Refs)+bufferLimit-1)/bufferLimit)
	for start := 0; start < len(job.sub.Refs); start += bufferLimit {
		end := start + bufferLimit
		if end > len(job.sub.Refs) {
			end = len(job.sub.Refs)
		}
		chunks = append(chunks, job.sub.Refs[start:end])
	}
	for tick := int64(0); tick <= solver.project.HorizonTicks; tick++ {
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
		}
		merged := SubscriptionValidity{
			Tick:           tick,
			NowMillis:      solver.project.TickToMillis(tick),
			SubscriptionID: job.sub.ID,
			Active:         job.sub.Active,
			Available:      []AvailableCredential{},
			States:         []CredentialState{},
		}
		for _, chunk := range chunks {
			chunkSub := job.sub
			chunkSub.Refs = chunk
			value, err := solver.validityForSubscriptionObject(tick, chunkSub)
			if err != nil {
				result.err = err
				return result
			}
			merged.Available = append(merged.Available, value.Available...)
			merged.States = append(merged.States, value.States...)
		}
		sort.Slice(merged.Available, func(i, j int) bool {
			if merged.Available[i].Version != merged.Available[j].Version {
				return merged.Available[i].Version > merged.Available[j].Version
			}
			return merged.Available[i].CredentialID < merged.Available[j].CredentialID
		})
		sort.Slice(merged.States, func(i, j int) bool {
			return merged.States[i].CredentialID < merged.States[j].CredentialID
		})
		result.values = append(result.values, merged)
	}
	return result
}

func runPropagationJobs(ctx context.Context, p *model.Project, opts ParallelOptions, events *[]BackpressureEvent) (*PropagationResult, error) {
	ctxData := newPropagationContext(p)
	jobs := make([]propagationJob, len(p.Rotations))
	for i, rotation := range p.Rotations {
		jobs[i] = propagationJob{index: i, rotation: rotation}
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].rotation.CredentialID < jobs[j].rotation.CredentialID
	})
	workers := minInt(opts.Workers, len(jobs))
	if workers == 0 {
		return &PropagationResult{Suspended: []SuspendedCredential{}}, nil
	}
	done := make(chan propagationJobResult, workers)
	results := make([]SuspendedCredential, len(jobs))
	inFlight := 0
	next := 0
	for next < len(jobs) || inFlight > 0 {
		for next < len(jobs) && inFlight < workers {
			job := jobs[next]
			go func(j propagationJob) {
				suspended, err := ctxData.one(j.rotation)
				done <- propagationJobResult{index: j.index, suspended: suspended, err: err}
			}(job)
			next++
			inFlight++
		}
		if inFlight == workers && next < len(jobs) {
			*events = append(*events, BackpressureEvent{
				Kind:       "worker_budget",
				QueuedJobs: len(jobs) - next,
				Limit:      workers,
				Action:     "wait_for_worker_slot",
			})
		}
		if inFlight == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res := <-done:
			inFlight--
			if res.err != nil {
				return nil, res.err
			}
			results[res.index] = res.suspended
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].CredentialID < results[j].CredentialID
	})
	return &PropagationResult{Suspended: results}, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Package pipeline wires the deterministic kernel, scheduler, cache and checkpoints.
package pipeline

import (
	"context"
	"reflect"

	"wardrobekernel/internal/assembly"
	"wardrobekernel/internal/batching"
	"wardrobekernel/internal/cache"
	"wardrobekernel/internal/checkpoint"
	"wardrobekernel/internal/compare"
	"wardrobekernel/internal/depgraph"
	"wardrobekernel/internal/model"
	"wardrobekernel/internal/scheduler"
	"wardrobekernel/internal/xerr"
)

func modelError(code, message string) error { return xerr.New(code, message) }

func errorAsXErr(err error) (*xerr.Error, bool) { return xerr.AsError(err) }

// FullPlan is the one-shot empty-cache/full-recompute path.
func FullPlan(project *model.Project) (*model.Plan, error) {
	plan, err := batching.Plan(project)
	if err != nil {
		return nil, err
	}
	depgraph.Enrich(project, plan)
	return plan, nil
}

// TruthAsPlan converts loaded truth to the comparator's plan shape.
func TruthAsPlan(truth *model.Truth) *model.Plan {
	return &model.Plan{
		AlgorithmVersion: truth.AlgorithmVersion,
		Assembly:         truth.Assembly,
		Batches:          truth.Batches,
		Decisions:        truth.Decisions,
		CommitSequence:   truth.CommitSequence,
	}
}

// VerifyTruth compares a freshly computed plan against checked-in truth.
func VerifyTruth(project *model.Project, truth *model.Truth) (*model.Plan, error) {
	plan, err := FullPlan(project)
	if err != nil {
		return nil, err
	}
	if err := compare.Plans(TruthAsPlan(truth), plan); err != nil {
		return plan, err
	}
	if !reflect.DeepEqual(truth.Decisions, plan.Decisions) {
		return plan, &xerr.Error{
			Code: xerr.EDiff, Message: "truth arbitration decisions differ from fresh plan",
			FirstMismatch: &xerr.Mismatch{
				BatchNumber: -1, FieldName: "decisions",
				Expected: truth.Decisions, Actual: plan.Decisions,
			},
		}
	}
	return plan, nil
}

// RealtimeOptions controls bounded preparation and durable commits.
type RealtimeOptions struct {
	Scheduler       scheduler.Options
	Checkpoint      *checkpoint.Store
	KillAfterCommit int
	Kill            func()
}

// RealtimeResult contains the canonical plan plus runtime diagnostics.
type RealtimeResult struct {
	Plan      *model.Plan
	Scheduled *scheduler.Result
	Commits   []model.CommitRecord
}

// RunRealtime executes bounded parallel preparation, deterministic reduction,
// and optional sequential checkpoint commits.
func RunRealtime(ctx context.Context, project *model.Project, opts RealtimeOptions) (*RealtimeResult, error) {
	asm, err := assembly.Evaluate(project, project.CandidateAssembly)
	if err != nil {
		return nil, err
	}
	scheduled, err := scheduler.Prepare(ctx, project, asm.LegalAssembly, opts.Scheduler)
	if err != nil {
		return nil, err
	}

	prepared := map[string]struct{}{}
	for _, p := range scheduled.Prepared {
		prepared[p.Part.ID] = struct{}{}
	}
	effectiveLegal := make([]string, 0, len(asm.LegalAssembly))
	for _, id := range asm.LegalAssembly {
		if _, ok := prepared[id]; ok {
			effectiveLegal = append(effectiveLegal, id)
		}
	}
	effectiveAssembly := *asm
	effectiveAssembly.LegalAssembly = effectiveLegal

	plan, err := batching.PlanLegalAssembly(project, &effectiveAssembly)
	if err != nil {
		return nil, err
	}
	depgraph.Enrich(project, plan)

	commits := []model.CommitRecord{}
	if opts.Checkpoint != nil {
		for _, batch := range plan.Batches {
			commit, err := opts.Checkpoint.CommitBatch(batch)
			if err != nil {
				return nil, err
			}
			commits = append(commits, commit)
			if opts.KillAfterCommit > 0 && commit.Sequence == opts.KillAfterCommit {
				if opts.Kill != nil {
					opts.Kill()
				}
			}
		}
	}
	return &RealtimeResult{Plan: plan, Scheduled: scheduled, Commits: commits}, nil
}

// ResumeResult is the recovered-path output.
type ResumeResult struct {
	Plan             *model.Plan
	RecoveredBatches []int
	NewCommits       []model.CommitRecord
}

// ResumeAndContinue loads durable batches, recomputes the deterministic tail,
// and commits missing batches in order.
func ResumeAndContinue(ctx context.Context, project *model.Project, store *checkpoint.Store, schedOpts scheduler.Options) (*ResumeResult, error) {
	target, err := RunRealtime(ctx, project, RealtimeOptions{Scheduler: schedOpts})
	if err != nil {
		return nil, err
	}
	persisted := store.Batches()
	recovered := make([]int, 0, len(persisted))
	byNumber := map[int]model.Batch{}
	for _, batch := range persisted {
		recovered = append(recovered, batch.Number)
		byNumber[batch.Number] = batch
	}

	newCommits := []model.CommitRecord{}
	for _, batch := range target.Plan.Batches {
		if _, ok := byNumber[batch.Number]; ok {
			continue
		}
		commit, err := store.CommitBatch(batch)
		if err != nil {
			return nil, err
		}
		newCommits = append(newCommits, commit)
		byNumber[batch.Number] = batch
	}

	batches := make([]model.Batch, 0, len(target.Plan.Batches))
	sequence := make([]int, 0, len(target.Plan.Batches))
	for _, expected := range target.Plan.Batches {
		batch, ok := byNumber[expected.Number]
		if !ok {
			batch = expected
		}
		batches = append(batches, batch)
		sequence = append(sequence, expected.Number)
	}
	return &ResumeResult{
		Plan: &model.Plan{
			AlgorithmVersion: target.Plan.AlgorithmVersion,
			Assembly:         target.Plan.Assembly,
			Batches:          batches,
			Decisions:        target.Plan.Decisions,
			CommitSequence:   sequence,
		},
		RecoveredBatches: recovered,
		NewCommits:       newCommits,
	}, nil
}

// IncrementalResult reports cache reuse, propagation and full-path comparison.
type IncrementalResult struct {
	Plan         *model.Plan
	FullPlan     *model.Plan
	Graph        *depgraph.Graph
	Invalidation depgraph.Invalidation
	Comparison   *model.ComparisonReport
	FallbackFull bool
}

// IncrementalReplan reuses unaffected cache batches, rebuilds invalid batches
// in topological order, and compares against empty-cache full recomputation.
func IncrementalReplan(oldProject *model.Project, oldPlan *model.Plan, persistent *cache.PersistentCache, newProject *model.Project) (*IncrementalResult, error) {
	changedParts := depgraph.ChangedParts(oldProject, newProject)
	constraintsChanged := oldProject.ConstraintVersion != newProject.ConstraintVersion ||
		depgraph.ConstraintHash(oldProject.Constraints) != depgraph.ConstraintHash(newProject.Constraints)
	graph := depgraph.Enrich(oldProject, oldPlan)
	invalidation := graph.Propagate(changedParts, constraintsChanged)
	topo, err := graph.TopologicalBatches(invalidation.InvalidBatches)
	if err != nil {
		return nil, err
	}

	rebuilt := map[int]model.Batch{}
	fallback := false
	for _, number := range topo {
		var oldBatch *model.Batch
		for i := range oldPlan.Batches {
			if oldPlan.Batches[i].Number == number {
				oldBatch = &oldPlan.Batches[i]
				break
			}
		}
		if oldBatch == nil {
			fallback = true
			break
		}
		batch, err := batching.RebuildBatch(newProject, number, oldBatch.PartIDs)
		if err != nil {
			fallback = true
			break
		}
		rebuilt[number] = batch
	}

	var incremental *model.Plan
	if fallback {
		incremental, err = FullPlan(newProject)
		if err != nil {
			return nil, err
		}
		for _, batch := range incremental.Batches {
			if err := persistent.Put(batch); err != nil {
				return nil, err
			}
		}
	} else {
		batches := make([]model.Batch, 0, len(oldPlan.Batches))
		for _, oldBatch := range oldPlan.Batches {
			if batch, ok := rebuilt[oldBatch.Number]; ok {
				batches = append(batches, batch)
				continue
			}
			cached, ok := persistent.Batch(oldBatch.Number)
			if !ok {
				return nil, xerr.New(xerr.EState, "persistent cache miss for unaffected batch")
			}
			batches = append(batches, cached)
		}
		asm, err := assembly.Evaluate(newProject, newProject.CandidateAssembly)
		if err != nil {
			return nil, err
		}
		incremental = &model.Plan{
			AlgorithmVersion: newProject.AlgorithmVersion,
			Assembly:         *asm,
			Batches:          batches,
			CommitSequence:   append([]int(nil), oldPlan.CommitSequence...),
		}
		newGraph := depgraph.Enrich(newProject, incremental)
		for _, number := range topo {
			if number >= 1 && number <= len(incremental.Batches) {
				if err := persistent.Put(incremental.Batches[number-1]); err != nil {
					return nil, err
				}
			}
		}
		_ = newGraph
	}

	full, err := FullPlan(newProject)
	if err != nil {
		return nil, err
	}
	comparison := &model.ComparisonReport{Equal: true, Paths: []string{"incremental_cache", "full_empty_cache"}}
	if err := compare.Plans(full, incremental); err != nil {
		if coded, ok := errorAsXErr(err); ok {
			comparison.Equal = false
			comparison.FirstMismatch = coded.FirstMismatch
			return &IncrementalResult{
				Plan: incremental, FullPlan: full, Invalidation: invalidation,
				Comparison: comparison, FallbackFull: fallback,
			}, err
		}
		return nil, err
	}
	return &IncrementalResult{
		Plan: incremental, FullPlan: full, Graph: depgraph.Enrich(newProject, incremental),
		Invalidation: invalidation, Comparison: comparison, FallbackFull: fallback,
	}, nil
}

package engine

import (
	"sort"

	"layerforge/internal/apperr"
	"layerforge/internal/build"
	"layerforge/internal/checkpoint"
	"layerforge/internal/model"
)

func EditStep(project model.Project, stepNumber int, mutate func(*model.Step)) (model.Project, error) {
	if mutate == nil {
		return model.Project{}, apperr.WithSteps(apperr.New(apperr.Internal, "edit-step", "mutate", "edit function is required"), stepNumber)
	}
	out := project.Clone()
	found := false
	for index := range out.Steps {
		if out.Steps[index].Number == stepNumber {
			mutate(&out.Steps[index])
			found = true
			break
		}
	}
	if !found {
		return model.Project{}, apperr.WithSteps(apperr.New(apperr.GraphMissingRef, "edit-step", "steps", "step number does not exist"), stepNumber)
	}
	if _, err := build.BuildGraph(out); err != nil {
		return model.Project{}, err
	}
	return out, nil
}

func (e *Engine) Cleanup(candidates []string) (CleanupResult, error) {
	snapshot, err := e.Store.Load()
	if err != nil {
		return CleanupResult{}, err
	}
	if snapshot.ProjectID != e.Project.ID {
		return CleanupResult{}, apperr.New(apperr.RecoveryMismatch, "cleanup", e.Store.Path, "checkpoint belongs to a different project")
	}
	result := CleanupResult{
		RemovedLayers: make([]string, 0),
		KeptLayers:    make([]string, 0),
		RebuildSteps:  make([]int, 0),
		Decisions:     make([]Decision, 0),
		Snapshot:      snapshot,
	}
	seen := make(map[string]bool)
	var conflicts []string
	conflictSteps := make(map[int]bool)
	removable := make(map[string]bool)
	for _, layerID := range candidates {
		if seen[layerID] {
			continue
		}
		seen[layerID] = true
		node, ok := e.Graph.Layers[layerID]
		if !ok {
			conflicts = append(conflicts, layerID)
			continue
		}
		if node.ProducerStep == 0 {
			conflicts = append(conflicts, layerID)
			continue
		}
		if len(node.Consumers) > 0 {
			conflicts = append(conflicts, layerID)
			for _, consumer := range node.Consumers {
				conflictSteps[consumer] = true
			}
			continue
		}
		if _, exists := snapshot.Layers[layerID]; !exists {
			conflicts = append(conflicts, layerID)
			continue
		}
		removable[layerID] = true
	}
	if len(conflicts) > 0 {
		steps := make([]int, 0, len(conflictSteps))
		for step := range conflictSteps {
			steps = append(steps, step)
		}
		sort.Ints(steps)
		err := apperr.WithLayers(
			apperr.WithSteps(apperr.New(apperr.CleanupConflict, "cleanup", "candidates", "cleanup would remove a base or still-referenced layer"), steps...),
			conflicts...,
		)
		return result, err
	}
	if len(removable) == 0 {
		result.Decisions = e.decisionsForSnapshot(snapshot)
		return result, nil
	}

	// Removal is ordered index first, then layer state, so no persisted index
	// can point at a removed layer. The whole step is still one atomic commit.
	for cacheKey, entry := range snapshot.Index {
		if removable[entry.LayerID] {
			delete(snapshot.Index, cacheKey)
		}
	}
	for layerID := range removable {
		delete(snapshot.Layers, layerID)
		result.RemovedLayers = append(result.RemovedLayers, layerID)
		if step := e.Graph.Layers[layerID].ProducerStep; step > 0 {
			result.RebuildSteps = append(result.RebuildSteps, step)
		}
	}
	sort.Strings(result.RemovedLayers)
	sort.Ints(result.RebuildSteps)
	for layerID := range snapshot.Layers {
		if !removable[layerID] && e.Graph.Layers[layerID].ProducerStep > 0 {
			result.KeptLayers = append(result.KeptLayers, layerID)
		}
	}
	sort.Strings(result.KeptLayers)
	snapshot.Generation++
	if err := snapshot.Validate(); err != nil {
		return result, err
	}
	if err := e.Store.CommitRound(snapshot); err != nil {
		return result, err
	}
	result.Decisions = e.decisionsForSnapshot(snapshot)
	return result, nil
}

func (e *Engine) decisionsForSnapshot(snapshot *checkpoint.Snapshot) []Decision {
	decisions := make(map[int]Decision, len(e.Graph.Steps))
	for _, number := range sortedNumbers(e.Graph.Steps) {
		step := e.Graph.Steps[number]
		derivation := e.Derivation.Steps[number]
		decision := Decision{
			Step:        number,
			Name:        step.Name,
			LayerID:     derivation.LayerID,
			CacheKey:    derivation.CacheKey,
			Digest:      derivation.LayerDigest,
			SizeBytes:   derivation.SizeBytes,
			BuildCostMS: step.BuildCost().Milliseconds(),
			MemoryBytes: step.MinimumMemoryBytes(),
			Source:      "cleanup",
		}
		if e.cacheHit(snapshot, derivation) {
			decision.Hit = true
			decision.Source = "cache"
		}
		decisions[number] = decision
	}
	return orderedDecisions(decisions)
}

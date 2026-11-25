package engine

import (
	"context"
	"errors"

	"wardrobe/domain"
)

type RecoveryReport struct {
	Result      domain.CaseResult `json:"result"`
	Found       bool              `json:"found"`
	Recovered   bool              `json:"recovered"`
	Invalidated bool              `json:"invalidated"`
	Reason      string            `json:"reason,omitempty"`
}

// RecoverCase ignores the in-memory cache and attempts SQLite recovery. A
// partially written, stale, or version-mismatched snapshot is invalidated as a
// unit and rebuilt from an empty cache.
func (e *Engine) RecoverCase(projectID, caseID string) (RecoveryReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	project, expected, err := e.lookupLocked(projectID, caseID)
	if err != nil {
		return RecoveryReport{}, err
	}
	if e.store == nil {
		return RecoveryReport{}, errors.New("recovery requires a SQLite store")
	}
	inputHash := domain.CaseInputHash(project, expected.SelectedPartIDs)
	loaded, err := e.store.LoadCase(context.Background(), projectID, caseID, inputHash, project.Constraints.Version, domain.AlgorithmVersion)
	if err != nil {
		return RecoveryReport{}, err
	}
	if loaded.Recovered && loaded.Result != nil {
		result := cloneCaseResult(*loaded.Result)
		for index := range result.Batches {
			result.Batches[index].Source = "sqlite_recovery"
		}
		e.cache[caseKey(projectID, caseID)] = cachedCase{inputHash: result.InputHash, result: result}
		return RecoveryReport{
			Result:    result,
			Found:     loaded.Found,
			Recovered: true,
		}, nil
	}
	result, err := e.computeCase(project, expected, "full_after_recovery", true)
	if err != nil {
		return RecoveryReport{}, err
	}
	e.cache[caseKey(projectID, caseID)] = cachedCase{inputHash: result.InputHash, result: result}
	return RecoveryReport{
		Result:      result,
		Found:       loaded.Found,
		Invalidated: loaded.Invalidated,
		Reason:      loaded.Reason,
	}, nil
}

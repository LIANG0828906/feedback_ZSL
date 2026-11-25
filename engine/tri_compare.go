package engine

import (
	"context"
	"fmt"
	"os"
	"sort"

	"wardrobe/diagnostics"
	"wardrobe/domain"
	"wardrobe/scheduler"
	"wardrobe/store"
)

type PathSummary struct {
	Name           string   `json:"name"`
	Source         string   `json:"source"`
	BatchCount     int      `json:"batchCount"`
	CommitSequence []string `json:"commitSequence"`
	ResultHash     string   `json:"resultHash"`
}

type TriCompareReport struct {
	ProjectID              string                `json:"projectId"`
	CaseID                 string                `json:"caseId"`
	PartID                 string                `json:"partId"`
	Applied                []string              `json:"applied"`
	Equal                  bool                  `json:"equal"`
	FullMatchesIncremental bool                  `json:"fullMatchesIncremental"`
	FullMatchesRecovery    bool                  `json:"fullMatchesRecovery"`
	FirstMismatch          *diagnostics.Mismatch `json:"firstMismatch,omitempty"`
	IncrementalMismatch    *diagnostics.Mismatch `json:"incrementalMismatch,omitempty"`
	RecoveryMismatch       *diagnostics.Mismatch `json:"recoveryMismatch,omitempty"`
	Paths                  []PathSummary         `json:"paths"`
}

// TriCompare executes full-from-empty, incremental-prefix, and SQLite-restart
// paths for the same edit and reports the first divergent batch/field.
func (e *Engine) TriCompare(projectID, caseID string, request EditRequest) (TriCompareReport, error) {
	baseProject, ok := e.Project(projectID)
	if !ok {
		return TriCompareReport{}, ErrProjectNotFound
	}
	expected, ok := e.TruthCase(projectID, caseID)
	if !ok {
		return TriCompareReport{}, ErrCaseNotFound
	}
	request.ProjectID = projectID
	request.CaseID = caseID
	if request.PartID == "" {
		request.PartID = defaultEditPart(baseProject, expected)
	}
	if request.MaterialSlot == "" && request.ScalePercent == 0 && request.Skinned == nil && request.MountID == "" {
		request.ScalePercent = 90
	}
	fullProject, _, _, applied, err := applyPartPatch(baseProject, request)
	if err != nil {
		return TriCompareReport{}, err
	}

	fullEngine, err := newEngine([]domain.Project{fullProject}, Config{Scheduler: e.schedulerConfig})
	if err != nil {
		return TriCompareReport{}, err
	}
	full, err := fullEngine.computeCase(fullProject, expected, "full", false)
	if err != nil {
		return TriCompareReport{}, fmt.Errorf("full path: %w", err)
	}

	incrementalEngine, err := newEngine([]domain.Project{baseProject}, Config{Scheduler: e.schedulerConfig})
	if err != nil {
		return TriCompareReport{}, err
	}
	if _, err := incrementalEngine.BuildCase(projectID, caseID); err != nil {
		return TriCompareReport{}, fmt.Errorf("incremental seed: %w", err)
	}
	editReport, err := incrementalEngine.EditPart(request)
	if err != nil {
		return TriCompareReport{}, fmt.Errorf("incremental path: %w", err)
	}
	incremental := editReport.Result

	recovered, err := compareViaRecovery(fullProject, expected, full, e.schedulerConfig)
	if err != nil {
		return TriCompareReport{}, fmt.Errorf("recovery path: %w", err)
	}

	incrementalMismatch := diagnostics.CompareResults(full, incremental)
	recoveryMismatch := diagnostics.CompareResults(full, recovered)
	first := incrementalMismatch
	if first == nil {
		first = recoveryMismatch
	}
	report := TriCompareReport{
		ProjectID:              projectID,
		CaseID:                 caseID,
		PartID:                 request.PartID,
		Applied:                applied,
		Equal:                  incrementalMismatch == nil && recoveryMismatch == nil,
		FullMatchesIncremental: incrementalMismatch == nil,
		FullMatchesRecovery:    recoveryMismatch == nil,
		FirstMismatch:          first,
		IncrementalMismatch:    incrementalMismatch,
		RecoveryMismatch:       recoveryMismatch,
	}
	for _, path := range []struct {
		name   string
		result domain.CaseResult
	}{
		{name: "full", result: full},
		{name: "incremental", result: incremental},
		{name: "recovery", result: recovered},
	} {
		hash := ResultHash(path.result)
		report.Paths = append(report.Paths, PathSummary{
			Name:           path.name,
			Source:         path.result.Source,
			BatchCount:     len(path.result.Batches),
			CommitSequence: append([]string(nil), path.result.CommitSequence...),
			ResultHash:     hash,
		})
	}
	return report, nil
}

func compareViaRecovery(project domain.Project, expected domain.ExpectedCase, full domain.CaseResult, schedulerConfig scheduler.Config) (domain.CaseResult, error) {
	file, err := os.CreateTemp("", "wardrobe-tricheck-*.db")
	if err != nil {
		return domain.CaseResult{}, err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return domain.CaseResult{}, err
	}
	_ = os.Remove(path)
	defer func() {
		_ = os.Remove(path)
		_ = os.Remove(path + "-wal")
		_ = os.Remove(path + "-shm")
	}()

	firstStore, err := store.Open(path)
	if err != nil {
		return domain.CaseResult{}, err
	}
	if err := firstStore.SaveCase(context.Background(), project, full); err != nil {
		_ = firstStore.Close()
		return domain.CaseResult{}, err
	}
	if err := firstStore.Close(); err != nil {
		return domain.CaseResult{}, err
	}

	recoveryStore, err := store.Open(path)
	if err != nil {
		return domain.CaseResult{}, err
	}
	defer recoveryStore.Close()
	recoveryEngine, err := newEngine([]domain.Project{project}, Config{Scheduler: schedulerConfig, Store: recoveryStore})
	if err != nil {
		return domain.CaseResult{}, err
	}
	recovery, err := recoveryEngine.RecoverCase(project.ProjectID, expected.CaseID)
	if err != nil {
		return domain.CaseResult{}, err
	}
	if !recovery.Recovered {
		return domain.CaseResult{}, fmt.Errorf("checkpoint was not recoverable: found=%t invalidated=%t reason=%s", recovery.Found, recovery.Invalidated, recovery.Reason)
	}
	return recovery.Result, nil
}

func defaultEditPart(project domain.Project, expected domain.ExpectedCase) string {
	partMap := project.PartMap()
	accepted := append([]string(nil), expected.AcceptedPartIDs...)
	sort.Slice(accepted, func(i, j int) bool {
		return domain.PartOrderLess(partMap[accepted[i]], partMap[accepted[j]])
	})
	if len(accepted) > 0 {
		return accepted[0]
	}
	if len(expected.SelectedPartIDs) > 0 {
		return expected.SelectedPartIDs[0]
	}
	return ""
}

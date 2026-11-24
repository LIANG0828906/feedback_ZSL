package workbench

import (
	"errors"
	"path/filepath"
	"strconv"

	"storyboardstudio/internal/checkpoint"
	"storyboardstudio/internal/dependency"
	"storyboardstudio/internal/engine"
	"storyboardstudio/internal/model"
)

type Verification struct {
	ProjectID       string             `json:"project_id"`
	Seed            int64              `json:"seed"`
	ShotCount       int                `json:"shot_count"`
	InterruptAt     int                `json:"interrupt_at"`
	Truth           *engine.Mismatch   `json:"truth"`
	Determinism     *engine.Mismatch   `json:"determinism"`
	ResumeSuffix    *engine.Mismatch   `json:"resume_suffix"`
	ThreePaths      *engine.Mismatch   `json:"three_paths"`
	CheckpointPath  *engine.Mismatch   `json:"checkpoint_path"`
	EditComparisons []*engine.Mismatch `json:"edit_comparisons"`
	Edits           []EditSpec         `json:"edits"`
	EditReports     []*ReplayReport    `json:"-"`
	OK              bool               `json:"ok"`
}

func VerifyProject(project *model.Project, dbDir string) (*Verification, error) {
	originalRun, err := engine.Run(project)
	if err != nil {
		return nil, err
	}
	secondRun, err := engine.Run(project)
	if err != nil {
		return nil, err
	}
	report := &Verification{
		ProjectID:   project.ID,
		Seed:        project.Seed,
		ShotCount:   len(originalRun.Shots),
		Truth:       engine.CompareTruth(project, originalRun),
		Determinism: engine.CompareResults(originalRun, secondRun),
	}

	edits := safeEditSequence(project)
	report.Edits = edits

	continuousProject, err := CloneProject(project)
	if err != nil {
		return nil, err
	}
	for _, edit := range edits {
		if err := ApplyEdit(continuousProject, edit); err != nil {
			return nil, err
		}
	}
	continuous, err := engine.Run(continuousProject)
	if err != nil {
		return nil, err
	}

	interruptAt := len(continuous.Shots)/2 - 1
	if interruptAt < 0 {
		interruptAt = 0
	}
	if interruptAt >= len(continuous.Shots) {
		interruptAt = len(continuous.Shots) - 1
	}
	report.InterruptAt = interruptAt

	checkpointFull, suffixMismatch, err := verifyResume(project, edits, dbDir, interruptAt, continuous)
	if err != nil {
		return report, err
	}
	report.ResumeSuffix = suffixMismatch
	report.CheckpointPath = engine.CompareResults(checkpointFull, continuous)

	incrementalFinal, editReports, err := verifyIncremental(project, edits, dbDir)
	if err != nil {
		return report, err
	}
	report.EditReports = editReports
	for _, editReport := range editReports {
		report.EditComparisons = append(report.EditComparisons, editReport.Comparison)
	}
	report.ThreePaths = engine.CompareResults(incrementalFinal, continuous)

	report.OK = mismatchOK(report.Truth) && mismatchOK(report.Determinism) &&
		mismatchOK(report.ResumeSuffix) && mismatchOK(report.ThreePaths) &&
		mismatchOK(report.CheckpointPath)
	for _, comparison := range report.EditComparisons {
		if !mismatchOK(comparison) {
			report.OK = false
		}
	}
	return report, nil
}

func safeEditSequence(project *model.Project) []EditSpec {
	byID := engine.NodeMap(project.Nodes)
	lastNodeID := project.Truth.EventOrder[len(project.Truth.EventOrder)-1]
	lastNode := byID[lastNodeID]
	duration := lastNode.Shots[len(lastNode.Shots)-1].DurationMS + 250
	lastLocal := len(lastNode.Shots) - 1

	branchNodeID := project.Truth.EventOrder[2]
	branchNode := byID[branchNodeID]
	condition := branchNode.Preconditions
	condition.All = append(append([]string{}, condition.All...), "world_established")

	effects := branchNode.OnArrive
	effects.Characters = append(append([]model.CharacterChange{}, effects.Characters...), model.CharacterChange{
		CharacterID: "editor", Key: "verification_pass", Value: "deterministic",
	})

	return []EditSpec{
		{NodeID: lastNodeID, Kind: dependency.EditDuration, LocalIndex: lastLocal, DurationMS: &duration},
		{NodeID: branchNodeID, Kind: dependency.EditPreconditions, Condition: &condition},
		{NodeID: branchNodeID, Kind: dependency.EditEffects, Effects: &effects},
	}
}

func verifyResume(project *model.Project, edits []EditSpec, dbDir string, interruptAt int, continuous *model.RunResult) (*model.RunResult, *engine.Mismatch, error) {
	edited, err := CloneProject(project)
	if err != nil {
		return nil, nil, err
	}
	for _, edit := range edits {
		if err := ApplyEdit(edited, edit); err != nil {
			return nil, nil, err
		}
	}
	dbPath := filepath.Join(dbDir, "resume.db")
	wb, err := New(dbPath)
	if err != nil {
		return nil, nil, err
	}
	if err := wb.AddProject(edited); err != nil {
		_ = wb.Close()
		return nil, nil, err
	}
	sessionID := "resume-verification"
	partial, err := wb.RunCheckpointed(edited.ID, sessionID, interruptAt)
	if !errors.Is(err, ErrInterrupted) {
		_ = wb.Close()
		if err == nil {
			err = &model.Error{Code: model.CodeState, Message: "interruption observer did not stop run"}
		}
		return nil, nil, err
	}
	if partial == nil || len(partial.Shots) != interruptAt+1 {
		_ = wb.Close()
		return nil, nil, &model.Error{Code: model.CodeState, Message: "interrupted run did not return committed prefix"}
	}
	if err := wb.Close(); err != nil {
		return nil, nil, err
	}

	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer store.Close()
	expectedHash := continuous.ContentHash
	cp, err := store.LoadValidCheckpoint(sessionID, expectedHash, edited.Schema, model.AlgorithmVersion)
	if err != nil {
		return nil, nil, err
	}
	if cp == nil || cp.ShotGlobalIndex != interruptAt {
		actual := "nil"
		if cp != nil {
			actual = "shot_index=" + strconv.Itoa(cp.ShotGlobalIndex)
		}
		return nil, nil, &model.Error{Code: model.CodeState, Expected: "shot_index=" + strconv.Itoa(interruptAt), Actual: actual, Message: "checkpoint boundary mismatch"}
	}
	rows, err := store.ListCheckpoints(sessionID)
	if err != nil {
		return nil, nil, err
	}
	prefix := make([]model.PresentedShot, 0, len(rows))
	for _, row := range rows {
		shot, found, err := store.GetShot(row.ShotHash)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			return nil, nil, &model.Error{Code: model.CodeHash, ConflictShot: row.ShotHash, Message: "prefix shot missing after restart"}
		}
		prefix = append(prefix, *shot)
	}

	state := StateFromCheckpoint(cp)
	remaining, err := engine.ResumeWithObserver(edited, state, nil)
	if err != nil {
		return nil, nil, err
	}
	expectedSuffix := &model.RunResult{Shots: append([]model.PresentedShot{}, continuous.Shots[interruptAt+1:]...)}
	actualSuffix := &model.RunResult{Shots: remaining.Shots}
	suffixMismatch := engine.CompareResults(actualSuffix, expectedSuffix)

	full := &model.RunResult{
		ProjectID: continuous.ProjectID, ContentHash: continuous.ContentHash,
		AlgorithmVersion: continuous.AlgorithmVersion, StructureVersion: continuous.StructureVersion,
		EventOrder: remaining.EventOrder, ReachableNodes: remaining.ReachableNodes,
		TerminalFlags: remaining.TerminalFlags, Characters: remaining.Characters,
		ConsumedChoices: remaining.ConsumedChoices,
		Shots:           append(append([]model.PresentedShot{}, prefix...), remaining.Shots...),
	}
	return full, suffixMismatch, nil
}

func verifyIncremental(project *model.Project, edits []EditSpec, dbDir string) (*model.RunResult, []*ReplayReport, error) {
	original, err := CloneProject(project)
	if err != nil {
		return nil, nil, err
	}
	wb, err := New(filepath.Join(dbDir, "incremental.db"))
	if err != nil {
		return nil, nil, err
	}
	defer wb.Close()
	if err := wb.AddProject(original); err != nil {
		return nil, nil, err
	}
	if _, err := wb.RunCheckpointed(original.ID, "baseline", -1); err != nil {
		return nil, nil, err
	}
	reports := []*ReplayReport{}
	var final *model.RunResult
	for _, edit := range edits {
		report, err := wb.EditAndReplay(original.ID, "incremental-final", edit)
		if err != nil {
			return nil, reports, err
		}
		reports = append(reports, report)
		final = report.Result
	}
	return final, reports, nil
}

func mismatchOK(mismatch *engine.Mismatch) bool {
	return mismatch != nil && mismatch.OK
}

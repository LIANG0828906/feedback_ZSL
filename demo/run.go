package demo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"dialoguefit/checkpoint"
	"dialoguefit/compare"
	"dialoguefit/engine"
	"dialoguefit/generate"
	"dialoguefit/model"
	"dialoguefit/recheck"
	"dialoguefit/sched"
)

type EditStep struct {
	Name   string                    `json:"name"`
	Edits  []model.Edit              `json:"edits"`
	Report *model.InvalidationReport `json:"report,omitempty"`
}

type PathSummary struct {
	Name           string `json:"name"`
	Dialogues      int    `json:"dialogues"`
	Frames         int    `json:"frames"`
	StressMarks    int    `json:"stress_marks"`
	Segments       int    `json:"segments"`
	CommitSequence int    `json:"commit_sequence"`
}

type ConsistencyReport struct {
	OK                  bool             `json:"ok"`
	Paths               []PathSummary    `json:"paths"`
	ComparedFrames      int              `json:"compared_frames"`
	ComparedStressMarks int              `json:"compared_stress_marks"`
	ComparedSegments    int              `json:"compared_segments"`
	IncrementalMismatch compare.Mismatch `json:"incremental_mismatch,omitempty"`
	ResumeMismatch      compare.Mismatch `json:"resume_mismatch,omitempty"`
}

type ResumeScenario struct {
	CrashAfterSegment int                       `json:"crash_after_segment"`
	Restore           *engine.RestoreReport     `json:"restore"`
	ResumeProcess     *model.InvalidationReport `json:"resume_process_report"`
}

type ScenarioReport struct {
	Seed             int64              `json:"seed"`
	AlgorithmVersion string             `json:"algorithm_version"`
	EditSteps        []EditStep         `json:"edit_steps"`
	Resume           ResumeScenario     `json:"resume"`
	Consistency      ConsistencyReport  `json:"consistency"`
	QueueEvents      []model.QueueEvent `json:"queue_events"`
	FinalCommitOrder []string           `json:"final_commit_order"`
}

type Report struct {
	Seed               int64          `json:"seed"`
	Database           string         `json:"database"`
	ProjectPath        string         `json:"project"`
	TruthPath          string         `json:"truth"`
	FinalTruthPath     string         `json:"final_truth"`
	Scenario           ScenarioReport `json:"scenario"`
	IndependentRecheck recheck.Report `json:"independent_recheck"`
}

type Bundle struct {
	Report       *Report
	LiveEngine   *engine.Engine
	LiveStore    *checkpoint.Store
	FinalProject *model.Project
	FinalTruth   *model.TruthFile
}

func SchedulerConfig() sched.Config {
	return sched.Config{MaxParallel: 2, SampleBufferCap: 16384, TotalBufferSamples: 24000, QueueBudget: 2}
}

func ScenarioEdits() []struct {
	Name  string
	Edits []model.Edit
} {
	target14507 := 14507
	cap13 := 1.3
	tol8 := 8
	target14456 := 14456
	min1120 := 1120
	return []struct {
		Name  string
		Edits []model.Edit
	}{
		{Name: "target-duration", Edits: []model.Edit{{DialogueID: "dlg-01", TargetSamples: &target14507}}},
		{Name: "stretch-cap", Edits: []model.Edit{{DialogueID: "dlg-02", MaxStretch: &cap13}}},
		{Name: "stress-tolerance", Edits: []model.Edit{{DialogueID: "dlg-03", StressToleranceSamples: &tol8}}},
		{Name: "silence-minimum", Edits: []model.Edit{{DialogueID: "dlg-04", TargetSamples: &target14456, MinSilenceSamples: &min1120}}},
	}
}

func Run(outDir string) (*Bundle, error) {
	if err := os.MkdirAll(filepath.Join(outDir, "final"), 0o755); err != nil {
		return nil, err
	}
	ctx := context.Background()
	seed := generate.DefaultSeed
	project := generate.GenerateProject(seed)
	truth := generate.GenerateTruth(project)
	projectPath := filepath.Join(outDir, "project.json")
	truthPath := filepath.Join(outDir, "truth.json")
	finalTruthPath := filepath.Join(outDir, "final", "truth.json")
	if err := WriteJSON(projectPath, project); err != nil {
		return nil, err
	}
	if err := WriteJSON(truthPath, truth); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(outDir, "checkpoints.db")
	resetDBFiles(dbPath)
	storeInc, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	engInc := engine.New(project, truth, storeInc, sched.New(SchedulerConfig()))
	if _, err := engInc.RunAll(ctx); err != nil {
		return nil, err
	}
	steps := make([]EditStep, 0, 4)
	for _, scenario := range ScenarioEdits() {
		step := EditStep{Name: scenario.Name, Edits: scenario.Edits}
		for _, edit := range scenario.Edits {
			rep, err := engInc.ApplyEdit(edit)
			if err != nil {
				return nil, err
			}
			step.Report = rep
		}
		steps = append(steps, step)
	}
	finalTruth := generate.GenerateTruth(project)
	if err := WriteJSON(finalTruthPath, finalTruth); err != nil {
		return nil, err
	}
	engInc.SetTruthFile(finalTruth)
	if err := engInc.RecommitAll(ctx); err != nil {
		return nil, err
	}
	incResults := engInc.SnapshotResults()
	independent := recheck.Run(ctx, storeInc, finalTruth)

	fullProject := generate.GenerateProject(seed)
	for _, scenario := range ScenarioEdits() {
		for _, edit := range scenario.Edits {
			ApplyEditToProject(fullProject, edit)
		}
	}
	fullTruth := generate.GenerateTruth(fullProject)
	fullDB := filepath.Join(outDir, "full.db")
	resetDBFiles(fullDB)
	storeFull, err := checkpoint.Open(fullDB)
	if err != nil {
		return nil, err
	}
	engFull := engine.New(fullProject, fullTruth, storeFull, sched.New(SchedulerConfig()))
	if _, err := engFull.RunAll(ctx); err != nil {
		return nil, err
	}
	fullResults := engFull.SnapshotResults()
	_ = storeFull.Close()

	resumeInfo, resumeResults, err := runResumeScenario(ctx, outDir)
	if err != nil {
		return nil, err
	}

	mInc := compare.RunsEqual(fullResults, incResults)
	mResume := compare.RunsEqual(fullResults, resumeResults)
	ok := mInc.OK && mResume.OK && independent.OK
	consistency := ConsistencyReport{
		OK: ok,
		Paths: []PathSummary{
			summary("full", fullResults), summary("incremental", incResults), summary("resume", resumeResults),
		},
		ComparedFrames: mInc.ComparedFrames, ComparedStressMarks: mInc.ComparedStress,
		ComparedSegments:    mInc.ComparedSegments,
		IncrementalMismatch: mInc, ResumeMismatch: mResume,
	}

	report := &Report{
		Seed: seed, Database: dbPath, ProjectPath: projectPath, TruthPath: truthPath, FinalTruthPath: finalTruthPath,
		Scenario: ScenarioReport{
			Seed: seed, AlgorithmVersion: model.AlgorithmVersion, EditSteps: steps,
			Resume:      ResumeScenario{CrashAfterSegment: 3, Restore: resumeInfo.restore, ResumeProcess: resumeInfo.report},
			Consistency: consistency, QueueEvents: engInc.SchedulerEvents(),
			FinalCommitOrder: engInc.CommitSequence(),
		},
		IndependentRecheck: independent,
	}
	reportPath := filepath.Join(outDir, "report.json")
	if err := WriteJSON(reportPath, report); err != nil {
		return nil, err
	}
	if !ok {
		return &Bundle{Report: report, LiveEngine: engInc, LiveStore: storeInc, FinalProject: project, FinalTruth: finalTruth},
			errors.New("three-path or independent recheck consistency failed; see report.json")
	}
	return &Bundle{Report: report, LiveEngine: engInc, LiveStore: storeInc, FinalProject: project, FinalTruth: finalTruth}, nil
}

type resumeOutput struct {
	restore *engine.RestoreReport
	report  *model.InvalidationReport
}

func runResumeScenario(ctx context.Context, outDir string) (resumeOutput, []*model.DialogueResult, error) {
	seed := generate.DefaultSeed
	project := generate.GenerateProject(seed)
	truth := generate.GenerateTruth(project)
	dbPath := filepath.Join(outDir, "resume.db")
	resetDBFiles(dbPath)
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return resumeOutput{}, nil, err
	}
	first := engine.New(project, truth, store, sched.New(SchedulerConfig()))
	if _, err := first.RunAll(ctx); err != nil {
		return resumeOutput{}, nil, err
	}
	scenarios := ScenarioEdits()
	for _, edit := range scenarios[0].Edits {
		if _, err := first.ApplyEdit(edit); err != nil {
			return resumeOutput{}, nil, err
		}
	}
	for _, edit := range scenarios[1].Edits {
		if _, err := first.ApplyEdit(edit); err != nil {
			return resumeOutput{}, nil, err
		}
	}
	first.CrashAfterSegment = 3
	_, crashErr := first.ApplyEdit(scenarios[2].Edits[0])
	if !errors.Is(crashErr, engine.ErrSimulatedKill) {
		return resumeOutput{}, nil, errors.New("expected simulated checkpoint kill")
	}
	_ = store.Close()

	reopened, err := checkpoint.Open(dbPath)
	if err != nil {
		return resumeOutput{}, nil, err
	}
	defer reopened.Close()
	second := engine.New(project, nil, reopened, sched.New(SchedulerConfig()))
	restoreReport, err := second.Restore(ctx)
	if err != nil {
		return resumeOutput{}, nil, err
	}
	resumeReport, err := second.RunMissing(ctx)
	if err != nil {
		return resumeOutput{restore: restoreReport}, second.SnapshotResults(), err
	}
	for _, edit := range scenarios[3].Edits {
		if _, err := second.ApplyEdit(edit); err != nil {
			return resumeOutput{restore: restoreReport}, second.SnapshotResults(), err
		}
	}
	finalTruth := generate.GenerateTruth(project)
	second.SetTruthFile(finalTruth)
	if err := second.RecommitAll(ctx); err != nil {
		return resumeOutput{restore: restoreReport}, second.SnapshotResults(), err
	}
	return resumeOutput{restore: restoreReport, report: resumeReport}, second.SnapshotResults(), nil
}

func ApplyEditToProject(p *model.Project, edit model.Edit) {
	for i := range p.Dialogues {
		d := &p.Dialogues[i]
		if d.ID != edit.DialogueID {
			continue
		}
		if edit.TargetSamples != nil {
			d.Params.TargetSamples = *edit.TargetSamples
		}
		if edit.MaxStretch != nil {
			d.Params.MaxStretch = *edit.MaxStretch
		}
		if edit.MinSilenceSamples != nil {
			d.Params.MinSilenceSamples = *edit.MinSilenceSamples
		}
		if edit.StressToleranceSamples != nil {
			d.Params.StressToleranceSamples = *edit.StressToleranceSamples
		}
	}
}

func summary(name string, results []*model.DialogueResult) PathSummary {
	s := PathSummary{Name: name, Dialogues: len(results)}
	for _, r := range results {
		s.Frames += len(r.Frames)
		s.StressMarks += len(r.Stress)
		s.Segments += len(r.Segments)
		s.CommitSequence += len(r.CommitSequence)
	}
	return s
}

func WriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func resetDBFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

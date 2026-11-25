package runtime

import (
	"github.com/google/uuid"

	"presentkernel/internal/domain"
	"presentkernel/internal/engine"
	"presentkernel/internal/scheduler"
)

type StepReport struct {
	Step              int                      `json:"step"`
	Label             string                   `json:"label"`
	Edit              *engine.Edit             `json:"edit,omitempty"`
	Invalidation      *engine.InvalidationPlan `json:"invalidation,omitempty"`
	OK                bool                     `json:"ok"`
	FirstMismatch     *domain.Error            `json:"first_mismatch,omitempty"`
	ResumeInvalidated bool                     `json:"resume_invalidated"`
	Realtime          *Snapshot                `json:"realtime,omitempty"`
	Resume            *Snapshot                `json:"resume,omitempty"`
	Full              *Snapshot                `json:"full,omitempty"`
}

type VerificationReport struct {
	OK            bool          `json:"ok"`
	DBPath        string        `json:"db_path"`
	RunID         string        `json:"run_id"`
	Steps         []StepReport  `json:"steps"`
	FirstMismatch *domain.Error `json:"first_mismatch,omitempty"`
}

func VerifyEditSequence(base *domain.Project, edits []engine.Edit, dbPath string, cfg scheduler.Config) (*VerificationReport, error) {
	rtProject := CloneProject(base)
	resumeProject := CloneProject(base)
	fullProject := CloneProject(base)
	cache := engine.NewMemoryCache()
	runID := "verify-" + uuid.NewString()
	report := &VerificationReport{OK: true, DBPath: dbPath, RunID: runID}

	compareCurrent := func(step int, label string, edit *engine.Edit, plan *engine.InvalidationPlan) error {
		rt, err := RunRealtime(rtProject, cache, cfg)
		if err != nil {
			return err
		}
		first, err := RunResume(resumeProject, dbPath, runID, 2, nil)
		if err != nil {
			return err
		}
		resumeOut, err := RunResume(resumeProject, dbPath, runID, -1, nil)
		if err != nil {
			return err
		}
		full, err := RunFull(fullProject, cfg)
		if err != nil {
			return err
		}
		if runErr := firstSnapshotError(rt, resumeOut.Snapshot, full); runErr != nil {
			return runErr
		}
		mismatch := CompareSnapshots("realtime", rt, "resume", resumeOut.Snapshot)
		if mismatch == nil {
			mismatch = CompareSnapshots("resume", resumeOut.Snapshot, "full", full)
		}
		stepReport := StepReport{
			Step: step, Label: label, Edit: edit, Invalidation: plan, OK: mismatch == nil,
			FirstMismatch: mismatch, ResumeInvalidated: first.Invalidated || resumeOut.Invalidated,
			Realtime: rt, Resume: resumeOut.Snapshot, Full: full,
		}
		report.Steps = append(report.Steps, stepReport)
		if mismatch != nil {
			report.OK = false
			report.FirstMismatch = mismatch
			return mismatch
		}
		return nil
	}

	if err := compareCurrent(0, "initial deterministic sample", nil, nil); err != nil {
		return report, nil
	}
	for i := range edits {
		edit := edits[i]
		graph := engine.BuildGraph(rtProject, rtProject.TargetFPS)
		plan := graph.PlanEdit(rtProject, edit)
		if err := engine.ApplyEdit(rtProject, edit); err != nil {
			return nil, err
		}
		if err := engine.ApplyEdit(resumeProject, edit); err != nil {
			return nil, err
		}
		if err := engine.ApplyEdit(fullProject, edit); err != nil {
			return nil, err
		}
		label := "edit " + edit.Kind
		if err := compareCurrent(i+1, label, &edit, plan); err != nil {
			return report, nil
		}
	}
	return report, nil
}

func firstSnapshotError(snaps ...*Snapshot) error {
	for _, s := range snaps {
		if s == nil {
			continue
		}
		for _, e := range s.Errors {
			return e
		}
	}
	return nil
}

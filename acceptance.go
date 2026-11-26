package lightprobe

import (
	"path/filepath"
)

type AcceptanceReport struct {
	Scene           *Scene          `json:"scene"`
	EditSequence    []string        `json:"edit_sequence"`
	Full            *PipelineResult `json:"full"`
	Realtime        *PipelineResult `json:"realtime"`
	ResumeFirst     *PipelineResult `json:"resume_first,omitempty"`
	Resume          *PipelineResult `json:"resume"`
	FullDiff        *Error          `json:"full_diff,omitempty"`
	ResumeDiff      *Error          `json:"resume_diff,omitempty"`
	BoundMaxActual  float64         `json:"bound_max_actual"`
	BoundMaxUpper   float64         `json:"bound_max_upper"`
	BoundViolations []string        `json:"bound_violations"`
	AllConsistent   bool            `json:"all_consistent"`
	CheckpointDir   string          `json:"checkpoint_dir"`
}

func RunAcceptance(workDir string) (*AcceptanceReport, error) {
	params := DefaultParams()
	scene, err := GenerateScene(DefaultSeed, params)
	if err != nil {
		return nil, err
	}
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}

	report := &AcceptanceReport{Scene: scene, CheckpointDir: workDir}
	probeID := "probe-room-b-020101"
	probe := scene.ProbeByID(probeID)
	if probe == nil {
		return nil, domainError(ESchema, probeID, "id", "acceptance edit probe missing")
	}
	oldRadiance := probe.Radiance[0]
	if _, err := ApplyRadianceEdit(scene, probeID, 0, oldRadiance+0.035); err != nil {
		return nil, err
	}
	report.EditSequence = append(report.EditSequence, "edit "+probeID+" radiance.R +0.035")
	if _, err := ApplyRadianceEdit(scene, probeID, 0, oldRadiance); err != nil {
		return nil, err
	}
	report.EditSequence = append(report.EditSequence, "restore "+probeID+" radiance.R generator value")

	fullPath := filepath.Join(workDir, "full.lpjson")
	realtimePath := filepath.Join(workDir, "realtime.lpjson")
	resumePath := filepath.Join(workDir, "resume.lpjson")

	report.Full, err = RunPipeline(scene, RunOptions{
		Label: "one-shot-full", CheckpointPath: fullPath,
		Pool: PoolConfig{Workers: 8, BufferLimit: 16, QueueBudget: 64},
	})
	if err != nil {
		return nil, err
	}
	if len(report.Full.RoomErrors) > 0 {
		return report, firstRoomError(report.Full.RoomErrors)
	}

	report.Realtime, err = RunPipeline(scene, RunOptions{
		Label: "realtime-budgeted", CheckpointPath: realtimePath,
		Pool: PoolConfig{Workers: 2, BufferLimit: 4, QueueBudget: 3},
	})
	if err != nil {
		return nil, err
	}
	if len(report.Realtime.RoomErrors) > 0 {
		return report, firstRoomError(report.Realtime.RoomErrors)
	}

	report.ResumeFirst, err = RunPipeline(scene, RunOptions{
		Label: "force-killed-after-room-a", CheckpointPath: resumePath,
		StopAfterCommits: 1,
		Pool:             PoolConfig{Workers: 1, BufferLimit: 2, QueueBudget: 1},
	})
	if err != nil {
		return nil, err
	}

	report.Resume, err = RunPipeline(scene, RunOptions{
		Label: "checkpoint-resume", CheckpointPath: resumePath,
		Pool: PoolConfig{Workers: 4, BufferLimit: 8, QueueBudget: 16},
	})
	if err != nil {
		return nil, err
	}
	if len(report.Resume.RoomErrors) > 0 {
		return report, firstRoomError(report.Resume.RoomErrors)
	}

	report.FullDiff = DiffPipelines(report.Full, report.Realtime)
	if report.FullDiff == nil {
		report.ResumeDiff = DiffPipelines(report.Full, report.Resume)
	} else {
		report.ResumeDiff = report.FullDiff
	}
	report.AllConsistent = report.FullDiff == nil && report.ResumeDiff == nil

	var maxActual, maxUpper float64
	var violations []string
	for _, pr := range []*PipelineResult{report.Full, report.Realtime, report.Resume} {
		a, u, v := CheckErrorBounds(pr)
		if a > maxActual {
			maxActual = a
		}
		if u > maxUpper {
			maxUpper = u
		}
		violations = append(violations, v...)
	}
	report.BoundMaxActual = maxActual
	report.BoundMaxUpper = maxUpper
	report.BoundViolations = violations
	return report, nil
}

func firstRoomError(errs map[string]*Error) error {
	for _, de := range errs {
		return de
	}
	return nil
}

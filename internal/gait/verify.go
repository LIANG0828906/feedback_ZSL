package gait

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type VerificationReport struct {
	Equal               bool      `json:"equal"`
	SourceID            string    `json:"source_id"`
	TargetID            string    `json:"target_id"`
	FrameCount          int       `json:"frame_count"`
	CommittedCycles     int       `json:"committed_cycles"`
	RealtimeLoaded      int       `json:"realtime_loaded"`
	ResumeLoaded        int       `json:"resume_loaded"`
	SnapshotInvalidated bool      `json:"snapshot_invalidated"`
	InterruptedAfter    int       `json:"interrupted_after"`
	Mismatch            *Mismatch `json:"mismatch,omitempty"`
}

// VerifyThreePaths executes recomputation, checkpointed realtime processing and
// checkpoint+restart processing against isolated persistence directories.
func VerifyThreePaths(ctx context.Context, source SourceLoop, phases PhaseTrack, truth Truth,
	target TargetSkeleton, params Params, storeRoot string, interruptAfter int) (VerificationReport, error) {
	report := VerificationReport{
		SourceID:         source.ID,
		TargetID:         target.ID,
		InterruptedAfter: interruptAfter,
	}
	full, _, err := FullRetarget(source, phases, truth, target, params)
	if err != nil {
		return report, err
	}
	report.FrameCount = full.FrameCount
	report.CommittedCycles = len(source.Cycles)

	if storeRoot == "" {
		return report, appErr(CodePara, "store root is required for three-path verification")
	}
	realtimeDir := filepath.Join(storeRoot, "realtime")
	resumeDir := filepath.Join(storeRoot, "resume")
	if err := os.RemoveAll(realtimeDir); err != nil {
		return report, err
	}
	if err := os.RemoveAll(resumeDir); err != nil {
		return report, err
	}

	realtimeEngine, err := NewEngine(source, phases, truth, target, params,
		EngineOptions{Store: NewSnapshotStore(realtimeDir)})
	if err != nil {
		return report, err
	}
	report.RealtimeLoaded = len(realtimeEngine.SnapshotReport().LoadedCycles)
	if err := realtimeEngine.Run(ctx); err != nil {
		return report, err
	}
	realtime, err := realtimeEngine.Result()
	if err != nil {
		return report, err
	}
	if err := ValidateEventTimeline(realtime, truth, realtimeEngine.Mapping(), params); err != nil {
		return report, err
	}

	if interruptAfter <= 0 {
		interruptAfter = len(source.Cycles) / 2
		if interruptAfter < 1 {
			interruptAfter = 1
		}
	}
	report.InterruptedAfter = interruptAfter
	resumeStore := NewSnapshotStore(resumeDir)
	firstRun, err := NewEngine(source, phases, truth, target, params, EngineOptions{Store: resumeStore})
	if err != nil {
		return report, err
	}
	if err := firstRun.RunCycles(ctx, interruptAfter); err != nil {
		return report, err
	}
	firstStatus := firstRun.Status()
	if firstStatus.Committed == 0 {
		return report, appErr(CodeState, "resume verification produced no committed checkpoint before interruption")
	}
	secondRun, err := NewEngine(source, phases, truth, target, params, EngineOptions{Store: resumeStore})
	if err != nil {
		return report, err
	}
	snapshotReport := secondRun.SnapshotReport()
	report.ResumeLoaded = len(snapshotReport.LoadedCycles)
	report.SnapshotInvalidated = snapshotReport.Invalidated
	if report.SnapshotInvalidated {
		return report, appErr(CodeState, fmt.Sprintf("resume checkpoint was invalidated: %s", snapshotReport.Reason))
	}
	if report.ResumeLoaded != firstStatus.Committed {
		return report, appErr(CodeState, fmt.Sprintf("resume loaded %d checkpoints, expected %d",
			report.ResumeLoaded, firstStatus.Committed))
	}
	if err := secondRun.Run(ctx); err != nil {
		return report, err
	}
	resumed, err := secondRun.Result()
	if err != nil {
		return report, err
	}
	if err := ValidateEventTimeline(resumed, truth, secondRun.Mapping(), params); err != nil {
		return report, err
	}

	if err := CompareResults(full, realtime); err != nil {
		report.Mismatch = mismatchFromError(err)
		return report, err
	}
	if err := CompareResults(full, resumed); err != nil {
		report.Mismatch = mismatchFromError(err)
		return report, err
	}
	report.Equal = true
	return report, nil
}

func mismatchFromError(err error) *Mismatch {
	var app *AppError
	if !errors.As(err, &app) {
		return nil
	}
	return &Mismatch{
		Frame: app.Frame,
		Cycle: app.Cycle,
		Foot:  app.Foot,
		Field: app.Field,
		Left:  app.Message,
	}
}

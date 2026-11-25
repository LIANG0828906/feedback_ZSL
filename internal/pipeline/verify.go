package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"prosodyalign/internal/checkpoint"
	"prosodyalign/internal/generator"
	"prosodyalign/internal/model"
	"prosodyalign/internal/sched"
)

type TruthComparison struct {
	DialogID           string `json:"dialog_id"`
	EnvelopeExact      bool   `json:"envelope_exact"`
	FirstEnvelopeFrame int    `json:"first_envelope_frame,omitempty"`
	TruthEnvelope      int64  `json:"truth_envelope,omitempty"`
	ActualEnvelope     int64  `json:"actual_envelope,omitempty"`
	AccentTruth        int64  `json:"accent_truth_sample"`
	AccentActual       int64  `json:"accent_actual_sample"`
	AccentErrorSamples int64  `json:"accent_error_samples"`
	AccentWithinTol    bool   `json:"accent_within_tolerance"`
	FitExact           bool   `json:"fit_exact"`
	FirstFitField      string `json:"first_fit_field,omitempty"`
	TruthValue         any    `json:"truth_value,omitempty"`
	ActualValue        any    `json:"actual_value,omitempty"`
}

type VerificationReport struct {
	Seed             int64                        `json:"seed"`
	WorkDir          string                       `json:"work_dir"`
	Edits            []Edit                       `json:"edits"`
	Realtime         *PathRun                     `json:"realtime"`
	Recovery         *PathRun                     `json:"recovery"`
	Full             *PathRun                     `json:"full"`
	Comparison       *ComparisonReport            `json:"comparison"`
	Truth            []TruthComparison            `json:"truth_comparison"`
	RecoveryBoundary *checkpoint.RecoveryBoundary `json:"recovery_boundary,omitempty"`
}

func DefaultEdits(baseTarget int64) []Edit {
	target := baseTarget + 1200
	capValue := int64(2000)
	tol := int64(240)
	weight := int64(3000)
	return []Edit{
		{Description: "initial deterministic project"},
		{Description: "target duration +1200 samples", TargetDurationSamples: &target},
		{Description: "global stretch upper bound -> 2000 samples", DefaultMaxStretch: &capValue},
		{Description: "accent tolerance -> 240 samples", AccentTolerance: &tol},
		{Description: "edit phoneme p5 weight -> 3000/10000", PhonemeWeight: &PhonemeWeightEdit{PhonemeIndex: 5, WeightPer10k: weight}},
	}
}

func RunThreeWay(workDir string, edits []Edit) (*VerificationReport, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	suite, err := generator.GenerateSuite(generator.FixedSeed, 4)
	if err != nil {
		return nil, err
	}
	if edits == nil {
		edits = DefaultEdits(suite.Dialogs[0].Project.DeclaredTargetDuration)
	}

	realtimeProjects, err := CloneProjects(projectsFromSuite(suite))
	if err != nil {
		return nil, err
	}
	recoveryProjects, err := CloneProjects(projectsFromSuite(suite))
	if err != nil {
		return nil, err
	}
	fullProjects, err := CloneProjects(projectsFromSuite(suite))
	if err != nil {
		return nil, err
	}

	rtPath := filepath.Join(workDir, "realtime.db")
	recPath := filepath.Join(workDir, "recovery.db")
	fullPath := filepath.Join(workDir, "full.db")
	for _, path := range []string{rtPath, recPath, fullPath} {
		resetSQLite(path)
	}

	rtStore, err := checkpoint.Open(rtPath)
	if err != nil {
		return nil, err
	}
	rtScheduler := sched.New(4, 2048, 4096)
	rtEngine := NewEngine("realtime_incremental", realtimeProjects, rtStore, rtScheduler, false)
	rtRun, rtErr := rtEngine.RunSequence(edits)
	if rtErr != nil {
		_ = rtStore.Close()
		return nil, rtErr
	}
	if err := rtStore.Close(); err != nil {
		return nil, err
	}

	recStore, err := checkpoint.Open(recPath)
	if err != nil {
		return nil, err
	}
	recScheduler := sched.New(4, 2048, 4096)
	crashEngine := NewEngine("recovery_before_kill", recoveryProjects, recStore, recScheduler, false)
	crashEngine.CrashAfterCommitKey = "1/dialog-a"
	recRun, crashErr := crashEngine.RunSequence(edits)
	if crashErr != nil && crashErr != ErrSimulatedKill {
		_ = recStore.Close()
		return nil, crashErr
	}
	if err := recStore.Close(); err != nil {
		return nil, err
	}

	reopened, err := checkpoint.Open(recPath)
	if err != nil {
		return nil, err
	}
	expectedContent := map[string]string{}
	for _, p := range recoveryProjects {
		expectedContent[p.ID] = p.ContentHash
	}
	boundary, err := reopened.RecoveryBoundary(SnapshotID, expectedContent, AlgorithmVersion)
	if err != nil {
		_ = reopened.Close()
		return nil, err
	}
	if !boundary.SnapshotValid {
		if err := reopened.DeleteFromSequence(SnapshotID, boundary.ResumeLogicalSeq); err != nil {
			_ = reopened.Close()
			return nil, err
		}
	}
	recScheduler2 := sched.New(4, 2048, 4096)
	recEngine := NewEngine("recovery_restart", recoveryProjects, reopened, recScheduler2, false)
	recRun2, err := recEngine.RunSequence(edits)
	if err != nil {
		_ = reopened.Close()
		return nil, err
	}
	if err := reopened.Close(); err != nil {
		return nil, err
	}
	recRun2.Name = "checkpoint_recovery"
	recRun2.DBPath = recPath
	recRun2.RecoveryBoundary = &boundary
	recRun = mergeRecoveryRun(recRun, recRun2)

	fullStore, err := checkpoint.Open(fullPath)
	if err != nil {
		return nil, err
	}
	fullEngine := NewEngine("full_recompute", fullProjects, fullStore, nil, true)
	fullRun, err := fullEngine.RunSequence(edits)
	if err != nil {
		_ = fullStore.Close()
		return nil, err
	}
	if err := fullStore.Close(); err != nil {
		return nil, err
	}

	rtRun.DBPath = rtPath
	fullRun.DBPath = fullPath
	comparison := ComparePaths(rtRun, recRun2, fullRun)
	truthReport := compareSuiteTruth(suite, rtRun)

	return &VerificationReport{
		Seed: generator.FixedSeed, WorkDir: workDir, Edits: edits,
		Realtime: rtRun, Recovery: recRun2, Full: fullRun,
		Comparison: comparison, Truth: truthReport, RecoveryBoundary: &boundary,
	}, nil
}

func projectsFromSuite(s *generator.Suite) []*model.Project {
	out := make([]*model.Project, 0, len(s.Dialogs))
	for _, d := range s.Dialogs {
		out = append(out, d.Project)
	}
	return out
}

func resetSQLite(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

func mergeRecoveryRun(crashed, restarted *PathRun) *PathRun {
	restarted.Terminated = true
	if crashed != nil && len(crashed.Epochs) > 0 && len(restarted.Epochs) > 0 {
		restarted.Epochs[0].Manifest = crashed.Epochs[0].Manifest
	}
	return restarted
}

func compareSuiteTruth(suite *generator.Suite, run *PathRun) []TruthComparison {
	out := make([]TruthComparison, 0, len(suite.Dialogs))
	if run == nil || len(run.Epochs) == 0 {
		return out
	}
	epoch0 := run.Epochs[0]
	for _, d := range suite.Dialogs {
		item := TruthComparison{DialogID: d.Project.ID, EnvelopeExact: true, FitExact: true, FirstEnvelopeFrame: -1}
		outcome := findOutcome(epoch0, d.Project.ID)
		if outcome == nil || !outcome.OK || outcome.Result == nil {
			item.EnvelopeExact = false
			item.FitExact = false
			out = append(out, item)
			continue
		}
		res := outcome.Result
		for i, want := range d.Truth.EnvelopeFixed {
			if i >= len(res.Frames) || res.Frames[i].EnvelopeFixed != want {
				item.EnvelopeExact = false
				item.FirstEnvelopeFrame = i
				item.TruthEnvelope = want
				if i < len(res.Frames) {
					item.ActualEnvelope = res.Frames[i].EnvelopeFixed
				}
				break
			}
		}
		item.AccentTruth = d.Truth.AccentSample
		item.AccentActual = res.Accent.Sample
		item.AccentErrorSamples = abs64(res.Accent.Sample - d.Truth.AccentSample)
		item.AccentWithinTol = item.AccentErrorSamples <= d.Project.AccentToleranceSamples
		for i, wantRange := range d.Truth.StretchedRanges {
			seg := res.Fit.Segments[i]
			if seg.NewStart != wantRange.StartSample || seg.NewEnd != wantRange.EndSample || seg.AddedSamples != d.Truth.AddedSamples[i] || seg.CapTriggered != d.Truth.CapTriggered[i] {
				item.FitExact = false
				item.FirstFitField = fmt.Sprintf("segments[%d]", i)
				item.TruthValue = map[string]any{"range": wantRange, "added": d.Truth.AddedSamples[i], "capped": d.Truth.CapTriggered[i]}
				item.ActualValue = map[string]any{"range": modelRange(seg), "added": seg.AddedSamples, "capped": seg.CapTriggered}
				break
			}
		}
		out = append(out, item)
	}
	return out
}

func modelRange(seg model.SegmentFit) map[string]any {
	return map[string]any{"start": seg.NewStart, "end": seg.NewEnd}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

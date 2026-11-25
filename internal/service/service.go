package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mocapbench/internal/core"
	"mocapbench/internal/store"
)

type Service struct {
	Samples       *core.SampleSet
	CheckpointDir string
}

func New(sampleDir, checkpointDir string) (*Service, *core.Error) {
	set, err := core.LoadSampleSet(sampleDir)
	if err != nil {
		return nil, err
	}
	if checkpointDir == "" {
		checkpointDir = "checkpoints"
	}
	if err := os.MkdirAll(checkpointDir, 0o755); err != nil {
		return nil, &core.Error{Code: core.ECodeSchema, Message: "cannot create checkpoint directory: " + err.Error()}
	}
	return &Service{Samples: set, CheckpointDir: checkpointDir}, nil
}

func (s *Service) Verify(clipID, targetID string, params core.Parameters, resources core.ResourceConfig, crashAfter int) (*core.RunReport, *core.Error) {
	if clipID == "" {
		clipID = firstClipID(s.Samples)
	}
	if targetID == "" {
		targetID = firstTargetID(s.Samples)
	}
	clip, ok := s.Samples.Clips[clipID]
	if !ok {
		return nil, &core.Error{Code: core.ECodeSchema, Joint: clipID, Message: "unknown clip id"}
	}
	target, ok := s.Samples.Targets[targetID]
	if !ok {
		return nil, &core.Error{Code: core.ECodeSchema, Joint: targetID, Message: "unknown target id"}
	}
	params = core.NormalizeParameters(params)
	engine, eerr := core.NewEngine(clip, target, params)
	if eerr != nil {
		return nil, eerr
	}

	truth := s.Samples.Truth[clipID]
	truthReport := core.VerifyTruth(clip, truth)
	report := &core.RunReport{
		OK: false, ClipID: clipID, TargetID: targetID, Parameters: params,
		ParamsVersion: engine.ParamsVersion, AlgoVersion: core.AlgorithmVersion,
		Mapping: engine.Mapping, Truth: truthReport, VerificationLog: []string{},
	}

	fullSM := core.NewStateMachine(len(clip.Frames))
	fullFrames, err := engine.Run(core.RunOptions{Path: "full", Parallel: false, StateMachine: fullSM})
	if err != nil {
		report.Frames = fullFrames
		return report, err
	}
	report.Frames = fullFrames
	contactCheck := engine.FinalizeContactCheck(fullFrames, &truth)
	report.ContactCheck = contactCheck

	rtPath := filepath.Join(s.CheckpointDir, safeName("realtime_"+clipID+"_"+targetID)+".db")
	rtStore, oerr := store.Open(rtPath)
	if oerr != nil {
		return report, oerr
	}
	if _, ierr := rtStore.Initialize(clipID, targetID, engine.ParamsVersion, core.AlgorithmVersion); ierr != nil {
		rtStore.Close()
		return report, ierr
	}
	rtFrames, rerr := engine.Run(core.RunOptions{
		Path: "realtime", Parallel: true, Resources: resources,
		Commit:       rtStore.SaveCommittedFrame,
		StateMachine: core.NewStateMachine(len(clip.Frames)),
	})
	rtCount, _ := rtStore.Count()
	rtStore.Close()
	if rerr != nil {
		return report, rerr
	}
	rtDiff := core.CompareResults("realtime", "full", &target, rtFrames, fullFrames)

	resumePath := filepath.Join(s.CheckpointDir, safeName("resume_"+clipID+"_"+targetID)+".db")
	_ = os.Remove(resumePath)
	_ = os.Remove(resumePath + "-wal")
	_ = os.Remove(resumePath + "-shm")
	resumeStore, oerr := store.Open(resumePath)
	if oerr != nil {
		return report, oerr
	}
	if _, ierr := resumeStore.Initialize(clipID, targetID, engine.ParamsVersion, core.AlgorithmVersion); ierr != nil {
		resumeStore.Close()
		return report, ierr
	}
	if crashAfter <= 0 || crashAfter >= len(clip.Frames) {
		crashAfter = len(clip.Frames) / 2
	}
	crashFrame := crashAfter - 1
	simulatedCrash := core.NewError("E_CRASH", "simulated forced termination after durable checkpoint")
	_, crashErr := engine.Run(core.RunOptions{
		Path: "resume-before-crash", Parallel: false,
		StateMachine: core.NewStateMachine(len(clip.Frames)),
		Commit: func(result core.FrameResult) *core.Error {
			if serr := resumeStore.SaveCommittedFrame(result); serr != nil {
				return serr
			}
			if result.Frame == crashFrame {
				return simulatedCrash
			}
			return nil
		},
	})
	if crashErr == nil || crashErr.Code != "E_CRASH" {
		resumeStore.Close()
		return report, &core.Error{Code: core.ECodeState, Message: "crash simulation did not stop at requested frame", Details: crashErr}
	}
	resumeStore.Close()

	reopened, oerr := store.Open(resumePath)
	if oerr != nil {
		return report, oerr
	}
	initInfo, ierr := reopened.Initialize(clipID, targetID, engine.ParamsVersion, core.AlgorithmVersion)
	if ierr != nil {
		reopened.Close()
		return report, ierr
	}
	prefix, lerr := reopened.LoadCommittedFrames(clipID, targetID, engine.ParamsVersion, core.AlgorithmVersion)
	if lerr != nil {
		reopened.Close()
		return report, lerr
	}
	recoveryOK := len(prefix) == crashAfter && !initInfo.Reset
	resumeFrames, rerr := engine.Run(core.RunOptions{
		Path: "resume", Parallel: true, Resources: resources, Prefix: prefix,
		Commit: reopened.SaveCommittedFrame, StateMachine: core.NewStateMachine(len(clip.Frames)),
	})
	reopened.Close()
	if rerr != nil {
		return report, rerr
	}
	resumeDiff := core.CompareResults("resume", "full", &target, resumeFrames, fullFrames)

	report.Paths = []core.PathSummary{
		summarizePath("full", "", fullFrames),
		summarizePath("realtime", rtPath, rtFrames),
		summarizePath("resume", resumePath, resumeFrames),
	}
	report.Paths[1].Comparison = rtDiff
	report.Paths[2].Comparison = resumeDiff
	if !rtDiff.OK {
		report.ThreeWay = rtDiff
	} else if !resumeDiff.OK {
		report.ThreeWay = resumeDiff
	} else {
		report.ThreeWay = &core.DiffReport{OK: true, PathA: "realtime,resume", PathB: "full", ComparedFrames: len(fullFrames)}
	}
	report.Summary = buildSummary(engine, fullFrames, engine.Mapping, rtCount, recoveryOK, report.ThreeWay.OK)
	report.DBPath = rtPath
	report.OK = truthReport.OK && contactCheck.OK && report.ThreeWay.OK && recoveryOK
	report.VerificationLog = append(report.VerificationLog,
		fmt.Sprintf("truth: world_max=%.3g bone_max=%.3g root_max=%.3g mismatches=%d", truthReport.MaxWorldError, truthReport.MaxBoneError, truthReport.RootError, len(truthReport.Mismatches)),
		fmt.Sprintf("mapping: %d entries, unmapped_source=%v, degraded=%v, unmapped_target=%v, global_scale=%.6f", len(engine.Mapping.Entries), engine.Mapping.UnmappedSource, engine.Mapping.DegradedSource, engine.Mapping.UnmappedTarget, engine.Mapping.GlobalScale),
		fmt.Sprintf("solver: frames=%d max_iterations=%d max_violation=%.3g", len(fullFrames), report.Summary.MaxIterations, report.Summary.MaxViolation),
		fmt.Sprintf("contact: max_drift=%.6f limit=%.6f interval_diffs=%v", contactCheck.MaxDrift, params.ContactDriftLimit, contactCheck.IntervalFrameDiffs),
		fmt.Sprintf("persistence: realtime_rows=%d resume_prefix=%d recovery_ok=%v", rtCount, len(prefix), recoveryOK),
		fmt.Sprintf("compare: realtime_ok=%v resume_ok=%v", rtDiff.OK, resumeDiff.OK),
	)

	if !contactCheck.OK {
		contactErr := &core.Error{Code: core.ECodeContact, Message: "foot contact drift or interval deviation exceeds bound", Details: contactCheck}
		if len(contactCheck.DriftViolations) > 0 {
			contactErr.Frame = contactCheck.DriftViolations[0].Frame
			contactErr.Joint = contactCheck.DriftViolations[0].Foot
		}
		return report, contactErr
	}
	if !report.ThreeWay.OK {
		return report, core.DiffAsError(report.ThreeWay)
	}
	return report, nil
}

func summarizePath(name, dbPath string, frames []core.FrameResult) core.PathSummary {
	s := core.PathSummary{Path: name, DBPath: dbPath, Frames: len(frames), Committed: len(frames)}
	if len(frames) > 0 {
		s.FirstHash = frames[0].ContentHash
		s.LastHash = frames[len(frames)-1].ContentHash
	}
	for _, f := range frames {
		if f.Stats.MaxViolation > s.MaxViolation {
			s.MaxViolation = f.Stats.MaxViolation
		}
		if f.Stats.MaxContactDrift > s.MaxContactDrift {
			s.MaxContactDrift = f.Stats.MaxContactDrift
		}
	}
	return s
}

func buildSummary(engine *core.Engine, frames []core.FrameResult, mapping *core.Mapping, rtRows int, recoveryOK, consistencyOK bool) core.ReportSummary {
	s := core.ReportSummary{Frames: len(frames), Joints: len(engine.Target.Joints), UnmappedSource: len(mapping.UnmappedSource), DegradedSource: len(mapping.DegradedSource), RecoveryOK: recoveryOK, ConsistencyOK: consistencyOK}
	for _, f := range frames {
		if f.Stats.Iterations > s.MaxIterations {
			s.MaxIterations = f.Stats.Iterations
		}
		if f.Stats.MaxViolation > s.MaxViolation {
			s.MaxViolation = f.Stats.MaxViolation
		}
		if f.Stats.MaxContactDrift > s.MaxContactDrift {
			s.MaxContactDrift = f.Stats.MaxContactDrift
		}
		for _, d := range f.Diagnostics {
			if d.TotalBound > s.MaxTotalBound {
				s.MaxTotalBound = d.TotalBound
			}
		}
	}
	return s
}

func firstClipID(set *core.SampleSet) string {
	for _, c := range set.Manifest.Clips {
		return c.ID
	}
	return ""
}

func firstTargetID(set *core.SampleSet) string {
	for _, t := range set.Manifest.Targets {
		return t.ID
	}
	return ""
}

func safeName(s string) string {
	return strings.NewReplacer("/", "_", "\\", "_", "..", "_", " ", "_").Replace(s)
}

type VersionCheckResult struct {
	OK     bool   `json:"ok"`
	Reset  bool   `json:"reset"`
	Reason string `json:"reason"`
}

func CheckVersionInvalidation(path string, clipID, targetID, paramsVersion, algoVersion string) (VersionCheckResult, *core.Error) {
	st, err := store.Open(path)
	if err != nil {
		return VersionCheckResult{}, err
	}
	defer st.Close()
	info, ierr := st.Initialize(clipID, targetID, paramsVersion, algoVersion)
	if ierr != nil {
		return VersionCheckResult{}, ierr
	}
	return VersionCheckResult{OK: true, Reset: info.Reset, Reason: info.Reason}, nil
}

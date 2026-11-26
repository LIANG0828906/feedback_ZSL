package bonerig

import (
	"context"
	"os"
	"path/filepath"
)

type PathVerification struct {
	Name              string           `json:"name"`
	CanonicalSequence []string         `json:"canonical_sequence"`
	WriteSequence     []string         `json:"write_sequence"`
	IntervalRuns      []IntervalRun    `json:"interval_runs"`
	Frames            []FrameResult    `json:"-"`
	Invalidations     [][]Invalidation `json:"invalidations"`
	MaxWorkers        int32            `json:"max_workers"`
	WaitCount         int              `json:"wait_count"`
}

type ThreePathReport struct {
	Paths                        []PathVerification `json:"paths"`
	FrameDiff                    *Error             `json:"frame_diff,omitempty"`
	SequenceDiff                 *Error             `json:"sequence_diff,omitempty"`
	RecoveryWriteSequenceMatches bool               `json:"recovery_write_sequence_matches"`
}

func freshV3Asset(sample *SampleSet, truth TruthManifest) Asset {
	loaded, err := LoadDocument(sample.Docs[V3Schema])
	if err != nil {
		panic(err.Error())
	}
	a := loaded.Asset
	a.Truth = truth
	return a
}

func handConstraintEdit(a *Asset) (old Constraint, next Constraint) {
	idx := a.Skeleton.Index()
	i := idx["hand_l"]
	old = *a.Skeleton.Joints[i].Constraint
	next = old
	next.MaxAngleMicroRad = 2_500_000
	next.DefaultFilled = false
	a.Skeleton.Joints[i].Constraint = &next
	return old, next
}

func handCoefficientEdit(a *Asset) MapItem {
	for i := range a.Mapping.Items {
		if a.Mapping.Items[i].TargetJoint == "HandL" {
			old := a.Mapping.Items[i]
			a.Mapping.Items[i].RotScaleNum = 9
			a.Mapping.Items[i].RotScaleDen = 10
			a.Mapping.Items[i].ScaleDefaultFilled = false
			return old
		}
	}
	panic("HandL mapping missing")
}

func crashError(stage string) *Error {
	e := errorf(CodeState, "simulated forced termination: "+stage)
	e.OpA = stage
	return e
}

// RunThreePaths executes realtime, kill/recovery and one-shot full paths.
func RunThreePaths(workDir string, budget Budget) (*ThreePathReport, *Error) {
	sample, serr := NewDeterministicSample(DefaultSeed)
	if serr != nil {
		return nil, serr
	}
	truthSample, terr := NewDeterministicSample(DefaultSeed)
	if terr != nil {
		return nil, terr
	}
	truthAsset := truthSample.Asset
	_, _ = handConstraintEdit(&truthAsset)
	handCoefficientEdit(&truthAsset)
	finalTruth := BuildReferenceTruth(truthAsset)
	ctx := context.Background()
	out := &ThreePathReport{}
	for _, name := range []string{"realtime", "recovery", "full"} {
		if err := os.RemoveAll(filepath.Join(workDir, name)); err != nil {
			return nil, &Error{Code: CodeSchema, Message: err.Error(), Field: workDir}
		}
	}

	rt, err := runRealtimePath(ctx, sample, finalTruth, workDir, budget)
	if err != nil {
		return nil, err
	}
	rec, err := runRecoveryPath(ctx, sample, finalTruth, workDir, budget)
	if err != nil {
		return nil, err
	}
	full, err := runFullPath(ctx, sample, finalTruth, workDir, budget)
	if err != nil {
		return nil, err
	}
	out.Paths = []PathVerification{rt, rec, full}
	if diff := CompareFrames(rt.Frames, rec.Frames); diff != nil {
		out.FrameDiff = diff
		return out, diff
	}
	if diff := CompareFrames(rt.Frames, full.Frames); diff != nil {
		out.FrameDiff = diff
		return out, diff
	}
	if diff := RequireEqualSequence(rt.CanonicalSequence, rec.CanonicalSequence); diff != nil {
		out.SequenceDiff = diff
		return out, diff
	}
	if diff := RequireEqualSequence(rt.CanonicalSequence, full.CanonicalSequence); diff != nil {
		out.SequenceDiff = diff
		return out, diff
	}
	out.RecoveryWriteSequenceMatches = equalStrings(rt.WriteSequence, rec.WriteSequence)
	if !out.RecoveryWriteSequenceMatches {
		diff := RequireEqualSequence(rt.WriteSequence, rec.WriteSequence)
		out.SequenceDiff = diff
		return out, diff
	}
	return out, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func finishPath(name string, report *RunReport, plans [][]Invalidation) PathVerification {
	p := PathVerification{Name: name, CanonicalSequence: CanonicalSequence(report), WriteSequence: report.StoreSequence, IntervalRuns: report.Intervals, Frames: report.Frames, Invalidations: plans, MaxWorkers: report.Metrics.MaxObservedWorkers, WaitCount: len(report.Metrics.Waits)}
	if report.Metrics.Waits != nil {
		p.WaitCount = len(report.Metrics.Waits)
	}
	return p
}

func runRealtimePath(ctx context.Context, sample *SampleSet, finalTruth TruthManifest, workDir string, budget Budget) (PathVerification, *Error) {
	a := freshV3Asset(sample, sample.Truth)
	store, err := OpenCheckpointStore(filepath.Join(workDir, "realtime", "checkpoint.json"))
	if err != nil {
		return PathVerification{}, err
	}
	engine := &Engine{Asset: a, Budget: budget, Store: store, Recorder: &WaitRecorder{}}
	first, err := engine.Run(ctx)
	if err != nil {
		return PathVerification{}, err
	}
	activity := engine.Activity
	oldC, nextC := handConstraintEdit(&a)
	plan1 := PlanConstraintEdit(a, activity, "hand_l", oldC, nextC)
	engine.Asset = a
	second, err := engine.Run(ctx)
	if err != nil {
		return PathVerification{}, err
	}
	handCoefficientEdit(&a)
	a.Truth = finalTruth
	engine.Asset = a
	plan2 := PlanCoefficientEdit(a, activity, mustMap(a, "HandL"), "retarget_coefficient_edited")
	third, err := engine.Run(ctx)
	if err != nil {
		return PathVerification{}, err
	}
	third.Metrics.Waits = append(first.Metrics.Waits, second.Metrics.Waits...)
	third.Metrics.Waits = append(third.Metrics.Waits, engine.Recorder.Records()...)
	return finishPath("realtime", third, [][]Invalidation{plan1, plan2}), nil
}

func mustMap(a Asset, target string) MapItem {
	for _, m := range a.Mapping.Items {
		if m.TargetJoint == target {
			return m
		}
	}
	panic("missing " + target)
}

func runRecoveryPath(ctx context.Context, sample *SampleSet, finalTruth TruthManifest, workDir string, budget Budget) (PathVerification, *Error) {
	dir := filepath.Join(workDir, "recovery", "checkpoint.json")
	a := freshV3Asset(sample, sample.Truth)
	store, err := OpenCheckpointStore(dir)
	if err != nil {
		return PathVerification{}, err
	}
	engine := &Engine{Asset: a, Budget: budget, Store: store, Recorder: &WaitRecorder{}}
	engine.Hooks.AfterCommit = func(id string) *Error {
		if id == "frames-0000-0004" {
			return crashError("after-first-commit")
		}
		return nil
	}
	if _, rerr := engine.Run(ctx); rerr == nil {
		return PathVerification{}, crashError("expected initial crash")
	}

	store2, oerr := OpenCheckpointStore(dir)
	if oerr != nil {
		return PathVerification{}, oerr
	}
	engine = &Engine{Asset: a, Budget: budget, Store: store2, Recorder: &WaitRecorder{}}
	if _, rerr := engine.Run(ctx); rerr != nil {
		return PathVerification{}, rerr
	}
	activity := engine.Activity
	oldC, nextC := handConstraintEdit(&a)
	plan1 := PlanConstraintEdit(a, activity, "hand_l", oldC, nextC)
	engine.Asset = a
	engine.Hooks.BeforeCommit = func(id string) *Error {
		if id == "frames-0008-0012" {
			return crashError("before-constraint-commit")
		}
		return nil
	}
	if _, rerr := engine.Run(ctx); rerr == nil {
		return PathVerification{}, crashError("expected constraint crash")
	}
	store3, oerr := OpenCheckpointStore(dir)
	if oerr != nil {
		return PathVerification{}, oerr
	}
	engine = &Engine{Asset: a, Budget: budget, Store: store3, Recorder: &WaitRecorder{}}
	constraintReport, rerr := engine.Run(ctx)
	if rerr != nil {
		return PathVerification{}, rerr
	}

	handCoefficientEdit(&a)
	a.Truth = finalTruth
	engine.Asset = a
	plan2 := PlanCoefficientEdit(a, activity, mustMap(a, "HandL"), "retarget_coefficient_edited")
	engine.Hooks.BeforeCommit = func(id string) *Error {
		if id == "frames-0008-0012" {
			return crashError("before-coefficient-commit")
		}
		return nil
	}
	if _, rerr := engine.Run(ctx); rerr == nil {
		return PathVerification{}, crashError("expected coefficient crash")
	}
	store4, oerr := OpenCheckpointStore(dir)
	if oerr != nil {
		return PathVerification{}, oerr
	}
	engine.Store = store4
	engine.Hooks = EngineHooks{}
	final, rerr := engine.Run(ctx)
	if rerr != nil {
		return PathVerification{}, rerr
	}
	final.Metrics.Waits = append(final.Metrics.Waits, constraintReport.Metrics.Waits...)
	return finishPath("recovery", final, [][]Invalidation{plan1, plan2}), nil
}

func runFullPath(ctx context.Context, sample *SampleSet, truth TruthManifest, workDir string, budget Budget) (PathVerification, *Error) {
	a := freshV3Asset(sample, truth)
	_, nextC := handConstraintEdit(&a)
	_ = nextC
	handCoefficientEdit(&a)
	store, err := OpenCheckpointStore(filepath.Join(workDir, "full", "checkpoint.json"))
	if err != nil {
		return PathVerification{}, err
	}
	engine := &Engine{Asset: a, Budget: budget, Store: store, Recorder: &WaitRecorder{}}
	rep, rerr := engine.Run(ctx)
	if rerr != nil {
		return PathVerification{}, rerr
	}
	return finishPath("full", rep, nil), nil
}

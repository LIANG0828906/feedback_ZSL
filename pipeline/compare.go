package pipeline

import (
	"context"

	"iconflow/checkpoint"
	"iconflow/errs"
	"iconflow/model"
)

type ConsistencyReport struct {
	RealtimeBlocks int         `json:"realtime_blocks"`
	ResumeBlocks   int         `json:"resume_blocks"`
	FullBlocks     int         `json:"full_blocks"`
	Realtime       *RunReport  `json:"-"`
	Resume         *RunReport  `json:"-"`
	Full           *RunReport  `json:"-"`
	Diff           *errs.Error `json:"diff,omitempty"`
	Passed         bool        `json:"passed"`
}

func RunConsistency(ctx context.Context, project *model.Project, options Options) (*ConsistencyReport, *errs.Error) {
	realtimeProject, cloneErr := CloneProject(project)
	if cloneErr != nil {
		return nil, cloneErr
	}
	resumeProject, cloneErr := CloneProject(project)
	if cloneErr != nil {
		return nil, cloneErr
	}
	fullProject, cloneErr := CloneProject(project)
	if cloneErr != nil {
		return nil, cloneErr
	}

	realtimeOptions := options
	realtimeOptions.Mode = "realtime"
	realtimeOptions.Store = checkpoint.NewMemoryStore()
	realtimeOptions.StopAfterBlocks = 0
	realtimeRunner, runnerErr := NewRunner(ctx, realtimeProject, realtimeOptions)
	if runnerErr != nil {
		return nil, runnerErr
	}
	realtime, runErr := realtimeRunner.Run(ctx)
	if runErr != nil {
		return nil, runErr
	}

	resumeStore := checkpoint.NewMemoryStore()
	interruptOptions := options
	interruptOptions.Mode = "resume-interrupted"
	interruptOptions.Store = resumeStore
	totalBlocks := len(resumeProject.SortedBlockCoords())
	interruptOptions.StopAfterBlocks = 3
	if totalBlocks < 4 {
		interruptOptions.StopAfterBlocks = 1
	}
	interruptRunner, runnerErr := NewRunner(ctx, resumeProject, interruptOptions)
	if runnerErr != nil {
		return nil, runnerErr
	}
	interrupted, interruptErr := interruptRunner.Run(ctx)
	if interruptErr != nil && interruptErr.Code != errs.EState && interruptErr != ErrInterrupted {
		if domainErr, ok := errs.AsError(interruptErr); !ok || domainErr.Code != "E_INTERRUPTED" {
			return nil, errs.New(errs.EInternal, "$.resume", "interrupted run returned unexpected error: %v", interruptErr)
		}
	}
	_ = interrupted

	resumeOptions := options
	resumeOptions.Mode = "resume"
	resumeOptions.Store = resumeStore
	resumeOptions.StopAfterBlocks = 0
	resumeRunner, runnerErr := NewRunner(ctx, resumeProject, resumeOptions)
	if runnerErr != nil {
		return nil, runnerErr
	}
	resume, runErr := resumeRunner.Run(ctx)
	if runErr != nil {
		return nil, runErr
	}

	fullOptions := options
	fullOptions.Mode = "full_recompute"
	fullOptions.Store = checkpoint.NewMemoryStore()
	fullOptions.StopAfterBlocks = 0
	fullRunner, runnerErr := NewRunner(ctx, fullProject, fullOptions)
	if runnerErr != nil {
		return nil, runnerErr
	}
	full, runErr := fullRunner.Run(ctx)
	if runErr != nil {
		return nil, runErr
	}

	report := &ConsistencyReport{
		RealtimeBlocks: len(realtime.Results),
		ResumeBlocks:   len(resume.Results),
		FullBlocks:     len(full.Results),
		Realtime:       realtime,
		Resume:         resume,
		Full:           full,
	}
	if diff := CompareRuns(realtime, full); diff != nil {
		report.Diff = diff
		return report, diff
	}
	if diff := CompareRuns(realtime, resume); diff != nil {
		report.Diff = diff
		return report, diff
	}
	report.Passed = true
	return report, nil
}

func CompareRuns(expected, actual *RunReport) *errs.Error {
	if expected == nil || actual == nil {
		return errs.Diff("$", "nil run report")
	}
	if len(expected.Results) != len(actual.Results) {
		return errs.Diff("results", "block count %d != %d", len(actual.Results), len(expected.Results))
	}
	for i := range expected.Results {
		want := expected.Results[i]
		got := actual.Results[i]
		if want.Coord.Key() != got.Coord.Key() {
			return errs.Diff("results["+itoa(i)+"].coord", "block order %s != %s", got.Coord.Key(), want.Coord.Key())
		}
		fieldPrefix := "block[" + want.Coord.Key() + "]"
		if (want.Threshold == nil) != (got.Threshold == nil) {
			return errs.Diff(fieldPrefix+".threshold", "threshold presence mismatch")
		}
		if want.Threshold != nil && want.Threshold.Threshold != got.Threshold.Threshold {
			e := errs.Diff(fieldPrefix+".threshold", "threshold %d != %d", got.Threshold.Threshold, want.Threshold.Threshold)
			e.Details = map[string]any{"coord": want.Coord.Key(), "field": "threshold", "actual": got.Threshold.Threshold, "expected": want.Threshold.Threshold}
			return e
		}
		if len(want.Markers) != len(got.Markers) {
			return errs.Diff(fieldPrefix+".markers", "marker length %d != %d", len(got.Markers), len(want.Markers))
		}
		for p := range want.Markers {
			if want.Markers[p] != got.Markers[p] {
				e := errs.Diff(fieldPrefix+".markers["+itoa(p)+"]", "marker pixel %d mismatch", p)
				e.Details = map[string]any{"coord": want.Coord.Key(), "field": "markers", "pixel_index": p}
				return e
			}
		}
		if (want.Stats == nil) != (got.Stats == nil) {
			return errs.Diff(fieldPrefix+".stats", "stats presence mismatch")
		}
		if want.Stats != nil {
			if len(want.Stats.Mask) != len(got.Stats.Mask) {
				return errs.Diff(fieldPrefix+".mask", "mask length mismatch")
			}
			for p := range want.Stats.Mask {
				if want.Stats.Mask[p] != got.Stats.Mask[p] {
					e := errs.Diff(fieldPrefix+".mask["+itoa(p)+"]", "mask pixel %d mismatch", p)
					e.Details = map[string]any{"coord": want.Coord.Key(), "field": "mask", "pixel_index": p}
					return e
				}
			}
			if want.Stats.Foreground != got.Stats.Foreground {
				return diffDetail(want.Coord.Key(), "foreground", got.Stats.Foreground, want.Stats.Foreground)
			}
			if len(want.Stats.Components) != len(got.Stats.Components) {
				return diffDetail(want.Coord.Key(), "component_count", len(got.Stats.Components), len(want.Stats.Components))
			}
			for c := range want.Stats.Components {
				wc := want.Stats.Components[c]
				gc := got.Stats.Components[c]
				if wc.Area != gc.Area {
					return diffDetail(want.Coord.Key(), "component["+itoa(c)+"].area", gc.Area, wc.Area)
				}
				if wc.Centroid != gc.Centroid {
					return diffDetail(want.Coord.Key(), "component["+itoa(c)+"].centroid", gc.Centroid, wc.Centroid)
				}
			}
		}
	}
	if err := compareSequence(expected.CommitSequence, actual.CommitSequence); err != nil {
		return err
	}
	return nil
}

func diffDetail(coord, field string, actual, expected any) *errs.Error {
	e := errs.Diff("block["+coord+"]."+field, "%v != %v", actual, expected)
	e.Details = map[string]any{"coord": coord, "field": field, "actual": actual, "expected": expected}
	return e
}

func compareSequence(want, got []CommitSequenceItem) *errs.Error {
	if len(want) != len(got) {
		return errs.Diff("commit_sequence", "length %d != %d", len(got), len(want))
	}
	for i := range want {
		if want[i] != got[i] {
			return errs.Diff("commit_sequence["+itoa(i)+"]", "got %+v want %+v", got[i], want[i])
		}
	}
	return nil
}

func memoryStore() checkpoint.Store { return checkpoint.NewMemoryStore() }

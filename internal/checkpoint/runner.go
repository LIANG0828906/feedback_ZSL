package checkpoint

import (
	"context"
	"sort"

	"featurekernel/internal/engine"
	"featurekernel/internal/kernel"
)

type Runner struct {
	Store   *Store
	Project *kernel.Project

	Runtimes    []*engine.Runtime
	watermarks  []kernel.Watermark
	startCursor int64
	cursor      int64
	AfterCommit func(seq int64)
}

type RunReport struct {
	StartCursor   int64 `json:"start_cursor"`
	CommittedSeq  int64 `json:"committed_seq"`
	FinalSeq      int64 `json:"final_seq"`
	ProcessedFrom int64 `json:"processed_from"`
	ProcessedTo   int64 `json:"processed_to"`
}

func NewRunner(ctx context.Context, store *Store, project *kernel.Project, forcePath string) (*Runner, error) {
	if _, err := engine.New(project); err != nil {
		return nil, err
	}
	snapshot, err := store.LoadSnapshot(ctx, project.ProjectID)
	if err != nil {
		return nil, err
	}

	runner := &Runner{Store: store, Project: project, startCursor: snapshot.Cursor, cursor: snapshot.Cursor}
	if snapshot.Watermarks != nil {
		runner.watermarks = append(runner.watermarks, snapshot.Watermarks...)
	}

	for _, feature := range project.Features {
		runtime, err := engine.NewRuntime(project, feature, forcePath)
		if err != nil {
			return nil, err
		}
		runtime.AppliedSeq = snapshot.Cursor
		if states := snapshot.FeatureStates[feature.ID]; states != nil {
			for _, state := range states {
				runtime.HydrateState(state)
			}
		}
		featureDiscards := []kernel.DiscardedRecord{}
		featureRecomputations := []kernel.Recomputation{}
		for _, record := range snapshot.Discarded {
			if record.FeatureID == feature.ID {
				featureDiscards = append(featureDiscards, record)
			}
		}
		for _, record := range snapshot.Recomputations {
			if record.FeatureID == feature.ID {
				featureRecomputations = append(featureRecomputations, record)
			}
		}
		runtime.HydrateDiscarded(featureDiscards)
		runtime.HydrateRecomputations(featureRecomputations)
		if len(snapshot.Watermarks) > 0 {
			last := snapshot.Watermarks[len(snapshot.Watermarks)-1].TimeMs
			runtime.SetWatermark(&last)
		}
		runner.Runtimes = append(runner.Runtimes, runtime)
	}
	return runner, nil
}

func ResetMismatchedCheckpoint(ctx context.Context, store *Store, project *kernel.Project) (bool, error) {
	_, err := store.LoadSnapshot(ctx, project.ProjectID)
	if err == nil {
		return false, nil
	}
	if kernel.CodeOf(err) != kernel.CodeCheckpointVersionMismatch {
		return false, err
	}
	if err := store.Reset(ctx, project.ProjectID); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Runner) Run(ctx context.Context, targetSeq int64, batchSize int) (*RunReport, *kernel.ResultSet, error) {
	if batchSize <= 0 {
		batchSize = 1
	}
	lastSeq := int64(0)
	if len(r.Project.Records) > 0 {
		lastSeq = r.Project.Records[len(r.Project.Records)-1].Seq
	}
	if targetSeq <= 0 || targetSeq > lastSeq {
		targetSeq = lastSeq
	}
	if targetSeq < r.cursor {
		return nil, nil, kernel.NewError(kernel.CodeInvalidArgument, "target sequence is before committed cursor", map[string]any{
			"target": targetSeq, "cursor": r.cursor,
		})
	}

	report := &RunReport{StartCursor: r.startCursor, ProcessedFrom: r.cursor + 1, ProcessedTo: targetSeq}
	pending := 0
	for _, record := range r.Project.Records {
		if record.Seq <= r.cursor {
			continue
		}
		if record.Seq > targetSeq {
			break
		}
		select {
		case <-ctx.Done():
			report.CommittedSeq = r.cursor
			report.FinalSeq = targetSeq
			return report, nil, ctx.Err()
		default:
		}

		for _, runtime := range r.Runtimes {
			if err := runtime.ApplyRecord(record); err != nil {
				return report, nil, err
			}
		}
		if record.Kind == kernel.RecordWatermark && record.Watermark != nil {
			r.watermarks = append(r.watermarks, *record.Watermark)
		}
		pending++

		if pending >= batchSize || record.Seq == targetSeq {
			if err := r.Store.Commit(ctx, CommitData{
				ProjectID: r.Project.ProjectID, BatchID: r.Project.BatchID,
				Cursor: record.Seq, Runtimes: r.Runtimes, Watermarks: r.watermarks,
			}); err != nil {
				if ctx.Err() != nil {
					report.CommittedSeq = r.cursor
					report.FinalSeq = targetSeq
					return report, nil, ctx.Err()
				}
				return report, nil, err
			}
			r.cursor = record.Seq
			pending = 0
			if r.AfterCommit != nil {
				r.AfterCommit(record.Seq)
			}
		}
	}

	report.CommittedSeq = r.cursor
	report.FinalSeq = targetSeq
	resultSet, err := r.ResultSet()
	if err != nil {
		return report, nil, err
	}
	return report, resultSet, nil
}

func (r *Runner) ResultSet() (*kernel.ResultSet, error) {
	result := &kernel.ResultSet{
		ProjectID:         r.Project.ProjectID,
		BatchID:           r.Project.BatchID,
		CanonicalVersion:  kernel.CanonicalVersion,
		Windows:           []kernel.WindowResult{},
		Derived:           []kernel.DerivedResult{},
		Recomputations:    []kernel.Recomputation{},
		RecomputedWindows: []string{},
		Discarded:         []kernel.DiscardedRecord{},
		PathDecisions:     []kernel.PathDecision{},
		Diagnostics:       []kernel.FeatureDiagnostic{},
		WatermarkStatus:   r.Project.WatermarkPolicy,
	}
	for _, runtime := range r.Runtimes {
		output, err := runtime.Output()
		if err != nil {
			return nil, err
		}
		result.Windows = append(result.Windows, output.Results...)
		result.Recomputations = append(result.Recomputations, output.Recomputations...)
		result.Discarded = append(result.Discarded, output.Discarded...)
		result.PathDecisions = append(result.PathDecisions, output.Decision)
		result.Diagnostics = append(result.Diagnostics, output.Diagnostic)
	}

	sort.Slice(result.Recomputations, func(i, j int) bool {
		a, b := result.Recomputations[i], result.Recomputations[j]
		if a.FeatureID != b.FeatureID {
			return a.FeatureID < b.FeatureID
		}
		if a.WindowIndex != b.WindowIndex {
			return a.WindowIndex < b.WindowIndex
		}
		return a.TriggerSeq < b.TriggerSeq
	})
	sort.Slice(result.Discarded, func(i, j int) bool {
		a, b := result.Discarded[i], result.Discarded[j]
		if a.FeatureID != b.FeatureID {
			return a.FeatureID < b.FeatureID
		}
		if a.ArrivalSeq != b.ArrivalSeq {
			return a.ArrivalSeq < b.ArrivalSeq
		}
		return a.WindowIndex < b.WindowIndex
	})
	ids := []string{}
	for _, item := range result.Recomputations {
		ids = append(ids, item.WindowID)
	}
	result.RecomputedWindows = kernel.UniqueSortedStrings(ids)
	derived, err := engine.BuildDerived(r.Project, result.Windows)
	if err != nil {
		return nil, err
	}
	result.Derived = derived
	return result, nil
}

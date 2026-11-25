package gen

import (
	"sort"

	"featurekernel/internal/engine"
	"featurekernel/internal/kernel"
)

type TruthFile struct {
	SchemaVersion string           `json:"schema_version"`
	ProjectID     string           `json:"project_id"`
	BatchID       string           `json:"batch_id"`
	Seed          int64            `json:"seed"`
	Tolerance     float64          `json:"fixed_tolerance"`
	Summary       TruthSummary     `json:"summary"`
	Expected      kernel.ResultSet `json:"expected"`
}

type TruthSummary struct {
	WindowCount           int `json:"window_count"`
	RecomputedWindowCount int `json:"recomputed_window_count"`
	RecomputationCount    int `json:"recomputation_count"`
	DiscardedRecordCount  int `json:"discarded_record_count"`
	DerivedWindowCount    int `json:"derived_window_count"`
}

func BuildTruth(project *kernel.Project) (*TruthFile, error) {
	result := kernel.ResultSet{
		ProjectID:         project.ProjectID,
		BatchID:           project.BatchID,
		CanonicalVersion:  kernel.CanonicalVersion,
		Windows:           []kernel.WindowResult{},
		Derived:           []kernel.DerivedResult{},
		Recomputations:    []kernel.Recomputation{},
		RecomputedWindows: []string{},
		Discarded:         []kernel.DiscardedRecord{},
		PathDecisions:     []kernel.PathDecision{},
		WatermarkStatus:   project.WatermarkPolicy,
	}

	for _, feature := range project.Features {
		windows, recomputations, discarded, err := referenceFeature(project, feature)
		if err != nil {
			return nil, err
		}
		result.Windows = append(result.Windows, windows...)
		result.Recomputations = append(result.Recomputations, recomputations...)
		result.Discarded = append(result.Discarded, discarded...)
		result.PathDecisions = append(result.PathDecisions, kernel.SelectPath(feature.WidthMs, feature.SlideMs, project.PathPolicy))
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
	ids := make([]string, 0, len(result.Recomputations))
	for _, item := range result.Recomputations {
		ids = append(ids, item.WindowID)
	}
	result.RecomputedWindows = kernel.UniqueSortedStrings(ids)

	derived, err := engine.BuildDerived(project, result.Windows)
	if err != nil {
		return nil, err
	}
	result.Derived = derived

	return &TruthFile{
		SchemaVersion: "truth.v1", ProjectID: project.ProjectID, BatchID: project.BatchID,
		Seed: project.Seed, Tolerance: project.FixedTolerance, Expected: result,
		Summary: TruthSummary{
			WindowCount: len(result.Windows), RecomputedWindowCount: len(result.RecomputedWindows),
			RecomputationCount: len(result.Recomputations), DiscardedRecordCount: len(result.Discarded),
			DerivedWindowCount: len(result.Derived),
		},
	}, nil
}

func referenceFeature(project *kernel.Project, feature kernel.FeatureDef) ([]kernel.WindowResult, []kernel.Recomputation, []kernel.DiscardedRecord, error) {
	buckets := map[int64][]kernel.Event{}
	submitted := map[int64]bool{}
	recomputations := []kernel.Recomputation{}
	discarded := []kernel.DiscardedRecord{}
	var watermark *int64

	for _, record := range project.Records {
		if record.Kind == kernel.RecordWatermark && record.Watermark != nil {
			wm := record.Watermark.TimeMs
			watermark = &wm
			for index, events := range buckets {
				if len(events) == 0 {
					continue
				}
				coord := kernel.WindowAt(feature.AnchorMs, feature.WidthMs, feature.SlideMs, index)
				if wm >= coord.End+feature.AllowedLatenessMs {
					submitted[index] = true
				}
			}
			continue
		}
		if record.Kind != kernel.RecordEvent || record.Event == nil || record.Event.FeatureID != feature.ID {
			continue
		}
		event := record.Event
		coords, err := kernel.WindowsForTime(feature.AnchorMs, feature.WidthMs, feature.SlideMs, event.TimeMs)
		if err != nil {
			return nil, nil, nil, err
		}
		lateMs := int64(0)
		late := false
		if watermark != nil && event.TimeMs < *watermark {
			lateMs = *watermark - event.TimeMs
			late = true
		}
		if lateMs > feature.AllowedLatenessMs {
			for _, coord := range coords {
				discarded = append(discarded, kernel.DiscardedRecord{
					ArrivalSeq: event.ArrivalSeq, FeatureID: feature.ID,
					WindowID: kernel.WindowID(feature.ID, coord.Index), WindowIndex: coord.Index,
					StartMs: coord.Start, EndMs: coord.End, TimeMs: event.TimeMs,
					Channel: event.Channel, Value: event.Value, LateMs: lateMs,
					Code: engine.CodeLateBeyondAllowed,
				})
			}
			continue
		}
		for _, coord := range coords {
			existing := buckets[coord.Index]
			if late && lateMs > 0 && len(existing) > 0 {
				recomputations = append(recomputations, kernel.Recomputation{
					FeatureID: feature.ID, WindowID: kernel.WindowID(feature.ID, coord.Index), WindowIndex: coord.Index,
					TriggerSeq: event.ArrivalSeq, EventArrivalSeq: event.ArrivalSeq, LateMs: lateMs,
					Reason: "reference_late_event_within_allowed_lateness",
				})
			}
			buckets[coord.Index] = append(existing, *event)
		}
	}

	indices := make([]int64, 0, len(buckets))
	for index := range buckets {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	decision := kernel.SelectPath(feature.WidthMs, feature.SlideMs, project.PathPolicy)
	rule := kernel.AttributionRule(feature.WidthMs, feature.SlideMs)
	results := make([]kernel.WindowResult, 0, len(indices))
	for _, index := range indices {
		acc := kernel.NewAccumulator()
		for _, event := range buckets[index] {
			weight, ok := feature.ChannelWeightsPPM[event.Channel]
			if !ok {
				weight = 1_000_000
			}
			acc.Add(event.Value, weight)
		}
		coord := kernel.WindowAt(feature.AnchorMs, feature.WidthMs, feature.SlideMs, index)
		results = append(results, kernel.WindowResult{
			FeatureID: feature.ID, WindowID: kernel.WindowID(feature.ID, index), WindowIndex: index,
			StartMs: coord.Start, EndMs: coord.End, EventCount: acc.Count,
			Aggregations: acc.Results(feature.Aggregations), AttributionRule: rule,
			Path: decision.Path, Submitted: submitted[index],
		})
	}
	return results, recomputations, discarded, nil
}

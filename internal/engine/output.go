package engine

import (
	"sort"
	"time"

	"featurekernel/internal/kernel"
)

func (r *Runtime) Output() (*FeatureOutput, error) {
	results, err := r.buildResults()
	if err != nil {
		return nil, err
	}
	diagnostic := kernel.FeatureDiagnostic{
		FeatureID:          r.Feature.ID,
		ElapsedNanoseconds: time.Since(r.started).Nanoseconds(),
		EventsProcessed:    r.eventCount,
		WindowsProcessed:   len(results),
		Path:               r.ActualPath,
	}
	return &FeatureOutput{
		Results: results, Recomputations: r.Recomputations, Discarded: r.Discarded,
		Diagnostic: diagnostic, States: r.States, Decision: r.Decision,
	}, nil
}

func (r *Runtime) buildResults() ([]kernel.WindowResult, error) {
	indices := make([]int64, 0, len(r.States))
	for index := range r.States {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })

	results := make([]kernel.WindowResult, 0, len(indices))
	rule := kernel.AttributionRule(r.Feature.WidthMs, r.Feature.SlideMs)
	for _, index := range indices {
		state := r.States[index]
		acc := state.Acc
		if r.ActualPath == kernel.PathRescan {
			rescanned := kernel.NewAccumulator()
			for _, record := range r.Project.Records {
				if record.Seq > r.AppliedSeq || record.Kind != kernel.RecordEvent || record.Event == nil || record.Event.FeatureID != r.Feature.ID {
					continue
				}
				event := record.Event
				coords, err := kernel.WindowsForTime(r.Feature.AnchorMs, r.Feature.WidthMs, r.Feature.SlideMs, event.TimeMs)
				if err != nil {
					return nil, err
				}
				for _, coord := range coords {
					if coord.Index != index || r.discardKeys[DiscardKey(event.ArrivalSeq, index)] {
						continue
					}
					weight, ok := r.Feature.ChannelWeightsPPM[event.Channel]
					if !ok {
						weight = 1_000_000
					}
					rescanned.Add(event.Value, weight)
				}
			}
			acc = *rescanned
			state.Acc = acc
		}
		result := kernel.WindowResult{
			FeatureID: r.Feature.ID, WindowID: kernel.WindowID(r.Feature.ID, index), WindowIndex: index,
			StartMs: state.Coord.Start, EndMs: state.Coord.End, EventCount: acc.Count,
			Aggregations: acc.Results(r.Feature.Aggregations), AttributionRule: rule, Path: r.ActualPath,
			Submitted: state.Submitted,
		}
		results = append(results, result)
	}

	sort.Slice(r.Recomputations, func(i, j int) bool {
		if r.Recomputations[i].WindowIndex != r.Recomputations[j].WindowIndex {
			return r.Recomputations[i].WindowIndex < r.Recomputations[j].WindowIndex
		}
		return r.Recomputations[i].TriggerSeq < r.Recomputations[j].TriggerSeq
	})
	sort.Slice(r.Discarded, func(i, j int) bool {
		if r.Discarded[i].ArrivalSeq != r.Discarded[j].ArrivalSeq {
			return r.Discarded[i].ArrivalSeq < r.Discarded[j].ArrivalSeq
		}
		return r.Discarded[i].WindowIndex < r.Discarded[j].WindowIndex
	})
	return results, nil
}

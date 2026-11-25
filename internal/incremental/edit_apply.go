package incremental

import (
	"sort"

	"featurekernel/internal/kernel"
)

func applyWidth(project *kernel.Project, featureID string, width int64) error {
	for i := range project.Features {
		if project.Features[i].ID == featureID {
			project.Features[i].WidthMs = width
			return nil
		}
	}
	return kernel.NewError(kernel.CodeInvalidArgument, "cannot change width for unknown feature", map[string]any{"feature_id": featureID})
}

func applyAggregations(project *kernel.Project, featureID string, aggs []kernel.AggFunc) error {
	for i := range project.Features {
		if project.Features[i].ID == featureID {
			project.Features[i].Aggregations = append([]kernel.AggFunc(nil), aggs...)
			return nil
		}
	}
	return kernel.NewError(kernel.CodeInvalidArgument, "cannot change aggregations for unknown feature", map[string]any{"feature_id": featureID})
}

func collectEventInvalidations(oldProject kernel.Project, candidate kernel.Project, edit Edit, add func(string, string), change *ChangeSet) error {
	featureByID := map[string]kernel.FeatureDef{}
	for _, feature := range candidate.Features {
		featureByID[feature.ID] = feature
	}
	oldRecords := map[int64]kernel.Record{}
	maxSeq := int64(0)
	for _, record := range oldProject.Records {
		oldRecords[record.Seq] = record
		if record.Seq > maxSeq {
			maxSeq = record.Seq
		}
	}

	for seq := range edit.UpdateEvents {
		record, ok := oldRecords[seq]
		if !ok || record.Event == nil {
			return kernel.NewError(kernel.CodeInvalidArgument, "cannot update missing event sequence", map[string]any{"seq": seq})
		}
		feature, ok := featureByID[record.Event.FeatureID]
		if !ok {
			return kernel.NewError(kernel.CodeEventUnknownFeature, "updated event references unknown feature", map[string]any{"seq": seq, "feature_id": record.Event.FeatureID})
		}
		source := "event:" + formatSeq(seq) + ":update"
		coords, err := kernel.WindowsForTime(feature.AnchorMs, feature.WidthMs, feature.SlideMs, record.Event.TimeMs)
		if err != nil {
			return err
		}
		for _, coord := range coords {
			add(windowKey(feature.ID, coord.Index), source)
		}
		patch := edit.UpdateEvents[seq]
		newTime := record.Event.TimeMs
		if patch.TimeMs != nil {
			newTime = *patch.TimeMs
			newCoords, err := kernel.WindowsForTime(feature.AnchorMs, feature.WidthMs, feature.SlideMs, newTime)
			if err != nil {
				return err
			}
			for _, coord := range newCoords {
				add(windowKey(feature.ID, coord.Index), source)
			}
		}
		change.UpdatedEventSeqs = append(change.UpdatedEventSeqs, seq)
	}

	for _, seq := range edit.DeleteEvents {
		record, ok := oldRecords[seq]
		if !ok || record.Event == nil {
			return kernel.NewError(kernel.CodeInvalidArgument, "cannot delete missing event sequence", map[string]any{"seq": seq})
		}
		feature, ok := featureByID[record.Event.FeatureID]
		if !ok {
			return kernel.NewError(kernel.CodeEventUnknownFeature, "deleted event references unknown feature", map[string]any{"seq": seq, "feature_id": record.Event.FeatureID})
		}
		source := "event:" + formatSeq(seq) + ":delete"
		coords, err := kernel.WindowsForTime(feature.AnchorMs, feature.WidthMs, feature.SlideMs, record.Event.TimeMs)
		if err != nil {
			return err
		}
		for _, coord := range coords {
			add(windowKey(feature.ID, coord.Index), source)
		}
		change.DeletedEventSeqs = append(change.DeletedEventSeqs, seq)
	}

	nextSeq := maxSeq + 1
	for i, event := range edit.AppendEvents {
		seq := nextSeq + int64(i)
		feature, ok := featureByID[event.FeatureID]
		if !ok {
			return kernel.NewError(kernel.CodeEventUnknownFeature, "appended event references unknown feature", map[string]any{"feature_id": event.FeatureID, "seq": seq})
		}
		source := "event:" + formatSeq(seq) + ":append"
		coords, err := kernel.WindowsForTime(feature.AnchorMs, feature.WidthMs, feature.SlideMs, event.TimeMs)
		if err != nil {
			return err
		}
		for _, coord := range coords {
			add(windowKey(feature.ID, coord.Index), source)
		}
		change.AppendedEventSeqs = append(change.AppendedEventSeqs, seq)
	}

	sort.Slice(change.WidthFeatureIDs, func(i, j int) bool { return change.WidthFeatureIDs[i] < change.WidthFeatureIDs[j] })
	sort.Slice(change.AggregationFeatureIDs, func(i, j int) bool { return change.AggregationFeatureIDs[i] < change.AggregationFeatureIDs[j] })
	sort.Slice(change.UpdatedEventSeqs, func(i, j int) bool { return change.UpdatedEventSeqs[i] < change.UpdatedEventSeqs[j] })
	sort.Slice(change.DeletedEventSeqs, func(i, j int) bool { return change.DeletedEventSeqs[i] < change.DeletedEventSeqs[j] })
	sort.Slice(change.AppendedEventSeqs, func(i, j int) bool { return change.AppendedEventSeqs[i] < change.AppendedEventSeqs[j] })
	return nil
}

func applyEventEdits(project *kernel.Project, edit Edit, change *ChangeSet) {
	deleteSet := map[int64]bool{}
	for _, seq := range edit.DeleteEvents {
		deleteSet[seq] = true
	}

	for i := range project.Records {
		record := &project.Records[i]
		if record.Kind != kernel.RecordEvent || record.Event == nil {
			continue
		}
		patch, ok := edit.UpdateEvents[record.Seq]
		if !ok {
			continue
		}
		if patch.TimeMs != nil {
			record.Event.TimeMs = *patch.TimeMs
		}
		if patch.Channel != nil {
			record.Event.Channel = *patch.Channel
		}
		if patch.Value != nil {
			record.Event.Value = *patch.Value
		}
	}

	if len(deleteSet) > 0 {
		filtered := project.Records[:0]
		for _, record := range project.Records {
			if deleteSet[record.Seq] && record.Kind == kernel.RecordEvent {
				continue
			}
			filtered = append(filtered, record)
		}
		project.Records = filtered
	}

	maxSeq := int64(0)
	for _, record := range project.Records {
		if record.Seq > maxSeq {
			maxSeq = record.Seq
		}
	}
	for i, event := range edit.AppendEvents {
		seq := maxSeq + 1 + int64(i)
		event.ArrivalSeq = seq
		project.Records = append(project.Records, kernel.Record{Seq: seq, Kind: kernel.RecordEvent, Event: &event})
	}
	sort.SliceStable(project.Records, func(i, j int) bool { return project.Records[i].Seq < project.Records[j].Seq })
}

func recomputeWindow(project kernel.Project, feature kernel.FeatureDef, index int64) (kernel.WindowResult, error) {
	coord := kernel.WindowAt(feature.AnchorMs, feature.WidthMs, feature.SlideMs, index)
	watermarkBefore := map[int64]int64{}
	var currentWM *int64
	maxWM := int64(0)
	haveWM := false
	for _, record := range project.Records {
		if record.Kind == kernel.RecordWatermark && record.Watermark != nil {
			wm := record.Watermark.TimeMs
			currentWM = &wm
			if !haveWM || wm > maxWM {
				maxWM = wm
				haveWM = true
			}
		}
		if record.Kind == kernel.RecordEvent && record.Event != nil && currentWM != nil {
			watermarkBefore[record.Seq] = *currentWM
		}
	}

	acc := kernel.NewAccumulator()
	for _, record := range project.Records {
		if record.Kind != kernel.RecordEvent || record.Event == nil || record.Event.FeatureID != feature.ID {
			continue
		}
		event := record.Event
		coords, err := kernel.WindowsForTime(feature.AnchorMs, feature.WidthMs, feature.SlideMs, event.TimeMs)
		if err != nil {
			return kernel.WindowResult{}, err
		}
		belongs := false
		for _, candidate := range coords {
			if candidate.Index == index {
				belongs = true
				break
			}
		}
		if !belongs {
			continue
		}
		if wm, ok := watermarkBefore[record.Seq]; ok && event.TimeMs < wm {
			if wm-event.TimeMs > feature.AllowedLatenessMs {
				continue
			}
		}
		weight, ok := feature.ChannelWeightsPPM[event.Channel]
		if !ok {
			weight = 1_000_000
		}
		acc.Add(event.Value, weight)
	}

	submitted := false
	if haveWM {
		submitted = maxWM >= coord.End+feature.AllowedLatenessMs
	}
	return kernel.WindowResult{
		FeatureID: feature.ID, WindowID: kernel.WindowID(feature.ID, index), WindowIndex: index,
		StartMs: coord.Start, EndMs: coord.End, EventCount: acc.Count,
		Aggregations:    acc.Results(feature.Aggregations),
		AttributionRule: kernel.AttributionRule(feature.WidthMs, feature.SlideMs),
		Path:            kernel.SelectPath(feature.WidthMs, feature.SlideMs, project.PathPolicy).Path,
		Submitted:       submitted,
	}, nil
}

func formatSeq(value int64) string {
	return intToString(value)
}

func intToString(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

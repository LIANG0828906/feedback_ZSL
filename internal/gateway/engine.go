package gateway

import (
	"fmt"
	"sort"
)

type Engine struct {
	project   *Project
	histogram *Histogram
}

type partitionResult struct {
	serviceID       string
	aggs            map[string]*Aggregate
	recompute       []RecomputeEvent
	dropped         []DroppedRecord
	traces          []DurationTrace
	finalWatermark  *int64
	watermarkSource string
}

func NewEngine(project *Project) (*Engine, error) {
	if err := project.Validate(); err != nil {
		return nil, err
	}
	hist, err := NewHistogram(project.HistogramBoundariesUS, project.DurationToleranceUS)
	if err != nil {
		return nil, err
	}
	return &Engine{project: project, histogram: hist}, nil
}

func (e *Engine) Run() (*RunReport, error) {
	result, err := e.processRecords(e.project.Records, "")
	if err != nil {
		return nil, err
	}
	return e.reportFromPartitions([]*partitionResult{result})
}

func (e *Engine) processRecords(input []Record, serviceID string) (*partitionResult, error) {
	p := e.project
	records := SortedRecords(input)
	aggs := map[string]*Aggregate{}
	var recompute []RecomputeEvent
	var dropped []DroppedRecord
	traces := make([]DurationTrace, 0, len(records))

	currentWM := p.WatermarkMS
	watermarkSource := p.WatermarkSource
	if currentWM != nil {
		watermarkSource = "explicit"
	} else if p.CommittedBoundaryMS != nil {
		wm := *p.CommittedBoundaryMS
		currentWM = &wm
		watermarkSource = "presumed_committed_boundary"
	} else {
		watermarkSource = "none"
	}

	marks := append([]WatermarkMark(nil), p.WatermarkMarks...)
	sort.Slice(marks, func(i, j int) bool { return marks[i].BeforeArrivalSeq < marks[j].BeforeArrivalSeq })
	markPos := 0
	advanceMarks := func(arrivalSeq int) {
		for markPos < len(marks) && marks[markPos].BeforeArrivalSeq <= arrivalSeq {
			mark := marks[markPos]
			value := mark.WatermarkMS
			if currentWM == nil || value > *currentWM {
				currentWM = &value
			}
			watermarkSource = "explicit"
			markPos++
		}
	}

	commitDue := func() {
		if currentWM == nil {
			return
		}
		keys := make([]string, 0, len(aggs))
		for key, agg := range aggs {
			if !agg.Committed && *currentWM >= agg.EndMS+p.AllowedLatenessMS {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			aggs[key].Committed = true
		}
	}

	for _, record := range records {
		advanceMarks(record.ArrivalSeq)
		route := p.RouteByID(record.RouteID)
		if route == nil {
			return nil, NewError(CodeFieldRequired, "record.route_id", "route not found").WithContext("route_id", record.RouteID)
		}
		errorClass, timedOut := route.Classify(record.StatusCode, record.DurationUS)
		if record.ErrorClass == "" {
			record.ErrorClass = errorClass
		}
		record.TimedOut = record.TimedOut || timedOut

		indices, err := p.WindowIndices(record.EventTimeMS)
		if err != nil {
			return nil, err
		}
		fixed := record.DurationFixed
		if fixed == 0 {
			fixed, err = USSmallToFixed(record.DurationUS)
			if err != nil {
				return nil, err
			}
		}
		bucket := ClassifyFixed(fixed, e.histogram.BoundariesFixed)

		var lateness int64
		late := false
		if currentWM != nil {
			lateness = *currentWM - record.EventTimeMS
			late = lateness > 0
		}
		tooLate := late && lateness > p.AllowedLatenessMS
		trace := e.durationTrace(record, fixed, bucket, 0)
		if tooLate {
			trace.Increment = 0
			trace.Applied = false
		} else {
			trace.Applied = true
		}
		traces = append(traces, trace)

		for _, index := range indices {
			start, end := p.WindowBounds(index)
			for _, key := range e.groupKeys(index, record) {
				keyText := key.String()
				if tooLate {
					dropped = append(dropped, DroppedRecord{
						RecordID: record.ID, WindowKey: key, WindowIndex: index,
						LatenessMS: lateness, Reason: "lateness_exceeds_allowed",
					})
					continue
				}
				agg := aggs[keyText]
				if agg == nil {
					agg = &Aggregate{
						Key: key, StartMS: start, EndMS: end,
						BucketCounts:     make([]int64, len(e.histogram.BoundariesUS)+1),
						ErrorClassCounts: map[string]int64{},
						Priority:         e.priorityFor(key),
					}
					aggs[keyText] = agg
				}
				agg.RecordCount++
				agg.BucketCounts[bucket]++
				agg.RequestBytes += record.RequestBytes
				agg.ResponseBytes += record.ResponseBytes
				class := record.ErrorClass
				if class == "" {
					class = errorClass
				}
				agg.ErrorClassCounts[class]++
				agg.RecordIDs = append(agg.RecordIDs, record.ID)
				if late {
					recompute = append(recompute, RecomputeEvent{
						WindowKey: key, Source: "late_record", SourceID: record.ID,
						Reason: "event_time_before_watermark_window_open", LatenessMS: lateness,
						TriggerSeq: record.ArrivalSeq,
					})
				}
			}
		}
		commitDue()
	}

	advanceMarks(1 << 30)
	commitDue()

	return &partitionResult{
		serviceID: serviceID, aggs: aggs, recompute: recompute, dropped: dropped, traces: traces,
		finalWatermark: currentWM, watermarkSource: watermarkSource,
	}, nil
}

func (e *Engine) reportFromPartitions(parts []*partitionResult) (*RunReport, error) {
	merged := map[string]*Aggregate{}
	var recompute []RecomputeEvent
	var dropped []DroppedRecord
	var traces []DurationTrace
	var watermark *int64
	watermarkSource := "none"

	for _, part := range parts {
		if part == nil {
			continue
		}
		for key, agg := range part.aggs {
			if _, exists := merged[key]; exists {
				return nil, NewError(CodeFieldAmbiguous, "window_key", "parallel partitions wrote same window").WithContext("window_key", key)
			}
			merged[key] = agg
		}
		recompute = append(recompute, part.recompute...)
		dropped = append(dropped, part.dropped...)
		traces = append(traces, part.traces...)
		if part.finalWatermark != nil {
			if watermark == nil || *part.finalWatermark > *watermark {
				watermark = part.finalWatermark
			}
		}
		if part.watermarkSource == "explicit" {
			watermarkSource = "explicit"
		} else if watermarkSource != "explicit" && part.watermarkSource != "none" {
			watermarkSource = part.watermarkSource
		}
	}

	windowList := make([]*Aggregate, 0, len(merged))
	for _, agg := range merged {
		sort.Strings(agg.RecordIDs)
		windowList = append(windowList, agg)
	}
	sort.Slice(windowList, func(i, j int) bool { return CompareWindowKey(windowList[i].Key, windowList[j].Key) < 0 })
	sort.Slice(traces, func(i, j int) bool { return traces[i].RecordID < traces[j].RecordID })
	recompute = dedupeRecomputeEvents(recompute)
	dropped = dedupeDrops(dropped)

	report := &RunReport{
		ProjectVersion: e.project.ProjectVersion, WatermarkMS: watermark, WatermarkSource: watermarkSource,
		Windows: windowList, RecomputeEvents: recompute, RecomputedKeys: recomputedKeySet(recompute),
		Dropped: dropped, DurationTraces: traces,
	}
	batches, err := BuildBatches(e.project, windowList)
	if err != nil {
		return nil, err
	}
	report.Batches = batches
	return report, nil
}

func (e *Engine) groupKeys(index int64, record Record) []WindowKey {
	return []WindowKey{
		{Index: index, GroupKey: GroupKey{Level: "service", ServiceID: record.ServiceID}},
		{Index: index, GroupKey: GroupKey{Level: "route", ServiceID: record.ServiceID, RouteID: record.RouteID}},
		{Index: index, GroupKey: GroupKey{Level: "status", ServiceID: record.ServiceID, RouteID: record.RouteID, StatusCode: record.StatusCode}},
	}
}

func (e *Engine) priorityFor(key WindowKey) int {
	for _, rule := range e.project.Export.PriorityRules {
		if rule.Level != "" && rule.Level != key.Level {
			continue
		}
		if rule.ServiceID != "" && rule.ServiceID != key.ServiceID {
			continue
		}
		if rule.RouteID != "" && rule.RouteID != key.RouteID {
			continue
		}
		if rule.StatusCode != 0 && rule.StatusCode != key.StatusCode {
			continue
		}
		return rule.Priority
	}
	return 100
}

func (e *Engine) durationTrace(record Record, fixed FixedDuration, bucket int, iteration int) DurationTrace {
	convertedUS := int64(fixed) / DurationScale
	errUS := float64(convertedUS - record.DurationUS)
	within := absFloat(errUS) <= float64(e.histogram.ToleranceUS)
	return DurationTrace{
		RecordID: record.ID, SourceUnit: record.DurationUnit, SourceValue: record.DurationRaw,
		ConvertedUS: convertedUS, FixedRaw: int64(fixed), BucketIndex: bucket, Increment: 1,
		Applied:           true,
		ConversionErrorUS: errUS, ToleranceUS: e.histogram.ToleranceUS, WithinTolerance: within,
		RecomputeIteration: iteration,
	}
}

func CompareWindowKey(a, b WindowKey) int {
	if a.Index != b.Index {
		if a.Index < b.Index {
			return -1
		}
		return 1
	}
	levelRank := map[string]int{"service": 0, "route": 1, "status": 2}
	if levelRank[a.Level] != levelRank[b.Level] {
		return levelRank[a.Level] - levelRank[b.Level]
	}
	if a.ServiceID != b.ServiceID {
		return compareString(a.ServiceID, b.ServiceID)
	}
	if a.RouteID != b.RouteID {
		return compareString(a.RouteID, b.RouteID)
	}
	if a.StatusCode != b.StatusCode {
		if a.StatusCode < b.StatusCode {
			return -1
		}
		return 1
	}
	return 0
}

func compareString(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func dedupeRecomputeEvents(events []RecomputeEvent) []RecomputeEvent {
	seen := map[string]bool{}
	out := make([]RecomputeEvent, 0, len(events))
	for _, event := range events {
		key := event.WindowKey.String() + "|" + event.Source + "|" + event.SourceID + "|" + event.Reason
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, event)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TriggerSeq != out[j].TriggerSeq {
			return out[i].TriggerSeq < out[j].TriggerSeq
		}
		return CompareWindowKey(out[i].WindowKey, out[j].WindowKey) < 0
	})
	return out
}

func recomputedKeySet(events []RecomputeEvent) []string {
	set := map[string]bool{}
	for _, event := range events {
		set[event.WindowKey.String()] = true
	}
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func dedupeDrops(drops []DroppedRecord) []DroppedRecord {
	seen := map[string]bool{}
	out := make([]DroppedRecord, 0, len(drops))
	for _, drop := range drops {
		key := drop.RecordID + "|" + drop.WindowKey.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, drop)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WindowIndex != out[j].WindowIndex {
			return out[i].WindowIndex < out[j].WindowIndex
		}
		if out[i].RecordID != out[j].RecordID {
			return out[i].RecordID < out[j].RecordID
		}
		return out[i].WindowKey.String() < out[j].WindowKey.String()
	})
	return out
}

func AggregateDigest(agg *Aggregate) string {
	return fmt.Sprintf("%s|c=%d|b=%v|req=%d|resp=%d|err=%s", agg.Key.String(), agg.RecordCount, agg.BucketCounts, agg.RequestBytes, agg.ResponseBytes, CanonicalJSON(agg.ErrorClassCounts))
}

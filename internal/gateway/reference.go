package gateway

import "sort"

// BuildReferenceTruth is the generator's direct, intentionally straightforward
// oracle. It duplicates the declared stream semantics with map accumulation
// rather than using Engine's partition path.
func BuildReferenceTruth(p *Project) (*RunReport, error) {
	hist, err := NewHistogram(p.HistogramBoundariesUS, p.DurationToleranceUS)
	if err != nil {
		return nil, err
	}
	records := SortedRecords(p.Records)
	aggs := map[string]*Aggregate{}
	events := []RecomputeEvent{}
	drops := []DroppedRecord{}
	traces := []DurationTrace{}

	watermark := p.WatermarkMS
	source := p.WatermarkSource
	if watermark == nil && p.CommittedBoundaryMS != nil {
		value := *p.CommittedBoundaryMS
		watermark = &value
		source = "presumed_committed_boundary"
	}

	marks := append([]WatermarkMark(nil), p.WatermarkMarks...)
	sort.Slice(marks, func(i, j int) bool { return marks[i].BeforeArrivalSeq < marks[j].BeforeArrivalSeq })
	markIndex := 0
	advance := func(arrival int) {
		for markIndex < len(marks) && marks[markIndex].BeforeArrivalSeq <= arrival {
			value := marks[markIndex].WatermarkMS
			if watermark == nil || value > *watermark {
				watermark = &value
			}
			source = "explicit"
			markIndex++
		}
	}

	for _, record := range records {
		advance(record.ArrivalSeq)
		route := p.RouteByID(record.RouteID)
		if route == nil {
			return nil, NewError(CodeFieldRequired, "record.route_id", "route not found in reference truth")
		}
		class, timedOut := route.Classify(record.StatusCode, record.DurationUS)
		if record.ErrorClass != "" {
			class = record.ErrorClass
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
		bucket := ClassifyFixed(fixed, hist.BoundariesFixed)
		convertedUS := int64(fixed) / DurationScale
		deltaUS := float64(convertedUS - record.DurationUS)

		lateness := int64(0)
		late := false
		if watermark != nil {
			lateness = *watermark - record.EventTimeMS
			late = lateness > 0
		}
		tooLate := late && lateness > p.AllowedLatenessMS
		increment := int64(1)
		if tooLate {
			increment = 0
		}
		traces = append(traces, DurationTrace{
			RecordID: record.ID, SourceUnit: record.DurationUnit, SourceValue: record.DurationRaw,
			ConvertedUS: convertedUS, FixedRaw: int64(fixed), BucketIndex: bucket,
			Increment: increment, Applied: !tooLate,
			ConversionErrorUS: deltaUS, ToleranceUS: hist.ToleranceUS,
			WithinTolerance: absFloat(deltaUS) <= float64(hist.ToleranceUS),
		})

		for _, index := range indices {
			start, end := p.WindowBounds(index)
			keys := []WindowKey{
				{Index: index, GroupKey: GroupKey{Level: "service", ServiceID: record.ServiceID}},
				{Index: index, GroupKey: GroupKey{Level: "route", ServiceID: record.ServiceID, RouteID: record.RouteID}},
				{Index: index, GroupKey: GroupKey{Level: "status", ServiceID: record.ServiceID, RouteID: record.RouteID, StatusCode: record.StatusCode}},
			}
			for _, key := range keys {
				text := key.String()
				if tooLate {
					drops = append(drops, DroppedRecord{
						RecordID: record.ID, WindowKey: key, WindowIndex: index,
						LatenessMS: lateness, Reason: "lateness_exceeds_allowed",
					})
					continue
				}
				agg := aggs[text]
				if agg == nil {
					agg = &Aggregate{
						Key: key, StartMS: start, EndMS: end,
						BucketCounts:     make([]int64, len(p.HistogramBoundariesUS)+1),
						ErrorClassCounts: map[string]int64{},
						Priority:         referencePriority(p, key),
					}
					aggs[text] = agg
				}
				agg.RecordCount++
				agg.BucketCounts[bucket]++
				agg.RequestBytes += record.RequestBytes
				agg.ResponseBytes += record.ResponseBytes
				agg.ErrorClassCounts[class]++
				agg.RecordIDs = append(agg.RecordIDs, record.ID)
				if late {
					events = append(events, RecomputeEvent{
						WindowKey: key, Source: "late_record", SourceID: record.ID,
						Reason: "event_time_before_watermark_window_open", LatenessMS: lateness,
						TriggerSeq: record.ArrivalSeq,
					})
				}
			}
		}
	}
	advance(1 << 30)
	if watermark != nil {
		for _, agg := range aggs {
			agg.Committed = *watermark >= agg.EndMS+p.AllowedLatenessMS
		}
	}

	windows := make([]*Aggregate, 0, len(aggs))
	for _, agg := range aggs {
		sort.Strings(agg.RecordIDs)
		windows = append(windows, agg)
	}
	sort.Slice(windows, func(i, j int) bool { return CompareWindowKey(windows[i].Key, windows[j].Key) < 0 })
	events = dedupeRecomputeEvents(events)
	drops = dedupeDrops(drops)
	sort.Slice(traces, func(i, j int) bool { return traces[i].RecordID < traces[j].RecordID })
	batches, err := BuildBatches(p, windows)
	if err != nil {
		return nil, err
	}
	return &RunReport{
		ProjectVersion: p.ProjectVersion, WatermarkMS: watermark, WatermarkSource: source,
		Windows: windows, RecomputeEvents: events, RecomputedKeys: recomputedKeySet(events),
		Dropped: drops, Batches: batches, DurationTraces: traces,
	}, nil
}

func referencePriority(p *Project, key WindowKey) int {
	engine := &Engine{project: p}
	return engine.priorityFor(key)
}

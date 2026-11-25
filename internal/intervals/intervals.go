package intervals

import (
	"fmt"
	"sort"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/model"
	"sre-timeline/internal/timeline"
)

type Point struct {
	EventID     string `json:"event_id"`
	TimeMS      int64  `json:"time_ms"`
	Metric      string `json:"metric"`
	ValueBP     int    `json:"value_bp"`
	ThresholdBP int    `json:"threshold_bp"`
	Unhealthy   bool   `json:"unhealthy"`
	AliveMS     int64  `json:"alive_ms"`
}

type Interval struct {
	Key            string         `json:"key"`
	Ordinal        int            `json:"ordinal,omitempty"`
	ID             string         `json:"id,omitempty"`
	ServiceID      string         `json:"service_id"`
	StartMS        int64          `json:"start_ms"`
	EndMS          int64          `json:"end_ms"`
	StartInclusive bool           `json:"start_inclusive"`
	EndInclusive   bool           `json:"end_inclusive"`
	Metric         string         `json:"metric"`
	ThresholdBP    int            `json:"threshold_bp"`
	Thresholds     map[string]int `json:"thresholds"`
	PeakBP         int            `json:"peak_bp"`
	EvidenceIDs    []string       `json:"evidence_ids"`
	TriggerIDs     []string       `json:"trigger_ids"`
	ClosingIDs     []string       `json:"closing_ids,omitempty"`
	Merged         bool           `json:"merged,omitempty"`
	MergeGapMS     int64          `json:"merge_gap_ms,omitempty"`
}

type BaselineSegment struct {
	ServiceID            string   `json:"service_id"`
	StartMS              int64    `json:"start_ms"`
	EndMS                int64    `json:"end_ms"`
	StartInclusive       bool     `json:"start_inclusive"`
	EndInclusive         bool     `json:"end_inclusive"`
	SupportingIDs        []string `json:"supporting_ids"`
	BaselineErrorBP      int      `json:"baseline_error_bp"`
	BaselineSaturationBP int      `json:"baseline_saturation_bp"`
}

type ServiceResult struct {
	ServiceID string            `json:"service_id"`
	Points    []Point           `json:"points"`
	Intervals []Interval        `json:"intervals"`
	Baseline  []BaselineSegment `json:"baseline"`
}

func Derive(p *model.Project, aligned *timeline.Report) (map[string]*ServiceResult, error) {
	if err := ValidateThresholds(p); err != nil {
		return nil, err
	}
	out := map[string]*ServiceResult{}
	for _, svc := range p.Services {
		result, err := DeriveService(p, aligned, svc.ID)
		if err != nil {
			return nil, err
		}
		out[svc.ID] = result
	}
	return out, nil
}

func DeriveService(p *model.Project, aligned *timeline.Report, serviceID string) (*ServiceResult, error) {
	if err := ValidateThresholds(p); err != nil {
		return nil, err
	}
	svc, ok := p.Service(serviceID)
	if !ok {
		return nil, bizerr.New(bizerr.CodeNotFound, "service_id", "unknown service %q", serviceID)
	}
	kept := map[string]bool{}
	if aligned != nil {
		for _, ev := range aligned.Kept {
			kept[ev.EventID] = true
		}
	}
	result := &ServiceResult{ServiceID: svc.ID}
	points, err := pointsForService(svc, p.Events, kept, aligned != nil)
	if err != nil {
		return nil, err
	}
	result.Points = points
	atomic := buildAtomicIntervals(svc, points, p.Window.EndMS)
	merged := mergeIntervals(atomic, p.Policy.MergeGapMS)
	for i := range merged {
		fillEvidence(&merged[i], points)
	}
	result.Intervals = merged
	result.Baseline = buildBaseline(svc, merged, points, p.Window)
	return result, nil
}

func ValidateThresholds(p *model.Project) error {
	for i, svc := range p.Services {
		if svc.ErrorThresholdBP < 0 {
			return bizerr.New(bizerr.CodeThresholdNegative, fmt.Sprintf("services[%d].error_threshold_bp", i), "error threshold must not be negative").
				With("service", svc.ID).With("value", svc.ErrorThresholdBP)
		}
		if svc.SaturationThresholdBP < 0 {
			return bizerr.New(bizerr.CodeThresholdNegative, fmt.Sprintf("services[%d].saturation_threshold_bp", i), "saturation threshold must not be negative").
				With("service", svc.ID).With("value", svc.SaturationThresholdBP)
		}
	}
	return nil
}

func pointsForService(svc model.Service, events []model.Event, kept map[string]bool, filterKept bool) ([]Point, error) {
	var points []Point
	for _, ev := range events {
		if ev.ServiceID != svc.ID {
			continue
		}
		if filterKept && !kept[ev.ID] {
			continue
		}
		switch ev.Source {
		case model.SourceHealth:
			if ev.ErrorRateBP == nil {
				return nil, bizerr.New(bizerr.CodeSchemaField, "events."+ev.ID+".error_rate_bp", "health event requires error_rate_bp")
			}
			value := *ev.ErrorRateBP
			points = append(points, Point{EventID: ev.ID, TimeMS: ev.OccurredMS, Metric: "error_rate", ValueBP: value, ThresholdBP: svc.ErrorThresholdBP, Unhealthy: value >= svc.ErrorThresholdBP, AliveMS: ev.AliveMS})
		case model.SourceCapacity:
			if ev.SaturationBP == nil {
				return nil, bizerr.New(bizerr.CodeSchemaField, "events."+ev.ID+".saturation_bp", "capacity event requires saturation_bp")
			}
			value := *ev.SaturationBP
			points = append(points, Point{EventID: ev.ID, TimeMS: ev.OccurredMS, Metric: "saturation", ValueBP: value, ThresholdBP: svc.SaturationThresholdBP, Unhealthy: value >= svc.SaturationThresholdBP, AliveMS: ev.AliveMS})
		}
	}
	sort.SliceStable(points, func(i, j int) bool {
		if points[i].TimeMS != points[j].TimeMS {
			return points[i].TimeMS < points[j].TimeMS
		}
		return points[i].EventID < points[j].EventID
	})
	return points, nil
}

type timeState struct {
	time       int64
	points     []Point
	unhealthy  bool
	primary    Point
	thresholds map[string]int
}

func aggregateTimes(points []Point) []timeState {
	// Rebuild deterministically using sorted unique times.
	times := []int64{}
	seenTime := map[int64]bool{}
	for _, pt := range points {
		if !seenTime[pt.TimeMS] {
			seenTime[pt.TimeMS] = true
			times = append(times, pt.TimeMS)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	out := make([]timeState, 0, len(times))
	for _, ts := range times {
		st := timeState{time: ts, thresholds: map[string]int{}}
		for _, pt := range points {
			if pt.TimeMS != ts {
				continue
			}
			st.points = append(st.points, pt)
			st.thresholds[pt.Metric] = pt.ThresholdBP
			if pt.Unhealthy {
				st.unhealthy = true
				if st.primary.EventID == "" || excess(pt) > excess(st.primary) || (excess(pt) == excess(st.primary) && pt.ValueBP > st.primary.ValueBP) {
					st.primary = pt
				}
			}
		}
		if st.primary.EventID == "" && len(st.points) > 0 {
			st.primary = st.points[0]
		}
		out = append(out, st)
	}
	return out
}

func excess(pt Point) int { return pt.ValueBP - pt.ThresholdBP }

func buildAtomicIntervals(svc model.Service, points []Point, windowEnd int64) []Interval {
	states := aggregateTimes(points)
	var intervals []Interval
	var active *Interval
	var aliveMS int64 = 60_000

	closeAt := func(end int64, closing []string) {
		if active == nil {
			return
		}
		active.EndMS = end
		active.ClosingIDs = append(active.ClosingIDs, closing...)
		active.Key = intervalKey(active.ServiceID, active.StartMS, active.EndMS)
		intervals = append(intervals, *active)
		active = nil
	}

	for _, st := range states {
		if st.unhealthy {
			if active == nil {
				primary := st.primary
				active = &Interval{
					ServiceID:      svc.ID,
					StartMS:        st.time,
					StartInclusive: true,
					EndInclusive:   false,
					Metric:         primary.Metric,
					ThresholdBP:    primary.ThresholdBP,
					Thresholds:     map[string]int{},
					PeakBP:         primary.ValueBP,
				}
				for k, v := range st.thresholds {
					active.Thresholds[k] = v
				}
			}
			for _, pt := range st.points {
				active.EvidenceIDs = append(active.EvidenceIDs, pt.EventID)
				if pt.Unhealthy {
					active.TriggerIDs = append(active.TriggerIDs, pt.EventID)
					if pt.ValueBP > active.PeakBP {
						active.PeakBP = pt.ValueBP
					}
					if excess(pt) > excess(Point{Metric: active.Metric, ValueBP: active.PeakBP, ThresholdBP: active.ThresholdBP}) {
						active.Metric = pt.Metric
						active.ThresholdBP = pt.ThresholdBP
					}
				}
				if pt.AliveMS > 0 {
					aliveMS = pt.AliveMS
				}
			}
		} else if active != nil {
			var closing []string
			for _, pt := range st.points {
				closing = append(closing, pt.EventID)
			}
			closeAt(st.time, closing)
		}
	}
	if active != nil {
		end := active.StartMS + aliveMS
		if windowEnd > active.StartMS && windowEnd < end {
			end = windowEnd
		}
		closeAt(end, nil)
	}
	return intervals
}

func intervalKey(service string, start, end int64) string {
	return service + "@" + itoa(start) + ":" + itoa(end)
}

func mergeIntervals(in []Interval, mergeGapMS int64) []Interval {
	if len(in) == 0 {
		return in
	}
	sort.Slice(in, func(i, j int) bool { return in[i].StartMS < in[j].StartMS })
	out := []Interval{in[0]}
	for i := 1; i < len(in); i++ {
		cur := in[i]
		last := &out[len(out)-1]
		gap := cur.StartMS - last.EndMS
		if gap >= 0 && gap <= mergeGapMS {
			last.EndMS = cur.EndMS
			last.Merged = true
			last.MergeGapMS = mergeGapMS
			last.EvidenceIDs = append(last.EvidenceIDs, cur.EvidenceIDs...)
			last.TriggerIDs = append(last.TriggerIDs, cur.TriggerIDs...)
			last.ClosingIDs = append(last.ClosingIDs, cur.ClosingIDs...)
			if cur.PeakBP > last.PeakBP {
				last.PeakBP = cur.PeakBP
				last.Metric = cur.Metric
				last.ThresholdBP = cur.ThresholdBP
			}
			for k, v := range cur.Thresholds {
				last.Thresholds[k] = v
			}
			last.Key = intervalKey(last.ServiceID, last.StartMS, last.EndMS)
		} else {
			out = append(out, cur)
		}
	}
	return out
}

func fillEvidence(interval *Interval, points []Point) {
	interval.EvidenceIDs = nil
	interval.TriggerIDs = nil
	for _, pt := range points {
		if pt.TimeMS >= interval.StartMS && pt.TimeMS <= interval.EndMS {
			interval.EvidenceIDs = append(interval.EvidenceIDs, pt.EventID)
			if pt.Unhealthy {
				interval.TriggerIDs = append(interval.TriggerIDs, pt.EventID)
			}
		}
	}
	sort.Strings(interval.EvidenceIDs)
	sort.Strings(interval.TriggerIDs)
}

func buildBaseline(svc model.Service, intervals []Interval, points []Point, window model.Window) []BaselineSegment {
	var out []BaselineSegment
	cursor := window.StartMS
	for _, iv := range intervals {
		if iv.StartMS > cursor {
			out = append(out, makeBaseline(svc, cursor, iv.StartMS, points))
		}
		if iv.EndMS > cursor {
			cursor = iv.EndMS
		}
	}
	if cursor < window.EndMS {
		out = append(out, makeBaseline(svc, cursor, window.EndMS, points))
	}
	return out
}

func makeBaseline(service model.Service, start, end int64, points []Point) BaselineSegment {
	seg := BaselineSegment{
		ServiceID:            service.ID,
		StartMS:              start,
		EndMS:                end,
		StartInclusive:       true,
		EndInclusive:         false,
		BaselineErrorBP:      service.BaselineErrorBP,
		BaselineSaturationBP: service.BaselineSaturationBP,
	}
	for _, pt := range points {
		if pt.TimeMS >= start && pt.TimeMS < end {
			seg.SupportingIDs = append(seg.SupportingIDs, pt.EventID)
		}
	}
	sort.Strings(seg.SupportingIDs)
	return seg
}

func itoa(i int64) string {
	return fmt.Sprintf("%d", i)
}

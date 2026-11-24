package engine

import (
	"fmt"
	"math"
	"sort"

	"performanceexpressions/workspace/domain"
)

type Diff struct {
	SegmentID string  `json:"segment_id,omitempty"`
	Tick      int     `json:"tick"`
	Beat      float64 `json:"beat"`
	Channel   string  `json:"channel,omitempty"`
	Target    string  `json:"target,omitempty"`
	Field     string  `json:"field"`
	Left      any     `json:"left,omitempty"`
	Right     any     `json:"right,omitempty"`
	Message   string  `json:"message"`
}

type Comparison struct {
	Equal         bool  `json:"equal"`
	Differences   int   `json:"differences"`
	First         *Diff `json:"first_difference,omitempty"`
	LeftSegments  int   `json:"left_segments"`
	RightSegments int   `json:"right_segments"`
}

func CompareResultSets(left, right domain.ResultSet) (Comparison, error) {
	cmp := Comparison{
		LeftSegments:  len(left.Segments),
		RightSegments: len(right.Segments),
	}
	leftByID := make(map[string]domain.SegmentResult, len(left.Segments))
	rightByID := make(map[string]domain.SegmentResult, len(right.Segments))
	ids := make(map[string]struct{})
	for _, seg := range left.Segments {
		leftByID[seg.SegmentID] = seg
		ids[seg.SegmentID] = struct{}{}
	}
	for _, seg := range right.Segments {
		rightByID[seg.SegmentID] = seg
		ids[seg.SegmentID] = struct{}{}
	}
	sortedIDs := make([]string, 0, len(ids))
	for id := range ids {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)
	record := func(d Diff) {
		cmp.Differences++
		if cmp.First == nil {
			copy := d
			cmp.First = &copy
		}
	}
	for _, id := range sortedIDs {
		ls, lok := leftByID[id]
		rs, rok := rightByID[id]
		if !lok || !rok {
			record(Diff{SegmentID: id, Field: "segment.presence", Left: lok, Right: rok, Message: "segment exists on only one side"})
			continue
		}
		if ls.PriorityVersion != rs.PriorityVersion {
			record(Diff{SegmentID: id, Field: "priority_version", Left: ls.PriorityVersion, Right: rs.PriorityVersion, Message: "priority versions differ"})
		}
		leftSamples := make(map[int]domain.SampleResult, len(ls.Samples))
		rightSamples := make(map[int]domain.SampleResult, len(rs.Samples))
		ticks := make(map[int]struct{})
		for _, sample := range ls.Samples {
			leftSamples[sample.Tick] = sample
			ticks[sample.Tick] = struct{}{}
		}
		for _, sample := range rs.Samples {
			rightSamples[sample.Tick] = sample
			ticks[sample.Tick] = struct{}{}
		}
		sortedTicks := make([]int, 0, len(ticks))
		for tick := range ticks {
			sortedTicks = append(sortedTicks, tick)
		}
		sort.Ints(sortedTicks)
		for _, tick := range sortedTicks {
			lsample, lok := leftSamples[tick]
			rsample, rok := rightSamples[tick]
			if !lok || !rok {
				beat := lsample.Beat
				if !lok {
					beat = rsample.Beat
				}
				record(Diff{SegmentID: id, Tick: tick, Beat: beat, Field: "sample.presence", Left: lok, Right: rok, Message: "sample exists on only one side"})
				continue
			}
			compareSample(id, lsample, rsample, record)
		}
	}
	cmp.Equal = cmp.Differences == 0
	if !cmp.Equal {
		first := cmp.First
		err := domain.NewError(domain.CodeDiff, "first difference at segment=%s tick=%d beat=%g channel=%s field=%s", first.SegmentID, first.Tick, first.Beat, first.Channel, first.Field)
		err.SegmentID = first.SegmentID
		err.Tick = &first.Tick
		err.Beat = &first.Beat
		err.ChannelID = first.Channel
		err.Field = first.Field
		err.Left = first.Left
		err.Right = first.Right
		return cmp, err
	}
	return cmp, nil
}

func compareSample(segmentID string, left, right domain.SampleResult, record func(Diff)) {
	valueKeys := unionStringKeys(left.Values, right.Values)
	for _, target := range valueKeys {
		lv, lok := left.Values[target]
		rv, rok := right.Values[target]
		if !lok || !rok {
			record(Diff{SegmentID: segmentID, Tick: left.Tick, Beat: left.Beat, Target: target, Field: "value." + target, Left: lv, Right: rv, Message: "target exists on only one side"})
			continue
		}
		if !sameFloat(lv, rv) {
			record(Diff{SegmentID: segmentID, Tick: left.Tick, Beat: left.Beat, Channel: left.Sources[target].Channel, Target: target, Field: "value." + target, Left: lv, Right: rv, Message: "curve value differs"})
		}
	}
	sourceKeys := make(map[string]struct{})
	for target := range left.Sources {
		sourceKeys[target] = struct{}{}
	}
	for target := range right.Sources {
		sourceKeys[target] = struct{}{}
	}
	sortedSources := make([]string, 0, len(sourceKeys))
	for target := range sourceKeys {
		sortedSources = append(sortedSources, target)
	}
	sort.Strings(sortedSources)
	for _, target := range sortedSources {
		ls, lok := left.Sources[target]
		rs, rok := right.Sources[target]
		if !lok || !rok {
			record(Diff{SegmentID: segmentID, Tick: left.Tick, Beat: left.Beat, Target: target, Field: "source." + target, Left: ls, Right: rs, Message: "source exists on only one side"})
			continue
		}
		if ls.Channel != rs.Channel {
			record(Diff{SegmentID: segmentID, Tick: left.Tick, Beat: left.Beat, Channel: ls.Channel, Target: target, Field: "source." + target + ".channel", Left: ls.Channel, Right: rs.Channel, Message: "effective source differs"})
		}
		if ls.Priority != rs.Priority {
			record(Diff{SegmentID: segmentID, Tick: left.Tick, Beat: left.Beat, Channel: ls.Channel, Target: target, Field: "source." + target + ".priority", Left: ls.Priority, Right: rs.Priority, Message: "source priority differs"})
		}
		if !sameFloat(ls.Value, rs.Value) {
			record(Diff{SegmentID: segmentID, Tick: left.Tick, Beat: left.Beat, Channel: ls.Channel, Target: target, Field: "source." + target + ".value", Left: ls.Value, Right: rs.Value, Message: "source value differs"})
		}
	}
	if len(left.Decisions) != len(right.Decisions) {
		record(Diff{SegmentID: segmentID, Tick: left.Tick, Beat: left.Beat, Field: "decisions.length", Left: len(left.Decisions), Right: len(right.Decisions), Message: "decision sequence length differs"})
	}
	n := min(len(left.Decisions), len(right.Decisions))
	for i := 0; i < n; i++ {
		ld, rd := left.Decisions[i], right.Decisions[i]
		fields := []struct {
			name string
			l, r any
		}{
			{"target", ld.Target, rd.Target},
			{"channel", ld.Channel, rd.Channel},
			{"overridden", ld.Overridden, rd.Overridden},
			{"overridden_value", ld.OverriddenValue, rd.OverriddenValue},
			{"basis", ld.Basis, rd.Basis},
		}
		for _, field := range fields {
			if fmt.Sprint(field.l) != fmt.Sprint(field.r) {
				record(Diff{
					SegmentID: segmentID, Tick: left.Tick, Beat: left.Beat, Channel: ld.Channel, Target: ld.Target,
					Field: fmt.Sprintf("decision[%d].%s", i, field.name), Left: field.l, Right: field.r,
					Message: "decision detail differs",
				})
			}
		}
	}
}

func unionStringKeys(a, b map[string]float64) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for key := range a {
		seen[key] = struct{}{}
	}
	for key := range b {
		seen[key] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sameFloat(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b)
}

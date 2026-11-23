package tactical

import (
	"context"
	"fmt"
	"sort"
)

const (
	DefaultMaxStepMilli     int64 = 5000
	DefaultMaxTurnMilli     int64 = 3142
	DefaultMaxTicks               = 20000
	DefaultFormationSamples       = 65
	// QuantizationTolerance is the maximum distance, in q0.001 grid units,
	// between a fixed-point trace position and its ideal segment.
	QuantizationTolerance int64 = 1
)

type SimulationOptions struct {
	MaxStepMilli     int64
	MaxTurnMilli     int64
	MaxTicks         int
	FormationSamples int
}

func (o SimulationOptions) normalized() SimulationOptions {
	if o.MaxStepMilli <= 0 {
		o.MaxStepMilli = DefaultMaxStepMilli
	}
	if o.MaxTurnMilli <= 0 {
		o.MaxTurnMilli = DefaultMaxTurnMilli
	}
	if o.MaxTicks <= 0 {
		o.MaxTicks = DefaultMaxTicks
	}
	if o.FormationSamples <= 1 {
		o.FormationSamples = DefaultFormationSamples
	}
	return o
}

// SimulateFormation advances every member independently but annotates each
// trace against the slot anchor at the same logical tick. Movement is exactly
// constrained to the smoothed polyline in q0.001 grid units.
func SimulateFormation(s Scenario, smooth map[string]SmoothedPath, opts SimulationOptions) (map[string]Trace, error) {
	opts = opts.normalized()
	traces := make(map[string]Trace, len(s.Formation.Members))
	ids := make([]string, 0, len(s.Formation.Members))
	for _, member := range s.Formation.Members {
		ids = append(ids, member.ID)
	}
	if err := simulateMemberSet(s, smooth, ids, traces, opts); err != nil {
		return nil, err
	}
	return traces, nil
}

// SimulateFormationParallel computes each member trace in a bounded worker
// pool, merges traces through a single ordered commit callback, then performs
// anchor-dependent formation annotation serially.
func SimulateFormationParallel(
	ctx context.Context,
	s Scenario,
	smooth map[string]SmoothedPath,
	opts SimulationOptions,
	maxParallel, stateBufferLimit int,
) (map[string]Trace, SchedulerStats, error) {
	opts = opts.normalized()
	ids := SortedMemberIDs(s)
	traces := make(map[string]Trace, len(ids))
	scheduler, err := NewWindowScheduler(maxParallel, stateBufferLimit)
	if err != nil {
		return nil, SchedulerStats{}, err
	}
	windows := make([]WindowSpec, len(ids))
	for i, id := range ids {
		windows[i] = WindowSpec{Index: i, MemberIDs: []string{id}, Label: "trace:" + id}
	}
	_, stats, err := RunWindows(ctx, scheduler, windows,
		func(ctx context.Context, window WindowSpec, _ *StateBuffer) (Trace, error) {
			member, ok := MemberByID(s, window.MemberIDs[0])
			if !ok {
				return Trace{}, memberError(CodeTrace, window.MemberIDs[0], "unknown member in trace window")
			}
			path, ok := smooth[member.ID]
			if !ok {
				return Trace{}, memberError(CodeTrace, member.ID, "missing smoothed path for simulation")
			}
			return simulateMember(member, path, opts)
		},
		func(ctx context.Context, window WindowSpec, trace Trace) error {
			traces[window.MemberIDs[0]] = trace
			return nil
		},
	)
	if err != nil {
		return nil, stats, err
	}
	if err := annotateFormation(s, traces, ids); err != nil {
		return nil, stats, err
	}
	return traces, stats, nil
}

func simulateMemberSet(s Scenario, smooth map[string]SmoothedPath, ids []string, traces map[string]Trace, opts SimulationOptions) error {
	opts = opts.normalized()
	for _, id := range ids {
		member, ok := MemberByID(s, id)
		if !ok {
			return memberError(CodeTrace, id, "unknown member in simulation set")
		}
		path, ok := smooth[member.ID]
		if !ok {
			return memberError(CodeTrace, member.ID, "missing smoothed path for simulation")
		}
		trace, err := simulateMember(member, path, opts)
		if err != nil {
			return err
		}
		traces[member.ID] = trace
	}
	return annotateFormation(s, traces, ids)
}

func simulateMemberSetParallel(
	ctx context.Context,
	s Scenario,
	smooth map[string]SmoothedPath,
	ids []string,
	traces map[string]Trace,
	opts SimulationOptions,
	maxParallel, stateBufferLimit int,
) (SchedulerStats, error) {
	opts = opts.normalized()
	if len(ids) == 0 {
		return SchedulerStats{}, nil
	}
	scheduler, err := NewWindowScheduler(maxParallel, stateBufferLimit)
	if err != nil {
		return SchedulerStats{}, err
	}
	windows := make([]WindowSpec, len(ids))
	for i, id := range ids {
		windows[i] = WindowSpec{Index: i, MemberIDs: []string{id}, Label: "trace:" + id}
	}
	_, stats, err := RunWindows(ctx, scheduler, windows,
		func(ctx context.Context, window WindowSpec, _ *StateBuffer) (Trace, error) {
			member, ok := MemberByID(s, window.MemberIDs[0])
			if !ok {
				return Trace{}, memberError(CodeTrace, window.MemberIDs[0], "unknown member in trace window")
			}
			path, ok := smooth[member.ID]
			if !ok {
				return Trace{}, memberError(CodeTrace, member.ID, "missing smoothed path for simulation")
			}
			return simulateMember(member, path, opts)
		},
		func(ctx context.Context, window WindowSpec, trace Trace) error {
			traces[window.MemberIDs[0]] = trace
			return nil
		},
	)
	if err != nil {
		return stats, err
	}
	return stats, annotateFormation(s, traces, ids)
}

func annotateFormation(s Scenario, traces map[string]Trace, ids []string) error {
	for _, id := range ids {
		member, _ := MemberByID(s, id)
		trace := traces[member.ID]
		slot, ok := SlotByID(s, member.SlotID)
		if !ok {
			return memberError(CodeTrace, member.ID, "missing slot for formation annotation")
		}
		anchorTrace, ok := traces[slot.Anchor]
		if !ok {
			return memberError(CodeTrace, member.ID, "missing anchor trace "+slot.Anchor)
		}
		dx, dxOK := MilliFloat(slot.DX)
		dy, dyOK := MilliFloat(slot.DY)
		if !dxOK || !dyOK {
			return memberError(CodeTrace, member.ID, "slot offset is not finite")
		}
		stats := FormationStats{Samples: int64(len(trace.States))}
		for i := range trace.States {
			anchorState := anchorTrace.States[minInt(i, len(anchorTrace.States)-1)]
			expected := FixPt{X: anchorState.Position.X + dx, Y: anchorState.Position.Y + dy}
			deviation := DistanceMilli(expected, trace.States[i].Position)
			trace.States[i].FormationDeviationMilli = deviation
			if deviation > stats.MaxDeviationMilli {
				stats.MaxDeviationMilli = deviation
			}
			stats.SumDeviationMilli += deviation
		}
		trace.Stats = stats
		traces[member.ID] = trace
	}
	return nil
}

func simulateMember(member Member, path SmoothedPath, opts SimulationOptions) (Trace, error) {
	if len(path.Corners) < 2 {
		return Trace{}, memberError(CodeTrace, member.ID, "simulation requires at least two corners")
	}
	if member.StepMilli <= 0 || member.StepMilli > opts.MaxStepMilli {
		return Trace{}, &Error{
			Code: CodePara, MemberID: member.ID,
			Message: fmt.Sprintf("step %d is outside 1..%d", member.StepMilli, opts.MaxStepMilli),
		}
	}
	if member.TurnRateMilli <= 0 || member.TurnRateMilli > opts.MaxTurnMilli {
		return Trace{}, &Error{
			Code: CodePara, MemberID: member.ID,
			Message: fmt.Sprintf("turn rate %d is outside 1..%d", member.TurnRateMilli, opts.MaxTurnMilli),
		}
	}
	points := make([]FixPt, len(path.Corners))
	for i, corner := range path.Corners {
		points[i] = corner.Fixed
	}
	position := points[0]
	segment := 0
	segmentTraveled := int64(0)
	heading := AngleTo(points[0], points[1])
	trace := Trace{MemberID: member.ID}
	for tick := 0; ; tick++ {
		if tick > opts.MaxTicks {
			return Trace{}, &Error{Code: CodeTrace, MemberID: member.ID, Tick: tick, Message: "tick budget exhausted before path completion"}
		}
		if tick > 0 {
			if position == points[len(points)-1] {
				break
			}
			if position == points[segment+1] && segment+1 < len(points)-1 {
				segment++
				segmentTraveled = 0
			}
			desired := AngleTo(points[segment], points[segment+1])
			delta := AngleDelta(heading, desired)
			if delta != 0 {
				maxTurn := member.TurnRateMilli
				if absInt64(delta) <= maxTurn {
					heading = desired
				} else {
					heading = NormalizeAngle(heading + Sign(delta)*maxTurn)
				}
				if heading != desired {
					minX, maxX, minY, maxY := segmentBounds(points[segment], points[segment+1], member.StepMilli/100+CoordinateScale)
					if position.X < minX || position.X > maxX || position.Y < minY || position.Y > maxY {
						return Trace{}, &Error{Code: CodeTrace, MemberID: member.ID, Tick: tick, Message: "turn pivot left the active path segment corridor"}
					}
					trace.States = append(trace.States, TickState{
						Tick: tick, MemberID: member.ID, Position: position,
						World: position.World(), HeadingMilli: heading, Segment: segment,
						AtCorner: isCorner(position, points),
					})
					continue
				}
			}
			remaining := member.StepMilli
			for remaining > 0 {
				start := points[segment]
				end := points[segment+1]
				length := DistanceMilli(start, end)
				if length == 0 {
					if segment+1 >= len(points)-1 {
						break
					}
					segment++
					segmentTraveled = 0
					continue
				}
				remainingOnSegment := length - segmentTraveled
				if remainingOnSegment <= 0 {
					position = end
					if segment+1 >= len(points)-1 {
						break
					}
					break
				}
				advance := remaining
				if advance > remainingOnSegment {
					advance = remainingOnSegment
				}
				segmentTraveled += advance
				next := end
				if segmentTraveled < length {
					next = FixPt{
						X: start.X + (end.X-start.X)*segmentTraveled/length,
						Y: start.Y + (end.Y-start.Y)*segmentTraveled/length,
					}
				}
				if !pointOnSegment(position, next, points[segment], end) {
					return Trace{}, &Error{Code: CodeTrace, MemberID: member.ID, Tick: tick, Message: "quantized movement left the current polyline segment"}
				}
				position = next
				remaining -= advance
				if position == end {
					if segment+1 >= len(points)-1 {
						break
					}
					break
				}
			}
		}
		trace.States = append(trace.States, TickState{
			Tick: tick, MemberID: member.ID, Position: position,
			World: position.World(), HeadingMilli: heading, Segment: segment,
			AtCorner: isCorner(position, points),
		})
		if position == points[len(points)-1] {
			break
		}
	}
	if err := ValidateTrace(member, path, trace, opts); err != nil {
		return Trace{}, err
	}
	return trace, nil
}

func ValidateTrace(member Member, path SmoothedPath, trace Trace, opts SimulationOptions) error {
	opts = opts.normalized()
	if len(path.Corners) < 2 {
		return memberError(CodeTrace, member.ID, "trace references a path with fewer than two corners")
	}
	points := make([]FixPt, len(path.Corners))
	for i, corner := range path.Corners {
		points[i] = corner.Fixed
	}
	lastSegment := -1
	for i, state := range trace.States {
		if state.Tick != i {
			return &Error{Code: CodeTrace, MemberID: member.ID, Tick: i, Message: "trace ticks are not contiguous"}
		}
		if state.MemberID != member.ID {
			return &Error{Code: CodeTrace, MemberID: member.ID, Tick: state.Tick, Message: "trace state has a mismatched member id"}
		}
		if state.Segment < 0 || state.Segment >= len(points)-1 {
			return &Error{Code: CodeTrace, MemberID: member.ID, Tick: state.Tick, Message: "trace segment index is outside the polyline"}
		}
		if lastSegment >= 0 && state.Segment < lastSegment {
			return &Error{Code: CodeTrace, MemberID: member.ID, Tick: state.Tick, Message: "trace segment index moved backwards"}
		}
		if lastSegment >= 0 && state.Segment > lastSegment+1 {
			return &Error{Code: CodeTrace, MemberID: member.ID, Tick: state.Tick, Message: "trace skipped a path segment"}
		}
		if !pointOnSegment(state.Position, state.Position, points[state.Segment], points[state.Segment+1]) {
			return &Error{Code: CodeTrace, MemberID: member.ID, Tick: state.Tick, Message: "trace position is outside its path segment"}
		}
		if i > 0 {
			delta := absInt64(AngleDelta(trace.States[i-1].HeadingMilli, state.HeadingMilli))
			if delta > member.TurnRateMilli+1 {
				return &Error{Code: CodeTrace, MemberID: member.ID, Tick: state.Tick, Message: fmt.Sprintf("heading changed by %d in one tick, exceeding turn rate %d", delta, member.TurnRateMilli)}
			}
		}
		lastSegment = state.Segment
	}
	return nil
}

func pointOnSegment(previous, current, a, b FixPt) bool {
	_ = previous
	if current == a || current == b {
		return true
	}
	if !DistanceSquaredBelow(a, b, current, QuantizationTolerance) {
		return false
	}
	minX, maxX := a.X, b.X
	if minX > maxX {
		minX, maxX = maxX, minX
	}
	minY, maxY := a.Y, b.Y
	if minY > maxY {
		minY, maxY = maxY, minY
	}
	return current.X >= minX && current.X <= maxX && current.Y >= minY && current.Y <= maxY
}

func isCorner(position FixPt, points []FixPt) bool {
	for _, point := range points {
		if point == position {
			return true
		}
	}
	return false
}

func segmentBounds(a, b FixPt, padding int64) (int64, int64, int64, int64) {
	minX, maxX := a.X, b.X
	if minX > maxX {
		minX, maxX = maxX, minX
	}
	minY, maxY := a.Y, b.Y
	if minY > maxY {
		minY, maxY = maxY, minY
	}
	return minX - padding, maxX + padding, minY - padding, maxY + padding
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// FormationDeviationAt returns the fixed-point deviation for a requested member
// and tick. Missing ticks are clamped to the final trace state for diagnostics.
func FormationDeviationAt(s Scenario, traces map[string]Trace, memberID string, tick int) (int64, bool) {
	trace, ok := traces[memberID]
	if !ok || len(trace.States) == 0 || tick < 0 {
		return 0, false
	}
	state := trace.States[minInt(tick, len(trace.States)-1)]
	return state.FormationDeviationMilli, true
}

func TraceMemberIDs(traces map[string]Trace) []string {
	ids := make([]string, 0, len(traces))
	for id := range traces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

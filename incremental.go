package cachebench

import (
	"fmt"
	"reflect"
	"sort"
)

// BoundaryCompletion is a serializable completed-work fact retained at a tick
// boundary for breaker error-rate windows and scheduled completion replay.
type BoundaryCompletion struct {
	NodeID    string
	RequestID string
	IsError   bool
	LatencyMS int64
	AtMS      int64
}

func (c BoundaryCompletion) toCompletion() completion {
	return completion{nodeID: c.NodeID, requestID: c.RequestID, isError: c.IsError, latencyMS: c.LatencyMS, atMS: c.AtMS}
}

// BoundaryNodeState contains exactly the mutable node state needed to resume.
type BoundaryNodeState struct {
	Inflight    int
	HighSinceMS int64
	CircuitOpen bool
	OpenSinceMS int64
	Recent      []BoundaryCompletion
}

// BoundaryState is a committed logical-time seed for suffix recomputation.
type BoundaryState struct {
	TickMS int64
	Nodes  map[string]BoundaryNodeState
	Due    []BoundaryCompletion
}

func cloneBoundary(tick int64, ids []string, rts map[string]*nodeRuntime, due map[int64][]completion) *BoundaryState {
	b := &BoundaryState{TickMS: tick, Nodes: map[string]BoundaryNodeState{}}
	for _, id := range ids {
		rt := rts[id]
		ns := BoundaryNodeState{Inflight: rt.inflight, HighSinceMS: rt.highSince, CircuitOpen: rt.open, OpenSinceMS: rt.openSince}
		for _, c := range rt.recent {
			ns.Recent = append(ns.Recent, BoundaryCompletion{NodeID: c.nodeID, RequestID: c.requestID, IsError: c.isError, LatencyMS: c.latencyMS, AtMS: c.atMS})
		}
		b.Nodes[id] = ns
	}
	times := make([]int64, 0, len(due))
	for at := range due {
		if at > tick {
			times = append(times, at)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	for _, at := range times {
		for _, c := range due[at] {
			b.Due = append(b.Due, BoundaryCompletion{NodeID: c.nodeID, RequestID: c.requestID, IsError: c.isError, LatencyMS: c.latencyMS, AtMS: c.atMS})
		}
	}
	return b
}

func (s *BoundaryState) recentCompletions() []completion {
	out := make([]completion, 0, len(s.Nodes))
	for _, ns := range s.Nodes {
		for _, c := range ns.Recent {
			out = append(out, c.toCompletion())
		}
	}
	return out
}

type EditKind string

const (
	EditMaxInflight      EditKind = "max_inflight"
	EditNodeWeight       EditKind = "node_weight"
	EditBreakerThreshold EditKind = "breaker_threshold"
	EditAddRequest       EditKind = "add_request"
	EditDeleteRequest    EditKind = "delete_request"
)

type Edit struct {
	ID        string
	Kind      EditKind
	NodeID    string
	AtMS      int64
	IntValue  int
	Request   *Request
	RequestID string
}

type CellProvenance struct {
	CellKey           string
	SourceEditID      string
	SourceDescription string
	ClosureID         string
	Recomputed        bool
	Reused            bool
}

type IncrementalReport struct {
	EditID            string
	ClosureID         string
	FromMS            int64
	StartMS           int64
	RecomputedTicks   []int64
	ReusedTicks       []int64
	RecomputedWindows []int
	ReusedWindows     []int
	Closure           []string
	Provenance        map[string]CellProvenance
	Result            *SimulationResult
	RecomputedCells   int
	ReusedCells       int
}

type IncrementalEngine struct {
	project     Project
	result      *SimulationResult
	boundaries  map[int64]*BoundaryState
	tickCache   map[int64]TickResult
	windowCache map[int]WindowStat
	edits       []Edit
	Reports     []*IncrementalReport
}

func NewIncrementalEngine(p Project) (*IncrementalEngine, error) {
	sim, boundaries, err := SimulateWithBoundaries(p)
	if err != nil {
		return nil, err
	}
	e := &IncrementalEngine{
		project: p, result: sim, boundaries: boundaries,
		tickCache: map[int64]TickResult{}, windowCache: map[int]WindowStat{},
	}
	for _, t := range sim.Ticks {
		e.tickCache[t.TickMS] = t
	}
	for _, w := range sim.Windows {
		e.windowCache[w.WindowIndex] = w
	}
	return e, nil
}

func (e *IncrementalEngine) Result() *SimulationResult { return e.result }
func (e *IncrementalEngine) Project() Project          { return e.project }

func CloneProject(p Project) Project {
	q := p
	q.Nodes = append([]Node(nil), p.Nodes...)
	q.Requests = append([]Request(nil), p.Requests...)
	q.Observations = append([]Observation(nil), p.Observations...)
	q.CriticalCases = append([]CriticalCase(nil), p.CriticalCases...)
	q.Overrides = append([]NodeOverride(nil), p.Overrides...)
	for i := range q.Requests {
		q.Requests[i].Candidates = append([]string(nil), p.Requests[i].Candidates...)
	}
	return q
}

// ApplyEdit validates the edited candidate before replacing engine state. A
// failed edit leaves the previous project and caches untouched.
func (e *IncrementalEngine) ApplyEdit(edit Edit) (*IncrementalReport, error) {
	candidate := CloneProject(e.project)
	if edit.ID == "" {
		edit.ID = fmt.Sprintf("edit-%d", len(e.edits)+1)
	}
	source := ""
	fromMS := edit.AtMS
	switch edit.Kind {
	case EditMaxInflight:
		n, ok := candidate.NodeByID(edit.NodeID)
		if !ok {
			return nil, NewError(CodeNodeNotFound, "edit node missing").WithNode(edit.NodeID).WithField("node_id")
		}
		candidate.Overrides = append(candidate.Overrides, NodeOverride{AtMS: edit.AtMS, NodeID: edit.NodeID, MaxInflight: edit.IntValue, SetMaxInflight: true})
		source = fmt.Sprintf("%s node=%s old=%d new=%d", edit.Kind, edit.NodeID, n.MaxInflight, edit.IntValue)
	case EditNodeWeight:
		n, ok := candidate.NodeByID(edit.NodeID)
		if !ok {
			return nil, NewError(CodeNodeNotFound, "edit node missing").WithNode(edit.NodeID).WithField("node_id")
		}
		candidate.Overrides = append(candidate.Overrides, NodeOverride{AtMS: edit.AtMS, NodeID: edit.NodeID, WeightPermyriad: int64(edit.IntValue), SetWeight: true})
		source = fmt.Sprintf("%s node=%s old=%d new=%d", edit.Kind, edit.NodeID, n.WeightPermyriad, edit.IntValue)
	case EditBreakerThreshold:
		n, ok := candidate.NodeByID(edit.NodeID)
		if !ok {
			return nil, NewError(CodeNodeNotFound, "edit node missing").WithNode(edit.NodeID).WithField("node_id")
		}
		candidate.Overrides = append(candidate.Overrides, NodeOverride{AtMS: edit.AtMS, NodeID: edit.NodeID, InflightThreshold: edit.IntValue, SetInflightThreshold: true})
		source = fmt.Sprintf("%s node=%s old=%d new=%d", edit.Kind, edit.NodeID, n.InflightThreshold, edit.IntValue)
	case EditAddRequest:
		if edit.Request == nil {
			return nil, NewError(CodeInvalidArgument, "add_request edit requires request").WithField("request")
		}
		r := *edit.Request
		if r.Ordinal == 0 {
			r.Ordinal = len(candidate.Requests) + 1
		}
		candidate.Requests = append(candidate.Requests, r)
		fromMS = r.AtMS
		source = fmt.Sprintf("%s request=%s at=%d", edit.Kind, r.ID, r.AtMS)
	case EditDeleteRequest:
		idx := -1
		for i, r := range candidate.Requests {
			if r.ID == edit.RequestID {
				idx = i
				fromMS = r.AtMS
				break
			}
		}
		if idx < 0 {
			return nil, NewError(CodeInvalidArgument, "delete target request not found").WithField("request_id").WithContext("request_id", edit.RequestID)
		}
		candidate.Requests = append(candidate.Requests[:idx], candidate.Requests[idx+1:]...)
		source = fmt.Sprintf("%s request=%s at=%d", edit.Kind, edit.RequestID, fromMS)
	default:
		return nil, NewError(CodeInvalidArgument, "unknown edit kind %q", edit.Kind).WithField("kind")
	}
	if err := ValidateProject(candidate); err != nil {
		return nil, err
	}

	oldHorizon := e.result.FinalTick
	seedTick := fromMS - 1
	if seedTick > oldHorizon {
		seedTick = oldHorizon
	}
	startMS := fromMS
	var initial *BoundaryState
	if seedTick >= 0 {
		initial = e.boundaries[seedTick]
		if initial == nil {
			return nil, NewError(CodeIncrementalConflict, "missing cached boundary").WithTick(seedTick)
		}
	} else {
		startMS = 0
	}
	if seedTick >= oldHorizon {
		startMS = oldHorizon + 1
		initial = e.boundaries[oldHorizon]
	}

	suffix, suffixBounds, err := simulateRange(candidate, startMS, initial, true)
	if err != nil {
		return nil, err
	}

	stitched := &SimulationResult{ProjectID: candidate.ProjectID, CheckpointVer: CheckpointSchemaVersion, FinalTick: suffix.FinalTick, NodeIDs: append([]string(nil), suffix.NodeIDs...)}
	for _, t := range e.result.Ticks {
		if t.TickMS < startMS {
			stitched.Ticks = append(stitched.Ticks, t)
		}
	}
	stitched.Ticks = append(stitched.Ticks, suffix.Ticks...)
	stitched.Windows = BuildWindows(candidate, stitched)

	report := e.buildReport(edit, source, fromMS, startMS, candidate, stitched)

	// Commit only after all recomputation succeeded.
	e.project = candidate
	e.result = stitched
	for k, v := range suffixBounds {
		e.boundaries[k] = v
	}
	for _, t := range suffix.Ticks {
		e.tickCache[t.TickMS] = t
	}
	firstWindow := int(startMS / candidate.WindowMS)
	for w := 0; w < firstWindow && w < len(stitched.Windows); w++ {
		if cached, ok := e.windowCache[w]; ok {
			stitched.Windows[w] = cached
		}
	}
	for _, w := range stitched.Windows {
		e.windowCache[w.WindowIndex] = w
	}
	e.edits = append(e.edits, edit)
	e.Reports = append(e.Reports, report)
	return report, nil
}

func (e *IncrementalEngine) buildReport(edit Edit, source string, fromMS, startMS int64, p Project, result *SimulationResult) *IncrementalReport {
	closureID := "closure-" + edit.ID
	rep := &IncrementalReport{EditID: edit.ID, ClosureID: closureID, FromMS: fromMS, StartMS: startMS, Result: result, Provenance: map[string]CellProvenance{}}
	rep.Closure = append(rep.Closure, "source:"+source)

	for t := int64(0); t <= result.FinalTick; t++ {
		key := fmt.Sprintf("tick:%d", t)
		pv := CellProvenance{CellKey: key, SourceEditID: edit.ID, SourceDescription: source, ClosureID: closureID}
		if t < startMS {
			pv.Reused = true
			rep.ReusedTicks = append(rep.ReusedTicks, t)
			rep.ReusedCells++
		} else {
			pv.Recomputed = true
			rep.RecomputedTicks = append(rep.RecomputedTicks, t)
			rep.RecomputedCells++
			rep.Closure = append(rep.Closure, key)
		}
		rep.Provenance[key] = pv
		for _, n := range p.Nodes {
			nkey := fmt.Sprintf("node-tick:%s:%d", n.ID, t)
			npv := pv
			npv.CellKey = nkey
			if t >= startMS {
				rep.RecomputedCells++
				rep.Closure = append(rep.Closure, nkey)
			} else {
				rep.ReusedCells++
			}
			rep.Provenance[nkey] = npv
		}
	}
	firstWindow := int(startMS / p.WindowMS)
	for _, w := range result.Windows {
		key := fmt.Sprintf("window:%d", w.WindowIndex)
		pv := CellProvenance{CellKey: key, SourceEditID: edit.ID, SourceDescription: source, ClosureID: closureID}
		if w.WindowIndex < firstWindow {
			if cached, ok := e.windowCache[w.WindowIndex]; ok {
				pv.Reused = true
				rep.ReusedWindows = append(rep.ReusedWindows, w.WindowIndex)
				rep.ReusedCells++
				_ = cached
			}
		}
		if !pv.Reused {
			pv.Recomputed = true
			rep.RecomputedWindows = append(rep.RecomputedWindows, w.WindowIndex)
			rep.RecomputedCells++
			rep.Closure = append(rep.Closure, key)
		}
		rep.Provenance[key] = pv
	}
	sort.Strings(rep.Closure)
	return rep
}

// VerifyAgainstFull is a test/diagnostic helper proving cache stitching equals
// a one-shot full recomputation on every logical tick.
func (e *IncrementalEngine) VerifyAgainstFull() (*SimulationResult, bool, error) {
	full, err := Simulate(e.project)
	if err != nil {
		return nil, false, err
	}
	ok := simulationEqual(e.result, full)
	return full, ok, nil
}

func simulationEqual(a, b *SimulationResult) bool {
	return ResultsEqual(a, b)
}

// ResultsEqual compares all externally observable ticks, decisions, overflow
// sets, breaker events and windows.
func ResultsEqual(a, b *SimulationResult) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.FinalTick != b.FinalTick || a.ProjectID != b.ProjectID || len(a.Ticks) != len(b.Ticks) || len(a.Windows) != len(b.Windows) {
		return false
	}
	for i := range a.Ticks {
		if !tickEqual(a.Ticks[i], b.Ticks[i]) {
			return false
		}
	}
	for i := range a.Windows {
		if a.Windows[i] != b.Windows[i] {
			return false
		}
	}
	return true
}

func tickEqual(a, b TickResult) bool {
	if a.TickMS != b.TickMS || a.TotalThroughput != b.TotalThroughput || len(a.Nodes) != len(b.Nodes) || len(a.Overflow) != len(b.Overflow) || len(a.Decisions) != len(b.Decisions) || len(a.BreakerEvents) != len(b.BreakerEvents) || len(a.OpenCircuits) != len(b.OpenCircuits) {
		return false
	}
	for i := range a.Nodes {
		if a.Nodes[i] != b.Nodes[i] {
			return false
		}
	}
	for i := range a.Overflow {
		if a.Overflow[i] != b.Overflow[i] {
			return false
		}
	}
	for i := range a.Decisions {
		if !reflect.DeepEqual(a.Decisions[i], b.Decisions[i]) {
			return false
		}
	}
	for i := range a.BreakerEvents {
		if a.BreakerEvents[i] != b.BreakerEvents[i] {
			return false
		}
	}
	for i := range a.OpenCircuits {
		if a.OpenCircuits[i] != b.OpenCircuits[i] {
			return false
		}
	}
	return true
}

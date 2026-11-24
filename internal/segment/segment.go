package segment

import (
	"fmt"
	"math"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/hashx"
	"gameintegrity/internal/model"
)

const (
	StatePending   = "pending"
	StateVerifying = "verifying"
	StateCommitted = "committed"
	StateFailed    = "failed"
)

type Segment struct {
	Index      int
	StartFrame int
	EndFrame   int
	State      string
	Result     *model.SegmentResult
}

type Machine struct {
	segments  []*Segment
	committed int
	failed    int
}

func Plan(frameCount int, frameDurationUS int64, segmentSeconds float64) ([]Segment, error) {
	if frameCount <= 0 || frameDurationUS <= 0 || segmentSeconds <= 0 {
		e := apperr.New(apperr.Para, "cannot plan segments for invalid dimensions or duration")
		e.Field = "segment_seconds"
		e.Expected = "positive frame_count, frame_duration_us and segment_seconds"
		e.Actual = []any{frameCount, frameDurationUS, segmentSeconds}
		return nil, e
	}
	segUS := segmentSeconds * 1_000_000
	perSegment := int(math.Round(segUS / float64(frameDurationUS)))
	if perSegment < 1 {
		perSegment = 1
	}
	total := (frameCount + perSegment - 1) / perSegment
	out := make([]Segment, total)
	for i := 0; i < total; i++ {
		start := i * perSegment
		end := min(frameCount-1, start+perSegment-1)
		out[i] = Segment{Index: i, StartFrame: start, EndFrame: end, State: StatePending}
	}
	return out, nil
}

func New(segments []Segment) *Machine {
	m := &Machine{segments: make([]*Segment, len(segments))}
	for i := range segments {
		s := segments[i]
		m.segments[i] = &s
	}
	return m
}

func NewWithCommitted(segments []Segment, committed []model.SegmentResult) (*Machine, error) {
	m := New(segments)
	for i := range committed {
		r := committed[i]
		if r.Index < 0 || r.Index >= len(m.segments) {
			e := apperr.New(apperr.State, "checkpoint segment index is outside the plan")
			e.Field = "segment_index"
			e.Expected = fmt.Sprintf("0..%d", len(m.segments)-1)
			e.Actual = r.Index
			return nil, e
		}
		if r.Index != i {
			e := apperr.New(apperr.State, "checkpoint commits are not in stable order")
			e.Field = "segment_index"
			e.Expected = i
			e.Actual = r.Index
			return nil, e
		}
		m.segments[i].State = StateCommitted
		copyResult := cloneResult(r)
		m.segments[i].Result = &copyResult
	}
	m.committed = len(committed)
	return m, nil
}

func (m *Machine) Segments() []Segment {
	out := make([]Segment, len(m.segments))
	for i := range m.segments {
		out[i] = *m.segments[i]
		if m.segments[i].Result != nil {
			r := cloneResult(*m.segments[i].Result)
			out[i].Result = &r
		}
	}
	return out
}

func (m *Machine) Segment(index int) (Segment, bool) {
	if index < 0 || index >= len(m.segments) {
		return Segment{}, false
	}
	return *m.segments[index], true
}

func (m *Machine) State(index int) string {
	if index < 0 || index >= len(m.segments) {
		return ""
	}
	return m.segments[index].State
}

func (m *Machine) Progress() (committed, total int) {
	return m.committed, len(m.segments)
}

func (m *Machine) FailedCount() int { return m.failed }

func (m *Machine) Transition(index int, to string) error {
	if index < 0 || index >= len(m.segments) {
		e := apperr.New(apperr.State, "segment index is outside the plan")
		e.Field = "segment_index"
		e.Expected = fmt.Sprintf("0..%d", len(m.segments)-1)
		e.Actual = index
		return e
	}
	s := m.segments[index]
	if !legal(s.State, to) {
		e := apperr.New(apperr.State, "illegal segment state transition")
		e.Field = "state"
		e.Expected = allowedFrom(s.State)
		e.Actual = fmt.Sprintf("%s->%s", s.State, to)
		e.Details = map[string]any{"segment": index, "current_state": s.State, "requested_state": to}
		return e
	}
	switch to {
	case StateCommitted:
		if s.Result == nil {
			e := apperr.New(apperr.State, "cannot commit a segment without a result")
			e.Field = "segment_result"
			e.Actual = "nil"
			return e
		}
		if s.State == StateFailed {
			m.failed--
		}
		if s.State != StateCommitted {
			m.committed++
		}
	case StateVerifying:
		if s.State == StateFailed {
			m.failed--
		}
	case StateFailed:
		if s.State != StateFailed {
			m.failed++
		}
	}
	s.State = to
	return nil
}

func (m *Machine) Begin(index int) error { return m.Transition(index, StateVerifying) }

func (m *Machine) Commit(index int, result model.SegmentResult) error {
	if index < 0 || index >= len(m.segments) {
		return m.Transition(index, StateCommitted)
	}
	s := m.segments[index]
	if s.State != StateVerifying {
		return m.Transition(index, StateCommitted)
	}
	if result.Index != index {
		e := apperr.New(apperr.State, "segment result index does not match its slot")
		e.Field = "segment_index"
		e.Expected = index
		e.Actual = result.Index
		return e
	}
	frozen := cloneResult(result)
	frozen.State = StateCommitted
	frozen.Summary = hashx.SegmentSummary(frozen.Frames)
	s.Result = &frozen
	return m.Transition(index, StateCommitted)
}

func (m *Machine) Fail(index int) error  { return m.Transition(index, StateFailed) }
func (m *Machine) Retry(index int) error { return m.Transition(index, StateVerifying) }

func (m *Machine) CommittedResults() []model.SegmentResult {
	out := make([]model.SegmentResult, 0, m.committed)
	for i := range m.segments {
		if m.segments[i].State == StateCommitted && m.segments[i].Result != nil {
			out = append(out, cloneResult(*m.segments[i].Result))
		}
	}
	return out
}

func (m *Machine) CommittedThrough() int {
	n := 0
	for i := range m.segments {
		if m.segments[i].State != StateCommitted {
			break
		}
		n++
	}
	return n
}

func (m *Machine) States() []model.SegmentState {
	out := make([]model.SegmentState, len(m.segments))
	for i, s := range m.segments {
		out[i] = model.SegmentState{
			Index: i, StartFrame: s.StartFrame, EndFrame: s.EndFrame, State: s.State,
		}
	}
	return out
}

func legal(from, to string) bool {
	switch from {
	case StatePending:
		return to == StateVerifying
	case StateVerifying:
		return to == StateCommitted || to == StateFailed
	case StateFailed:
		return to == StateVerifying
	default:
		return false
	}
}

func allowedFrom(state string) string {
	switch state {
	case StatePending:
		return "verifying"
	case StateVerifying:
		return "committed|failed"
	case StateFailed:
		return "verifying"
	case StateCommitted:
		return "none (terminal)"
	default:
		return "unknown"
	}
}

func cloneResult(r model.SegmentResult) model.SegmentResult {
	out := r
	out.Frames = append([]model.FrameResult(nil), r.Frames...)
	for i := range out.Frames {
		out.Frames[i].Blocks = append([]model.BlockMetric(nil), r.Frames[i].Blocks...)
		out.Frames[i].ChangedBlocks = append([]model.BlockRef(nil), r.Frames[i].ChangedBlocks...)
		out.Frames[i].AbnormalBlocks = append([]model.BlockRef(nil), r.Frames[i].AbnormalBlocks...)
		out.Frames[i].Reasons = append([]string(nil), r.Frames[i].Reasons...)
	}
	return out
}

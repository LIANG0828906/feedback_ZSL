// Package incremental applies frame engineering edits and recomputes only the
// invalidated suffix/closure while reusing cached prefix state.
package incremental

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/engine"
	"multiplexgw/internal/model"
	"multiplexgw/internal/schema"
)

type Closure struct {
	RootSource string   `json:"root_source"`
	Streams    []int    `json:"streams"`
	Nodes      []string `json:"nodes"`
	Edges      []string `json:"edges"`
	EventStart int      `json:"event_start"`
	EventEnd   int      `json:"event_end"`
}

type Report struct {
	Edit               model.Edit       `json:"edit"`
	Closure            Closure          `json:"closure"`
	ReusedEvents       []int            `json:"reused_events"`
	RecomputedEvents   []int            `json:"recomputed_events"`
	SourcesByEvent     map[int][]string `json:"sources_by_event"`
	SemanticEqual      bool             `json:"semantic_equal"`
	Mismatches         []string         `json:"mismatches,omitempty"`
	Actual             *model.RunResult `json:"actual,omitempty"`
	ExpectedEventCount int              `json:"expected_event_count"`
}

type Manager struct {
	Original *model.Project
	Options  engine.Options
	Full     *model.RunResult
	before   [][]byte
}

func NewManager(p *model.Project, opts engine.Options) (*Manager, error) {
	eng, err := engine.New(p, opts)
	if err != nil {
		return nil, err
	}
	m := &Manager{Original: p, Options: opts}
	initial, err := cloneEngine(eng)
	if err != nil {
		return nil, err
	}
	m.before = append(m.before, initial)
	for i := 0; i < len(p.Events); i++ {
		if _, err := eng.Step(); err != nil {
			return nil, err
		}
		state, err := cloneEngine(eng)
		if err != nil {
			return nil, err
		}
		m.before = append(m.before, state)
	}
	m.Full = eng.Result()
	return m, nil
}

func cloneEngine(e *engine.Engine) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, bizerr.Wrap(bizerr.Internal, "incremental.clone", err, "clone engine state")
	}
	return b, nil
}

func restoreEngine(b []byte) (*engine.Engine, error) {
	var e engine.Engine
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, bizerr.Wrap(bizerr.Internal, "incremental.restore", err, "restore engine state")
	}
	return &e, nil
}

// Apply mutates a deep copy, reuses unaffected prefix state, and compares with
// a one-shot full recomputation.
func (m *Manager) Apply(edit model.Edit) (*Report, *model.RunResult, error) {
	edited, err := ApplyEdit(m.Original, edit)
	if err != nil {
		return nil, nil, err
	}
	closure := buildClosure(m.Original, edit)
	start := closure.EventStart

	var eng *engine.Engine
	if start == 0 || start >= len(m.before) {
		eng, err = engine.New(edited, m.Options)
		if err != nil {
			return nil, nil, err
		}
	} else {
		eng, err = restoreEngine(m.before[start])
		if err != nil {
			return nil, nil, err
		}
		eng.Project = edited
		eng.Options = m.Options
	}

	var recomputed []model.Trace
	for {
		trace, stepErr := eng.Step()
		if stepErr != nil {
			return nil, eng.Result(), stepErr
		}
		if trace == nil {
			break
		}
		recomputed = append(recomputed, *trace)
	}
	actual := eng.Result()

	expected, err := engine.Run(edited, m.Options)
	if err != nil {
		return nil, actual, err
	}

	incTraces := []model.Trace{}
	reusedEvents := []int{}
	recomputedEvents := []int{}
	sources := map[int][]string{}

	for i := 0; i < start && i < len(m.Full.Traces); i++ {
		t := m.Full.Traces[i]
		t.CacheStatus = "reused"
		t.Invalidation = []string{"reuse:unaffected-prefix"}
		incTraces = append(incTraces, t)
		reusedEvents = append(reusedEvents, i)
		sources[i] = append([]string(nil), t.Invalidation...)
	}
	rootSource := rootSource(edit)
	for j, t := range recomputed {
		idx := start + j
		t.CacheStatus = "recomputed"
		t.Invalidation = []string{rootSource, "propagate:dependency-closure", "propagate:sequential-window-state"}
		if idx < len(m.Full.Traces) {
			t.Invalidation = append(t.Invalidation, diffSources(&m.Full.Traces[idx], &t)...)
		}
		incTraces = append(incTraces, t)
		recomputedEvents = append(recomputedEvents, idx)
		sources[idx] = append([]string(nil), t.Invalidation...)
	}
	actual.Traces = incTraces

	mismatches := compareTraces(expected.Traces, incTraces)
	report := &Report{
		Edit:               edit,
		Closure:            closure,
		ReusedEvents:       reusedEvents,
		RecomputedEvents:   recomputedEvents,
		SourcesByEvent:     sources,
		SemanticEqual:      len(mismatches) == 0,
		Mismatches:         mismatches,
		Actual:             actual,
		ExpectedEventCount: len(expected.Traces),
	}
	if !report.SemanticEqual {
		return report, actual, bizerr.New(bizerr.IncrementalMismatch, "incremental",
			"%d incremental/full trace mismatches", len(mismatches)).
			WithContext("first", firstMismatch(mismatches))
	}
	return report, actual, nil
}

func ApplyEdit(p *model.Project, edit model.Edit) (*model.Project, error) {
	cp, err := CloneProject(p)
	if err != nil {
		return nil, err
	}
	switch edit.Kind {
	case model.EditWeight:
		if edit.Weight <= 0 || edit.Weight > 10000 {
			return nil, bizerr.New(bizerr.WeightRange, "edit.weight", "weight %d outside [1,10000]", edit.Weight).
				WithContext("stream_id", edit.StreamID)
		}
		s := findStream(cp, edit.StreamID)
		if s == nil {
			return nil, bizerr.New(bizerr.NotFound, "edit.stream_id", "stream %d not found", edit.StreamID)
		}
		s.WeightPermyriad = edit.Weight
	case model.EditParent:
		s := findStream(cp, edit.StreamID)
		if s == nil {
			return nil, bizerr.New(bizerr.NotFound, "edit.stream_id", "stream %d not found", edit.StreamID)
		}
		s.ParentID = edit.ParentID
	case model.EditWindow:
		if edit.WindowBytes < 0 {
			return nil, bizerr.New(bizerr.WindowNegative, "edit.window_bytes", "window is negative").
				WithContext("stream_id", edit.StreamID).WithContext("value", edit.WindowBytes)
		}
		s := findStream(cp, edit.StreamID)
		if s == nil {
			return nil, bizerr.New(bizerr.NotFound, "edit.stream_id", "stream %d not found", edit.StreamID)
		}
		if edit.WindowBytes > s.MaxWindowBytes {
			return nil, bizerr.New(bizerr.WindowOverCap, "edit.window_bytes",
				"window %d exceeds cap %d", edit.WindowBytes, s.MaxWindowBytes)
		}
		s.InitialWindowBytes = edit.WindowBytes
	case model.EditAppendEvent:
		if edit.Event == nil {
			return nil, bizerr.New(bizerr.FieldMapping, "edit.event", "append requires event")
		}
		ev := *edit.Event
		if ev.TimeMS == 0 && ev.FrameSeq == 0 {
			interval := cp.FrameIntervalMS
			if interval <= 0 {
				interval = 10
			}
			ev.FrameSeq = int64(len(cp.Events) + 1)
			ev.TimeMS = ev.FrameSeq * interval
		}
		cp.Events = append(cp.Events, ev)
	case model.EditDeleteEvent:
		if edit.EventIndex < 0 || edit.EventIndex >= len(cp.Events) {
			return nil, bizerr.New(bizerr.NotFound, "edit.event_index", "delete index %d out of range", edit.EventIndex)
		}
		cp.Events = append(cp.Events[:edit.EventIndex], cp.Events[edit.EventIndex+1:]...)
	case model.EditUpdateEvent:
		if edit.EventIndex < 0 || edit.EventIndex >= len(cp.Events) || edit.Event == nil {
			return nil, bizerr.New(bizerr.NotFound, "edit.event_index", "update index %d invalid", edit.EventIndex)
		}
		ev := *edit.Event
		cp.Events[edit.EventIndex] = ev
	default:
		return nil, bizerr.New(bizerr.FieldMapping, "edit.kind", "unknown edit kind %q", edit.Kind)
	}
	normalizeEditedEvents(cp)
	if err := schema.Validate(cp); err != nil {
		return nil, err
	}
	return cp, nil
}

func CloneProject(p *model.Project) (*model.Project, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, bizerr.Wrap(bizerr.Internal, "project.clone", err, "clone project")
	}
	var cp model.Project
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, bizerr.Wrap(bizerr.Internal, "project.clone", err, "clone project")
	}
	return &cp, nil
}

func normalizeEditedEvents(p *model.Project) {
	sort.SliceStable(p.Events, func(i, j int) bool {
		if p.Events[i].TimeMS != p.Events[j].TimeMS {
			return p.Events[i].TimeMS < p.Events[j].TimeMS
		}
		return p.Events[i].FrameSeq < p.Events[j].FrameSeq
	})
	interval := p.FrameIntervalMS
	if interval <= 0 {
		interval = 10
	}
	for i := range p.Events {
		p.Events[i].Index = i
		p.Events[i].FrameSeq = int64(i + 1)
		p.Events[i].TimeMS = int64(i+1) * interval
	}
}

func findStream(p *model.Project, id int) *model.StreamSpec {
	for i := range p.Streams {
		if p.Streams[i].ID == id {
			return &p.Streams[i]
		}
	}
	return nil
}

func rootSource(edit model.Edit) string {
	switch edit.Kind {
	case model.EditWeight:
		return fmt.Sprintf("source:initial-weight:stream-%d", edit.StreamID)
	case model.EditParent:
		return fmt.Sprintf("source:dependency-parent:stream-%d", edit.StreamID)
	case model.EditWindow:
		return fmt.Sprintf("source:initial-window:stream-%d", edit.StreamID)
	case model.EditAppendEvent:
		return "source:event-append"
	case model.EditDeleteEvent:
		return fmt.Sprintf("source:event-delete:index-%d", edit.EventIndex)
	case model.EditUpdateEvent:
		return fmt.Sprintf("source:event-update:index-%d", edit.EventIndex)
	default:
		return "source:unknown-edit"
	}
}

func affectedStart(edit model.Edit) int {
	switch edit.Kind {
	case model.EditAppendEvent:
		return edit.EventIndex
	case model.EditDeleteEvent, model.EditUpdateEvent:
		return edit.EventIndex
	default:
		return 0
	}
}

func buildClosure(p *model.Project, edit model.Edit) Closure {
	start := affectedStart(edit)
	c := Closure{RootSource: rootSource(edit), EventStart: start, EventEnd: len(p.Events) - 1}
	if edit.Kind == model.EditAppendEvent {
		c.EventEnd = len(p.Events)
	}
	ids := map[int]bool{}
	add := func(id int) {
		if id > 0 {
			ids[id] = true
		}
	}
	switch edit.Kind {
	case model.EditWeight, model.EditWindow:
		add(edit.StreamID)
		for _, id := range dependencyClosure(p, edit.StreamID) {
			add(id)
		}
	case model.EditParent:
		add(edit.StreamID)
		for _, id := range dependencyClosure(p, edit.StreamID) {
			add(id)
		}
		for _, s := range p.Streams {
			if s.ParentID == edit.ParentID || s.ParentID == parentOf(p, edit.StreamID) {
				add(s.ID)
			}
		}
	case model.EditDeleteEvent, model.EditUpdateEvent, model.EditAppendEvent:
		if edit.Event != nil && edit.Event.StreamID > 0 {
			add(edit.Event.StreamID)
			for _, id := range dependencyClosure(p, edit.Event.StreamID) {
				add(id)
			}
		} else if edit.EventIndex >= 0 && edit.EventIndex < len(p.Events) {
			sid := p.Events[edit.EventIndex].StreamID
			add(sid)
			for _, id := range dependencyClosure(p, sid) {
				add(id)
			}
		}
	}
	for id := range ids {
		c.Streams = append(c.Streams, id)
		c.Nodes = append(c.Nodes, fmt.Sprintf("stream:%d", id))
	}
	sort.Ints(c.Streams)
	c.Nodes = append(c.Nodes, "connection:0", "scheduler:wrr", "window:connection")
	c.Edges = append(c.Edges,
		fmt.Sprintf("%s->scheduler:wrr", c.RootSource),
		"scheduler:wrr->window:connection",
		"window:connection->all-active-streams",
	)
	for _, id := range c.Streams {
		c.Edges = append(c.Edges, fmt.Sprintf("dependency-tree->stream:%d", id))
	}
	return c
}

func parentOf(p *model.Project, id int) int {
	s := findStream(p, id)
	if s == nil {
		return 0
	}
	return s.ParentID
}

// dependencyClosure returns ancestors and descendants of id.
func dependencyClosure(p *model.Project, id int) []int {
	set := map[int]bool{}
	parent := parentOf(p, id)
	for parent > 0 {
		if set[parent] {
			break
		}
		set[parent] = true
		parent = parentOf(p, parent)
	}
	var collectChildren func(parent int)
	collectChildren = func(parent int) {
		for _, s := range p.Streams {
			if s.ParentID == parent && !set[s.ID] {
				set[s.ID] = true
				collectChildren(s.ID)
			}
		}
	}
	set[id] = true
	collectChildren(id)
	out := make([]int, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func diffSources(old, now *model.Trace) []string {
	var src []string
	if old.ConnectionWindow != now.ConnectionWindow {
		src = append(src, "propagate:connection-window")
	}
	oldW := map[int]int64{}
	for _, s := range old.Streams {
		oldW[s.StreamID] = s.WindowRemaining
	}
	for _, s := range now.Streams {
		if w, ok := oldW[s.StreamID]; ok && w != s.WindowRemaining {
			src = append(src, fmt.Sprintf("propagate:stream-window:%d", s.StreamID))
		}
	}
	if !reflect.DeepEqual(old.Sends, now.Sends) {
		src = append(src, "propagate:scheduler-selection")
	}
	if !reflect.DeepEqual(old.Blocked, now.Blocked) {
		src = append(src, "propagate:blocking-state")
	}
	return unique(src)
}

func unique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func compareTraces(expected, actual []model.Trace) []string {
	if len(expected) != len(actual) {
		return []string{fmt.Sprintf("trace length expected=%d actual=%d", len(expected), len(actual))}
	}
	var mismatches []string
	for i := range expected {
		a := expected[i]
		b := actual[i]
		a.CacheStatus = ""
		a.Invalidation = nil
		b.CacheStatus = ""
		b.Invalidation = nil
		if !reflect.DeepEqual(a, b) {
			mismatches = append(mismatches, fmt.Sprintf("event %d semantic trace differs", i))
		}
	}
	return mismatches
}

func firstMismatch(in []string) string {
	if len(in) == 0 {
		return ""
	}
	return in[0]
}

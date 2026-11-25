// Package engine implements deterministic logical-time flow control and
// hierarchical weighted round-robin scheduling.
package engine

import (
	"sort"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/model"
)

const DefaultMaxPendingFrames = 8

type Options struct {
	MaxPendingFrames int  `json:"max_pending_frames"`
	RecordFaults     bool `json:"record_faults"`
}

func (o Options) withDefaults() Options {
	if o.MaxPendingFrames <= 0 {
		o.MaxPendingFrames = DefaultMaxPendingFrames
	}
	return o
}

type PendingFrame struct {
	EventIndex   int   `json:"event_index"`
	SegmentIndex int   `json:"segment_index"`
	Length       int64 `json:"length"`
	Remaining    int64 `json:"remaining"`
	Fin          bool  `json:"fin"`
}

type StreamRuntime struct {
	ID                int              `json:"id"`
	Name              string           `json:"name,omitempty"`
	ParentID          int              `json:"parent_id"`
	EffectiveParentID int              `json:"effective_parent_id"`
	WeightPermyriad   int64            `json:"weight_permyriad"`
	InitialWindow     int64            `json:"initial_window"`
	Window            int64            `json:"window"`
	MaxWindow         int64            `json:"max_window"`
	MaxFrame          int64            `json:"max_frame"`
	BodySegments      []int64          `json:"body_segments"`
	Pending           []PendingFrame   `json:"pending"`
	Open              bool             `json:"open"`
	Reset             bool             `json:"reset"`
	Closed            bool             `json:"closed"`
	FaultCode         string           `json:"fault_code,omitempty"`
	State             string           `json:"state"`
	SentBytes         int64            `json:"sent_bytes"`
	SegmentsSent      int              `json:"segments_sent"`
	SegmentsEnqueued  int              `json:"segments_enqueued"`
	ReleasedCredit    int64            `json:"released_credit"`
	ReleasedBuffer    int64            `json:"released_buffer"`
	RemovedAtMS       int64            `json:"removed_at_ms,omitempty"`
	CurrentWeight     int64            `json:"current_weight"`
	ReasonSince       map[string]int64 `json:"reason_since,omitempty"`
	OrphanPolicy      string           `json:"orphan_policy,omitempty"`
}

type Engine struct {
	Project          *model.Project         `json:"project"`
	Options          Options                `json:"options"`
	ConnectionWindow int64                  `json:"connection_window"`
	CommittedThrough int                    `json:"committed_through"`
	Streams          map[int]*StreamRuntime `json:"streams"`
	Order            []int                  `json:"order"`
	Traces           []model.Trace          `json:"traces"`
	Sends            []model.SendRecord     `json:"sends"`
}

func New(p *model.Project, opts Options) (*Engine, error) {
	if p == nil {
		return nil, bizerr.New(bizerr.Internal, "engine", "nil project")
	}
	if err := validateRuntimeBudget(opts.withDefaults()); err != nil {
		return nil, err
	}
	e := &Engine{
		Project:          p,
		Options:          opts.withDefaults(),
		ConnectionWindow: p.Connection.InitialWindowBytes,
		CommittedThrough: -1,
		Streams:          map[int]*StreamRuntime{},
	}
	for _, s := range p.Streams {
		policy := s.OrphanPolicy
		if policy == "" {
			policy = p.OrphanPolicy
		}
		e.Streams[s.ID] = &StreamRuntime{
			ID:                s.ID,
			Name:              s.Name,
			ParentID:          s.ParentID,
			EffectiveParentID: s.ParentID,
			WeightPermyriad:   s.WeightPermyriad,
			InitialWindow:     s.InitialWindowBytes,
			Window:            s.InitialWindowBytes,
			MaxWindow:         s.MaxWindowBytes,
			MaxFrame:          s.MaxFrameBytes,
			BodySegments:      append([]int64(nil), s.BodySegments...),
			State:             model.StateIdle,
			ReasonSince:       map[string]int64{},
			OrphanPolicy:      policy,
		}
		e.Order = append(e.Order, s.ID)
	}
	sort.Ints(e.Order)
	return e, nil
}

func validateRuntimeBudget(opts Options) error {
	if opts.MaxPendingFrames < 1 || opts.MaxPendingFrames > 4096 {
		return bizerr.New(bizerr.BudgetRange, "options.max_pending_frames",
			"per-stream pending frame budget %d outside [1,4096]", opts.MaxPendingFrames).
			WithContext("min", 1).WithContext("max", 4096)
	}
	return nil
}

// Run processes all events from the initial state.
func Run(p *model.Project, opts Options) (*model.RunResult, error) {
	e, err := New(p, opts)
	if err != nil {
		return nil, err
	}
	for {
		trace, err := e.Step()
		if err != nil {
			res := e.Result()
			return res, err
		}
		if trace == nil {
			break
		}
	}
	return e.Result(), nil
}

// Step processes exactly the next uncommitted event. It returns nil at end.
func (e *Engine) Step() (*model.Trace, error) {
	next := e.CommittedThrough + 1
	if next >= len(e.Project.Events) {
		return nil, nil
	}
	ev := e.Project.Events[next]
	if ev.Index != next {
		// Loaders normalize indexes; this guards restored/edited projects.
		ev.Index = next
	}
	for _, st := range e.Streams {
		st.FaultCode = ""
	}

	trace := model.Trace{
		EventIndex:       next,
		FrameSeq:         ev.FrameSeq,
		LogicalMS:        ev.TimeMS,
		EventType:        ev.Type,
		SourceStreamID:   ev.StreamID,
		ConnectionWindow: e.ConnectionWindow,
		ConnectionCap:    e.Project.Connection.MaxWindowBytes,
	}

	var stepErr error
	switch ev.Type {
	case model.EventHeaders:
		stepErr = e.applyHeaders(&ev)
	case model.EventData:
		stepErr = e.applyData(&ev, &trace)
	case model.EventWindowUpdate:
		stepErr = e.applyWindowUpdate(&ev)
	case model.EventRSTStream:
		stepErr = e.applyReset(&ev, &trace)
	case model.EventPriority:
		stepErr = e.applyPriority(&ev)
	default:
		stepErr = bizerr.New(bizerr.FieldMapping, "event.type", "unsupported event type %q", ev.Type)
	}
	if stepErr != nil {
		if be, ok := bizerr.AsError(stepErr); ok {
			trace.Error = &model.ErrorReport{Code: be.Code, Path: be.Path, Message: be.Message, Context: be.Context}
		}
		trace.ConnectionWindow = e.ConnectionWindow
		trace.Streams = e.snapshots(ev.TimeMS)
		trace.Blocked = blockedEntries(trace.Streams)
		e.Traces = append(e.Traces, trace)
		return &trace, stepErr
	}

	// Data and WINDOW_UPDATE can open credit; advance as far as deterministic
	// windows and pending buffers allow.
	e.drainSchedules(&trace)

	e.CommittedThrough = next
	trace.ConnectionWindow = e.ConnectionWindow
	trace.Streams = e.snapshots(ev.TimeMS)
	trace.Blocked = blockedEntries(trace.Streams)
	e.Traces = append(e.Traces, trace)
	return &trace, nil
}

func (e *Engine) Result() *model.RunResult {
	streams := e.snapshots(e.lastTime())
	return &model.RunResult{
		ProjectID:        e.Project.ProjectID,
		SchemaVersion:    e.Project.SchemaVersion,
		CommittedThrough: e.CommittedThrough,
		Traces:           append([]model.Trace(nil), e.Traces...),
		Sends:            append([]model.SendRecord(nil), e.Sends...),
		FinalStreams:     streams,
	}
}

func (e *Engine) lastTime() int64 {
	if len(e.Project.Events) == 0 {
		return 0
	}
	idx := e.CommittedThrough
	if idx < 0 {
		idx = 0
	}
	if idx >= len(e.Project.Events) {
		idx = len(e.Project.Events) - 1
	}
	return e.Project.Events[idx].TimeMS
}

func (e *Engine) stream(id int) (*StreamRuntime, error) {
	st, ok := e.Streams[id]
	if !ok {
		return nil, bizerr.New(bizerr.NotFound, "event.stream_id", "stream %d not found", id).WithContext("stream_id", id)
	}
	return st, nil
}

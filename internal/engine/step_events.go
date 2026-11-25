package engine

import (
	"sort"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/model"
)

func (e *Engine) applyHeaders(ev *model.Event) error {
	st, err := e.stream(ev.StreamID)
	if err != nil {
		return err
	}
	if st.Reset {
		return bizerr.New(bizerr.SchemaConflict, "event.headers", "HEADERS on reset stream %d", ev.StreamID).
			WithContext("stream_id", ev.StreamID)
	}
	st.Open = true
	if st.State == model.StateIdle {
		st.State = model.StateOpen
	}
	return nil
}

func (e *Engine) applyData(ev *model.Event, trace *model.Trace) error {
	st, err := e.stream(ev.StreamID)
	if err != nil {
		return err
	}
	if st.Reset || st.Closed {
		return bizerr.New(bizerr.SchemaConflict, "event.data", "DATA on closed/reset stream %d", ev.StreamID).
			WithContext("stream_id", ev.StreamID).WithContext("reset", st.Reset).WithContext("closed", st.Closed)
	}
	if ev.LengthBytes > st.MaxFrame {
		be := bizerr.New(bizerr.FrameTooLarge, "event.data.length_bytes",
			"DATA length %d exceeds max frame %d", ev.LengthBytes, st.MaxFrame).
			WithContext("stream_id", ev.StreamID).
			WithContext("event_index", ev.Index).
			WithContext("length", ev.LengthBytes).
			WithContext("max_frame", st.MaxFrame)
		if e.Options.RecordFaults {
			st.FaultCode = bizerr.FrameTooLarge
			trace.Error = &model.ErrorReport{
				Code: be.Code, Path: be.Path, Message: be.Message, Context: be.Context,
			}
			return nil
		}
		return be
	}
	if len(st.Pending) >= e.Options.MaxPendingFrames {
		be := bizerr.New(bizerr.BufferFull, "stream.pending_frames",
			"stream %d pending buffer budget %d exhausted", ev.StreamID, e.Options.MaxPendingFrames).
			WithContext("stream_id", ev.StreamID).
			WithContext("event_index", ev.Index).
			WithContext("budget", e.Options.MaxPendingFrames).
			WithContext("pending", len(st.Pending))
		trace.BackpressureCode = be.Code
		trace.BackpressureContext = be.Context
		return be
	}
	st.Open = true
	st.Pending = append(st.Pending, PendingFrame{
		EventIndex:   ev.Index,
		SegmentIndex: ev.SegmentIndex,
		Length:       ev.LengthBytes,
		Remaining:    ev.LengthBytes,
		Fin:          ev.Fin,
	})
	st.SegmentsEnqueued++
	return nil
}

func (e *Engine) applyWindowUpdate(ev *model.Event) error {
	if ev.IncrementBytes < 0 {
		return bizerr.New(bizerr.WindowNegative, "event.window_update.increment_bytes",
			"WINDOW_UPDATE increment %d is negative", ev.IncrementBytes).
			WithContext("stream_id", ev.StreamID).WithContext("event_index", ev.Index)
	}
	if ev.StreamID == 0 {
		current := e.ConnectionWindow
		cap := e.Project.Connection.MaxWindowBytes
		next, ok := addChecked(current, ev.IncrementBytes)
		if !ok {
			return bizerr.New(bizerr.FixedOverflow, "connection.window", "window addition overflow").
				WithContext("current", current).WithContext("increment", ev.IncrementBytes)
		}
		if next > cap {
			return bizerr.New(bizerr.WindowOverCap, "connection.window_update",
				"connection window %d + %d exceeds cap %d", current, ev.IncrementBytes, cap).
				WithContext("stream_id", 0).
				WithContext("current", current).
				WithContext("increment", ev.IncrementBytes).
				WithContext("cap", cap)
		}
		e.ConnectionWindow = next
		return nil
	}
	st, err := e.stream(ev.StreamID)
	if err != nil {
		return err
	}
	next, ok := addChecked(st.Window, ev.IncrementBytes)
	if !ok {
		return bizerr.New(bizerr.FixedOverflow, "stream.window", "window addition overflow").
			WithContext("stream_id", ev.StreamID)
	}
	if next > st.MaxWindow {
		return bizerr.New(bizerr.WindowOverCap, "stream.window_update",
			"stream %d window %d + %d exceeds cap %d", st.ID, st.Window, ev.IncrementBytes, st.MaxWindow).
			WithContext("stream_id", st.ID).
			WithContext("current", st.Window).
			WithContext("increment", ev.IncrementBytes).
			WithContext("cap", st.MaxWindow)
	}
	st.Window = next
	return nil
}

func (e *Engine) applyPriority(ev *model.Event) error {
	st, err := e.stream(ev.StreamID)
	if err != nil {
		return err
	}
	if ev.WeightPermyriad <= 0 || ev.WeightPermyriad > 10000 {
		return bizerr.New(bizerr.WeightRange, "event.priority.weight",
			"weight %d outside [1,10000]", ev.WeightPermyriad).
			WithContext("stream_id", ev.StreamID).WithContext("event_index", ev.Index)
	}
	st.WeightPermyriad = ev.WeightPermyriad
	return nil
}

func (e *Engine) applyReset(ev *model.Event, trace *model.Trace) error {
	st, err := e.stream(ev.StreamID)
	if err != nil {
		return err
	}
	if st.Reset {
		return nil
	}
	st.Reset = true
	st.Open = false
	st.State = model.StateReset
	st.RemovedAtMS = ev.TimeMS

	releasedWindow := st.Window
	releasedBuffer := int64(0)
	for _, f := range st.Pending {
		releasedBuffer += f.Remaining
	}
	st.ReleasedCredit += releasedWindow
	st.ReleasedBuffer += releasedBuffer
	st.Window = 0
	st.Pending = nil
	st.CurrentWeight = 0
	trace.RemovedStreamID = st.ID
	trace.RemovedAtMS = ev.TimeMS
	trace.ReleasedCredit = releasedWindow
	trace.ReleasedBufferBytes = releasedBuffer

	e.orphanChildren(st)
	return nil
}

func (e *Engine) orphanChildren(parent *StreamRuntime) {
	var children []*StreamRuntime
	for _, id := range e.Order {
		child := e.Streams[id]
		if child.EffectiveParentID == parent.ID && !child.Reset {
			children = append(children, child)
		}
	}
	heirID := 0
	if parent.OrphanPolicy == model.OrphanInherit && len(children) > 0 {
		heirID = children[0].ID // Order is sorted, so this is the lowest ID.
	}
	for _, child := range children {
		child.EffectiveParentID = parent.EffectiveParentID
		if child.ID == heirID {
			child.WeightPermyriad += parent.WeightPermyriad
			if child.WeightPermyriad > 10000 {
				child.WeightPermyriad = 10000
			}
		}
	}
}

func (e *Engine) drainSchedules(trace *model.Trace) {
	guard := 0
	for {
		guard++
		if guard > 1_000_000 {
			be := bizerr.New(bizerr.Internal, "engine.schedule", "scheduling drain guard exceeded")
			trace.Error = &model.ErrorReport{Code: be.Code, Message: be.Message, Path: be.Path}
			return
		}
		id := e.selectStream()
		if id == 0 {
			return
		}
		st := e.Streams[id]
		n := e.sendable(st)
		if n <= 0 || len(st.Pending) == 0 {
			return
		}
		frame := &st.Pending[0]
		if n > frame.Remaining {
			n = frame.Remaining
		}
		e.ConnectionWindow -= n
		st.Window -= n
		st.SentBytes += n
		frame.Remaining -= n

		rec := model.SendRecord{
			EventIndex:       e.CommittedThrough + 1,
			LogicalMS:        trace.LogicalMS,
			FrameSeq:         trace.FrameSeq,
			StreamID:         st.ID,
			WeightPermyriad:  st.WeightPermyriad,
			AllocatedWeight:  st.WeightPermyriad,
			BytesSent:        n,
			ConnectionWindow: e.ConnectionWindow,
			StreamWindow:     st.Window,
			SegmentIndex:     frame.SegmentIndex,
		}
		e.Sends = append(e.Sends, rec)
		trace.Sends = append(trace.Sends, rec)

		if frame.Remaining == 0 {
			fin := frame.Fin
			st.Pending = st.Pending[1:]
			st.SegmentsSent++
			if fin && len(st.Pending) == 0 {
				st.Closed = true
				st.Open = false
				released := st.Window
				st.ReleasedCredit += released
				st.Window = 0
				e.orphanChildren(st)
			}
		}
	}
}

func (e *Engine) selectStream() int {
	return e.selectAt(0)
}

func (e *Engine) selectAt(parentID int) int {
	type candidate struct{ st *StreamRuntime }
	var active []*StreamRuntime
	for _, id := range e.Order {
		st := e.Streams[id]
		if st.EffectiveParentID == parentID && e.subtreeRunnable(st.ID) {
			active = append(active, st)
		}
	}
	if len(active) == 0 {
		return 0
	}
	total := int64(0)
	for _, st := range active {
		total += st.WeightPermyriad
		st.CurrentWeight += st.WeightPermyriad
	}
	chosen := active[0]
	for _, st := range active[1:] {
		if st.CurrentWeight > chosen.CurrentWeight ||
			(st.CurrentWeight == chosen.CurrentWeight && st.ID < chosen.ID) {
			chosen = st
		}
	}
	chosen.CurrentWeight -= total
	if e.directlyEligible(chosen) {
		return chosen.ID
	}
	return e.selectAt(chosen.ID)
}

func (e *Engine) subtreeRunnable(id int) bool {
	st := e.Streams[id]
	if e.directlyEligible(st) {
		return true
	}
	for _, childID := range e.Order {
		child := e.Streams[childID]
		if child.EffectiveParentID == id && e.subtreeRunnable(childID) {
			return true
		}
	}
	return false
}

func (e *Engine) directlyEligible(st *StreamRuntime) bool {
	if st == nil || !st.Open || st.Reset || st.Closed || st.FaultCode != "" {
		return false
	}
	if len(st.Pending) == 0 || st.Pending[0].Remaining <= 0 {
		return false
	}
	return e.sendable(st) > 0
}

func (e *Engine) sendable(st *StreamRuntime) int64 {
	if e.ConnectionWindow <= 0 || st.Window <= 0 || st.MaxFrame <= 0 || len(st.Pending) == 0 {
		return 0
	}
	head := st.Pending[0]
	return minInt64(e.ConnectionWindow, st.Window, st.MaxFrame, head.Remaining)
}

func (e *Engine) snapshots(nowMS int64) []model.StreamSnapshot {
	out := make([]model.StreamSnapshot, 0, len(e.Order))
	for _, id := range e.Order {
		st := e.Streams[id]
		if st.ReasonSince == nil {
			st.ReasonSince = map[string]int64{}
		}
		reasons := e.currentReasons(st)
		active := map[string]bool{}
		for _, r := range reasons {
			active[r] = true
			if _, ok := st.ReasonSince[r]; !ok {
				st.ReasonSince[r] = nowMS
			}
		}
		for r := range st.ReasonSince {
			if !active[r] {
				delete(st.ReasonSince, r)
			}
		}
		var since *int64
		reasonSince := map[string]int64{}
		for r, t := range st.ReasonSince {
			reasonSince[r] = t
			if since == nil || t < *since {
				v := t
				since = &v
			}
		}
		if len(reasonSince) == 0 {
			reasonSince = nil
		}
		state := model.StateOpen
		switch {
		case st.Reset:
			state = model.StateReset
		case st.Closed:
			state = model.StateClosed
		case containsReason(reasons, model.ReasonBodyExhausted):
			state = model.StateDrained
		case len(reasons) > 0:
			state = model.StateBlocked
		case !st.Open:
			state = model.StateIdle
		}
		st.State = state
		pendingBytes := int64(0)
		for _, f := range st.Pending {
			pendingBytes += f.Remaining
		}
		out = append(out, model.StreamSnapshot{
			StreamID:            st.ID,
			Name:                st.Name,
			ParentID:            st.ParentID,
			EffectiveParentID:   st.EffectiveParentID,
			WeightPermyriad:     st.WeightPermyriad,
			WindowRemaining:     st.Window,
			SentBytes:           st.SentBytes,
			SendableBytes:       e.sendable(st),
			PendingFrames:       len(st.Pending),
			PendingBytes:        pendingBytes,
			State:               state,
			Reasons:             reasons,
			BlockedSinceMS:      since,
			ReasonSinceMS:       reasonSince,
			SegmentsTotal:       len(st.BodySegments),
			SegmentsSent:        st.SegmentsSent,
			ReleasedCredit:      st.ReleasedCredit,
			ReleasedBufferBytes: st.ReleasedBuffer,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StreamID < out[j].StreamID })
	return out
}

func (e *Engine) currentReasons(st *StreamRuntime) []string {
	if st.Reset {
		return []string{model.ReasonReset}
	}
	if st.Closed {
		return []string{model.ReasonClosed}
	}
	var reasons []string
	if st.FaultCode != "" {
		reasons = append(reasons, st.FaultCode)
	}
	bodyDone := len(st.BodySegments) > 0 && st.SegmentsSent >= len(st.BodySegments) && len(st.Pending) == 0
	if bodyDone {
		reasons = append(reasons, model.ReasonBodyExhausted)
	}
	hasDemand := st.Open && len(st.Pending) > 0 && !bodyDone
	if hasDemand {
		if e.ConnectionWindow <= 0 {
			reasons = append(reasons, model.ReasonConnectionWindow)
		}
		if st.Window <= 0 {
			reasons = append(reasons, model.ReasonStreamWindow)
		}
	}
	return reasons
}

// CurrentReasonsForCheckpoint exposes stable reason codes to the checkpoint
// package without making the classification part of the scheduling API.
func (e *Engine) CurrentReasonsForCheckpoint(st *StreamRuntime) []string {
	return append([]string(nil), e.currentReasons(st)...)
}

func blockedEntries(snaps []model.StreamSnapshot) []model.BlockedEntry {
	var out []model.BlockedEntry
	for _, s := range snaps {
		if len(s.Reasons) == 0 {
			continue
		}
		active := make([]string, 0, len(s.Reasons))
		for _, reason := range s.Reasons {
			if reason == model.ReasonReset || reason == model.ReasonClosed {
				continue
			}
			active = append(active, reason)
		}
		if len(active) == 0 {
			continue
		}
		since := int64(0)
		if s.BlockedSinceMS != nil {
			since = *s.BlockedSinceMS
		}
		out = append(out, model.BlockedEntry{StreamID: s.StreamID, Reasons: active, SinceMS: since})
	}
	return out
}

func addChecked(a, b int64) (int64, bool) {
	if b > 0 && a > (1<<62)-b {
		return 0, false
	}
	return a + b, true
}

func minInt64(values ...int64) int64 {
	m := values[0]
	for _, v := range values[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func containsReason(reasons []string, target string) bool {
	for _, r := range reasons {
		if r == target {
			return true
		}
	}
	return false
}

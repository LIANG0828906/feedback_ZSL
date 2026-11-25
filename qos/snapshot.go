package qos

import "sort"

type ReadySnapshot struct {
	PacketID   string `json:"packet_id"`
	ReadyAtMS  int64  `json:"ready_at_ms"`
	ReplaySeq  int64  `json:"replay_seq,omitempty"`
	DeadlineMS int64  `json:"deadline_ms,omitempty"`
	DelayCause string `json:"delay_cause,omitempty"`
}

type DelaySnapshot struct {
	PacketID   string `json:"packet_id"`
	ExpireAtMS int64  `json:"expire_at_ms"`
	DeadlineMS int64  `json:"deadline_ms,omitempty"`
	ReplaySeq  int64  `json:"replay_seq,omitempty"`
	Reason     string `json:"reason"`
}

type ChannelSnapshot struct {
	ChannelID   string          `json:"channel_id"`
	TokensBytes int64           `json:"tokens_bytes"`
	Remainder   int64           `json:"refill_remainder"`
	Credit      int64           `json:"weight_credit"`
	Ready       []ReadySnapshot `json:"ready"`
}

type EngineSnapshot struct {
	NextStep      int               `json:"next_step"`
	NextMS        int64             `json:"next_ms"`
	ReplayCounter int64             `json:"replay_counter"`
	EmitCounter   int64             `json:"emit_counter"`
	Channels      []ChannelSnapshot `json:"channels"`
	Delays        []DelaySnapshot   `json:"delays"`
	Emitted       []Emission        `json:"emitted"`
	Dropped       []DropEvent       `json:"dropped"`
	DelayedEvents []DelayEvent      `json:"delay_events"`
	Expired       []ExpiredEvent    `json:"expired"`
}

func (e *Engine) Snapshot() *EngineSnapshot {
	if e == nil {
		return nil
	}
	s := &EngineSnapshot{
		NextStep: e.nextStep, NextMS: e.nextMS,
		ReplayCounter: e.replayCounter, EmitCounter: e.emitCounter,
		Emitted: cloneEmissions(e.emitted), Dropped: cloneDrops(e.dropped),
		DelayedEvents: append([]DelayEvent(nil), e.delayedEvents...),
		Expired:       append([]ExpiredEvent(nil), e.expired...),
	}
	for _, cr := range e.channels {
		cs := ChannelSnapshot{
			ChannelID: cr.ch.ID, TokensBytes: cr.tokens,
			Remainder: cr.remainder, Credit: cr.credit,
			Ready: make([]ReadySnapshot, 0, len(cr.ready)),
		}
		for _, rt := range cr.ready {
			cs.Ready = append(cs.Ready, ReadySnapshot{
				PacketID: rt.p.ID, ReadyAtMS: rt.readyAtMS,
				ReplaySeq: rt.replaySeq, DeadlineMS: rt.deadlineMS,
			})
		}
		s.Channels = append(s.Channels, cs)
	}
	for _, rt := range e.packets {
		if rt.status != statusDelayed {
			continue
		}
		s.Delays = append(s.Delays, DelaySnapshot{
			PacketID: rt.p.ID, ExpireAtMS: rt.readyAtMS,
			DeadlineMS: rt.deadlineMS, ReplaySeq: rt.replaySeq,
			Reason: rt.delayCause,
		})
	}
	sort.SliceStable(s.Delays, func(i, j int) bool {
		if s.Delays[i].ExpireAtMS != s.Delays[j].ExpireAtMS {
			return s.Delays[i].ExpireAtMS < s.Delays[j].ExpireAtMS
		}
		return s.Delays[i].PacketID < s.Delays[j].PacketID
	})
	return s
}

func RestoreSnapshot(p *Project, snap *EngineSnapshot) (*Engine, error) {
	if snap == nil {
		return nil, NewError(ECheckpointCorrupt, "checkpoint.snapshot", "snapshot is nil", nil)
	}
	e, err := NewEngine(p)
	if err != nil {
		return nil, err
	}
	if snap.NextStep < 0 || snap.NextMS < 0 || snap.NextMS != int64(snap.NextStep)*p.TickMS {
		return nil, NewError(ECheckpointCorrupt, "checkpoint.snapshot", "snapshot step/time pair is invalid",
			map[string]any{"next_step": snap.NextStep, "next_ms": snap.NextMS, "tick_ms": p.TickMS})
	}
	e.nextStep = snap.NextStep
	e.nextMS = snap.NextMS
	e.replayCounter = snap.ReplayCounter
	e.emitCounter = snap.EmitCounter
	e.emitted = cloneEmissions(snap.Emitted)
	e.dropped = cloneDrops(snap.Dropped)
	e.delayedEvents = append([]DelayEvent(nil), snap.DelayedEvents...)
	e.expired = append([]ExpiredEvent(nil), snap.Expired...)

	final := make(map[string]bool, len(e.emitted)+len(e.dropped))
	for _, em := range e.emitted {
		rt, ok := e.packets[em.PacketID]
		if !ok {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.emitted", "unknown emitted packet", map[string]any{"packet": em.PacketID})
		}
		if rt.status != statusPending || final[em.PacketID] {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.emitted", "packet has conflicting state", map[string]any{"packet": em.PacketID})
		}
		rt.status = statusEmitted
		final[em.PacketID] = true
	}
	for _, dr := range e.dropped {
		rt, ok := e.packets[dr.PacketID]
		if !ok {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.dropped", "unknown dropped packet", map[string]any{"packet": dr.PacketID})
		}
		if rt.status != statusPending || final[dr.PacketID] {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.dropped", "packet has conflicting state", map[string]any{"packet": dr.PacketID})
		}
		rt.status = statusDropped
		final[dr.PacketID] = true
	}

	for _, cs := range snap.Channels {
		cr, ok := e.chByID[cs.ChannelID]
		if !ok {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.channels", "unknown channel", map[string]any{"channel": cs.ChannelID})
		}
		cr.tokens = cs.TokensBytes
		if cr.tokens > cr.ch.CapacityBytes {
			cr.tokens = cr.ch.CapacityBytes
		}
		if cr.tokens < 0 {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.channels", "negative token balance", map[string]any{"channel": cs.ChannelID, "tokens": cr.tokens})
		}
		if cs.Remainder < 0 || cs.Remainder >= TokenScale {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.channels", "refill remainder out of range", map[string]any{"channel": cs.ChannelID, "remainder": cs.Remainder})
		}
		cr.remainder = cs.Remainder
		cr.credit = cs.Credit
		for _, rs := range cs.Ready {
			rt, ok := e.packets[rs.PacketID]
			if !ok {
				return nil, NewError(ECheckpointCorrupt, "checkpoint.ready", "unknown ready packet", map[string]any{"packet": rs.PacketID})
			}
			if rt.p.ChannelID != cs.ChannelID || rt.status != statusPending {
				return nil, NewError(ECheckpointCorrupt, "checkpoint.ready", "ready queue packet is duplicated or on the wrong channel",
					map[string]any{"packet": rs.PacketID, "channel": cs.ChannelID})
			}
			rt.status = statusReady
			rt.readyAtMS = rs.ReadyAtMS
			rt.replaySeq = rs.ReplaySeq
			rt.deadlineMS = rs.DeadlineMS
			cr.ready = append(cr.ready, rt)
			cr.outstanding++
		}
		sortChannelReady(cr.ready)
	}
	for _, ds := range snap.Delays {
		rt, ok := e.packets[ds.PacketID]
		if !ok {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.delays", "unknown delayed packet", map[string]any{"packet": ds.PacketID})
		}
		if rt.status != statusPending {
			return nil, NewError(EDelayQueue, "checkpoint.delays", "packet appears more than once or is already final",
				map[string]any{"packet": ds.PacketID})
		}
		if ds.ExpireAtMS < e.nextMS {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.delays", "delay expiry is before the committed clock",
				map[string]any{"packet": ds.PacketID, "expire_at_ms": ds.ExpireAtMS, "next_ms": e.nextMS})
		}
		if ds.DeadlineMS > 0 && ds.ExpireAtMS > ds.DeadlineMS {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.delays", "delay expiry exceeds its deadline",
				map[string]any{"packet": ds.PacketID, "expire_at_ms": ds.ExpireAtMS, "deadline_ms": ds.DeadlineMS})
		}
		rt.status = statusDelayed
		rt.readyAtMS = ds.ExpireAtMS
		rt.deadlineMS = ds.DeadlineMS
		rt.replaySeq = ds.ReplaySeq
		rt.delayCause = ds.Reason
		e.chByID[rt.p.ChannelID].outstanding++
	}
	for ms := range e.arrivals {
		if ms < e.nextMS {
			delete(e.arrivals, ms)
		}
	}
	for _, rt := range e.packets {
		if rt.status == statusPending && rt.p.ArrivalMS < e.nextMS {
			return nil, NewError(ECheckpointCorrupt, "checkpoint", "past packet has no terminal or queued state",
				map[string]any{"packet": rt.p.ID, "arrival_ms": rt.p.ArrivalMS, "next_ms": e.nextMS})
		}
	}
	e.unresolved = len(e.packets) - len(final)
	if err := e.validateQueues(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) validateQueues() error {
	seen := make(map[string]string, len(e.packets))
	for _, cr := range e.channels {
		for _, rt := range cr.ready {
			if where, ok := seen[rt.p.ID]; ok {
				return NewError(EDelayQueue, "checkpoint.queues", "packet appears more than once", map[string]any{"packet": rt.p.ID, "first": where, "second": "channel:" + cr.ch.ID})
			}
			seen[rt.p.ID] = "channel:" + cr.ch.ID
		}
	}
	for _, rt := range e.packets {
		if rt.status != statusDelayed {
			continue
		}
		if where, ok := seen[rt.p.ID]; ok {
			return NewError(EDelayQueue, "checkpoint.queues", "packet appears more than once", map[string]any{"packet": rt.p.ID, "first": where, "second": "delay_queue"})
		}
		seen[rt.p.ID] = "delay_queue"
	}
	return nil
}

func (e *Engine) StepUntil(target int) error {
	if target < e.nextStep {
		return NewError(EEditTarget, "engine.step_until", "target step is in the past", map[string]any{"next_step": e.nextStep, "target": target})
	}
	for e.nextStep < target {
		if _, err := e.Step(); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Summary() map[string]any {
	return map[string]any{
		"next_step": e.nextStep, "next_ms": e.nextMS, "unresolved": e.unresolved,
		"emitted": len(e.emitted), "dropped": len(e.dropped),
		"delayed_queue": countDelayed(e.packets),
	}
}

func countDelayed(packets map[string]*packetRT) int {
	n := 0
	for _, rt := range packets {
		if rt.status == statusDelayed {
			n++
		}
	}
	return n
}

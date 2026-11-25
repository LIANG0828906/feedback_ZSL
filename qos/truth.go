package qos

import (
	"encoding/json"
	"fmt"
)

type TruthDocument struct {
	Schema  int            `json:"schema"`
	Version string         `json:"project_version"`
	Digest  string         `json:"project_digest"`
	Steps   []StepResult   `json:"steps"`
	Summary map[string]any `json:"summary"`
}

func EncodeTruth(p *Project, result *RunResult) ([]byte, error) {
	if p == nil || result == nil {
		return nil, NewError(EInternal, "truth", "project and result are required", nil)
	}
	digest, err := p.CanonicalDigest()
	if err != nil {
		return nil, err
	}
	steps := make([]any, 0, len(result.Steps))
	for _, st := range result.Steps {
		sm, err := truthStep(p, st)
		if err != nil {
			return nil, err
		}
		steps = append(steps, sm)
	}
	doc := map[string]any{
		"schema":          fmt.Sprintf("qos.v%d", p.SchemaVersion),
		"kind":            "truth",
		"project_version": p.Version,
		"project_digest":  digest,
		"steps":           steps,
		"emissions":       result.Emissions,
		"delayed_events":  result.Delays,
		"drops":           result.Drops,
		"expired":         result.Expired,
		"final_tokens":    result.FinalTokens,
		"summary": map[string]any{
			"steps":        result.TotalSteps,
			"emitted":      len(result.Emissions),
			"delayed":      len(result.Delays),
			"dropped":      len(result.Drops),
			"expired":      len(result.Expired),
			"final_tokens": result.FinalTokens,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, wrapError(EInternal, "truth", err, nil)
	}
	return append(raw, '\n'), nil
}

func truthStep(p *Project, st StepResult) (map[string]any, error) {
	tokens := make([]any, 0, len(st.Tokens))
	for _, ts := range st.Tokens {
		entry := map[string]any{
			"channel":                ts.ChannelID,
			"tokens_bytes":           ts.TokensBytes,
			"tokens_packets_milli":   ts.TokensPacketsMilli,
			"capacity_bytes":         ts.CapacityBytes,
			"capacity_packets_milli": ts.CapacityPacketsMilli,
			"refill_added_bytes":     ts.RefillAddedBytes,
			"refill_numerator":       ts.RefillNumerator,
			"refill_remainder":       ts.RefillRemainder,
			"saturated":              ts.Saturated,
		}
		tokens = append(tokens, entry)
	}
	delayed := make([]any, 0, len(st.Delayed))
	for _, d := range st.Delayed {
		entry := map[string]any{
			"packet_id": d.PacketID, "channel_id": d.ChannelID,
			"reason": d.Reason, "delay_ms": d.DelayMS,
			"deadline_ms": d.DeadlineMS,
		}
		switch p.SchemaVersion {
		case 1:
			t, err := ticks(d.ExpireAtMS, p.TickMS, "truth.delayed.expire_tick")
			if err != nil {
				return nil, err
			}
			entry["expire_tick"] = t
			entry["delay_ticks"] = d.DelayMS / p.TickMS
		case 2:
			entry["expire_at_ms"] = d.ExpireAtMS
		case 3:
			entry["remaining_ms"] = d.ExpireAtMS - st.AtMS
		}
		delayed = append(delayed, entry)
	}
	queue := make([]any, 0, len(st.Queue))
	for _, q := range st.Queue {
		entry := map[string]any{
			"packet_id": q.PacketID, "channel_id": q.ChannelID,
			"deadline_ms": q.DeadlineMS, "replay_seq": q.ReplaySeq,
			"reason": q.Reason, "size_bytes": q.SizeBytes,
		}
		switch p.SchemaVersion {
		case 1:
			t, err := ticks(q.ExpireAtMS, p.TickMS, "truth.delay_queue.expire_tick")
			if err != nil {
				return nil, err
			}
			if q.RemainingMS%p.TickMS != 0 {
				return nil, NewError(ETimeAlignment, "truth.delay_queue.remaining_ticks",
					"remaining delay is not tick aligned", map[string]any{"packet": q.PacketID, "remaining_ms": q.RemainingMS})
			}
			entry["expire_tick"] = t
			entry["remaining_ticks"] = q.RemainingMS / p.TickMS
		case 2:
			entry["expire_at_ms"] = q.ExpireAtMS
		case 3:
			entry["remaining_ms"] = q.RemainingMS
		}
		queue = append(queue, entry)
	}
	replayed := make([]any, 0, len(st.Replayed))
	for _, r := range st.Replayed {
		entry := map[string]any{
			"packet_id": r.PacketID, "channel_id": r.ChannelID, "replay_seq": r.ReplaySeq,
		}
		switch p.SchemaVersion {
		case 1:
			t, err := ticks(r.ExpireAtMS, p.TickMS, "truth.replayed.expire_tick")
			if err != nil {
				return nil, err
			}
			entry["expire_tick"] = t
		case 2:
			entry["expire_at_ms"] = r.ExpireAtMS
		case 3:
			entry["remaining_ms"] = r.ExpireAtMS - st.AtMS
		}
		replayed = append(replayed, entry)
	}
	emitted := make([]any, 0, len(st.Emitted))
	for _, em := range st.Emitted {
		entry := map[string]any{
			"packet_id": em.PacketID, "channel_id": em.ChannelID,
			"size_bytes": em.SizeBytes, "at_ms": em.AtMS,
			"packet_priority": em.PacketPriority, "effective_priority": em.EffectivePriority,
			"channel_order": em.ChannelOrder, "weight_basis_id": em.WeightBasisID,
			"emit_seq": em.EmitSeq,
		}
		if em.DSCP != nil {
			entry["dscp"] = *em.DSCP
		}
		emitted = append(emitted, entry)
	}
	dropped := make([]any, 0, len(st.Dropped))
	for _, dr := range st.Dropped {
		dropped = append(dropped, map[string]any{
			"packet_id": dr.PacketID, "channel_id": dr.ChannelID,
			"size_bytes": dr.SizeBytes, "at_ms": dr.AtMS,
			"reason_code": dr.ReasonCode, "reason": dr.Reason, "detail": dr.Detail,
		})
	}
	expired := make([]any, 0, len(st.Expired))
	for _, ex := range st.Expired {
		expired = append(expired, map[string]any{
			"packet_id": ex.PacketID, "channel_id": ex.ChannelID,
			"expire_at_ms": ex.ExpireAtMS, "deadline_ms": ex.DeadlineMS,
		})
	}
	suppressed := make([]any, 0, len(st.Suppressed))
	for _, sp := range st.Suppressed {
		suppressed = append(suppressed, map[string]any{
			"channel_id": sp.ChannelID, "pending_packets": sp.PendingPackets,
			"pending_bytes": sp.PendingBytes, "head_bytes": sp.HeadBytes,
			"budget_remaining_bytes": sp.BudgetRemaining,
			"weight_basis_id":        sp.WeightBasisID,
		})
	}
	step := map[string]any{
		"step": st.Index, "at_ms": st.AtMS, "tick": st.AtMS / p.TickMS,
		"tokens": tokens, "delayed": delayed, "dropped": dropped,
		"emitted": emitted, "delay_queue": queue, "replayed": replayed,
		"expired": expired, "suppressed": suppressed,
	}
	return step, nil
}

// DecodeTruth normalizes an emitted truth document back into the canonical
// StepResult representation and validates ordering invariants.
func DecodeTruth(data []byte, p *Project) (*TruthDocument, error) {
	root, err := decodeRoot(data)
	if err != nil {
		return nil, err
	}
	if raw, ok := root["kind"]; ok {
		var kind string
		if err := json.Unmarshal(raw, &kind); err != nil || kind != "truth" {
			return nil, NewError(EFieldType, "kind", "expected truth document", map[string]any{"kind": kind})
		}
	} else {
		return nil, NewError(EFieldMissing, "kind", "truth document kind is required", nil)
	}
	schema, err := truthSchema(root)
	if err != nil {
		return nil, err
	}
	if schema != p.SchemaVersion {
		return nil, NewError(ECheckpointVersion, "schema", "truth schema does not match project schema", map[string]any{"truth": schema, "project": p.SchemaVersion})
	}
	rawSteps, ok := root["steps"]
	if !ok {
		return nil, NewError(EFieldMissing, "steps", "truth steps are required", nil)
	}
	stepArr, err := array(rawSteps, "steps")
	if err != nil {
		return nil, err
	}
	doc := &TruthDocument{Schema: schema}
	if raw, ok := root["project_version"]; ok {
		_ = json.Unmarshal(raw, &doc.Version)
	}
	if raw, ok := root["project_digest"]; ok {
		_ = json.Unmarshal(raw, &doc.Digest)
	}
	for i, raw := range stepArr {
		o, err := object(raw, fmt.Sprintf("steps[%d]", i))
		if err != nil {
			return nil, err
		}
		st, err := decodeTruthStep(o, p, i)
		if err != nil {
			return nil, err
		}
		doc.Steps = append(doc.Steps, st)
	}
	return doc, nil
}

func truthSchema(root map[string]json.RawMessage) (int, error) {
	raw, ok := root["schema"]
	if !ok {
		return 0, NewError(EFieldMissing, "schema", "truth schema is required", nil)
	}
	var schema string
	if err := json.Unmarshal(raw, &schema); err != nil {
		return 0, wrapError(EFieldType, "schema", err, nil)
	}
	v, ok := schemaNumber(schema)
	if !ok {
		return 0, NewError(ESCHEMA, "schema", "unsupported truth schema", map[string]any{"schema": schema})
	}
	return v, nil
}

func decodeTruthStep(o map[string]json.RawMessage, p *Project, fallback int) (StepResult, error) {
	objs := []namedObj{{Path: fmt.Sprintf("steps[%d]", fallback), Obj: o}}
	step, ok, err := oneInt(objs, "step", "step")
	if err != nil {
		return StepResult{}, err
	}
	if !ok {
		step = int64(fallback)
	}
	atMS := step * p.TickMS
	if v, ok, err := oneInt(objs, "at_ms", "at_ms"); err != nil {
		return StepResult{}, err
	} else if ok {
		atMS = v
	}
	st := StepResult{Index: int(step), AtMS: atMS, AtTick: atMS / p.TickMS}
	if raw, ok := o["tokens"]; ok {
		arr, err := array(raw, fmt.Sprintf("steps[%d].tokens", fallback))
		if err != nil {
			return st, err
		}
		for j, tr := range arr {
			to, err := object(tr, fmt.Sprintf("steps[%d].tokens[%d]", fallback, j))
			if err != nil {
				return st, err
			}
			tobjs := []namedObj{{Path: fmt.Sprintf("steps[%d].tokens[%d]", fallback, j), Obj: to}}
			ts := TokenState{}
			if ts.ChannelID, err = requiredString(tobjs, "tokens.channel", "channel"); err != nil {
				return st, err
			}
			if ts.TokensBytes, err = requiredInt(tobjs, "tokens.tokens_bytes", "tokens_bytes"); err != nil {
				return st, err
			}
			ts.TokensPacketsMilli, _, _ = oneInt(tobjs, "tokens.tokens_packets_milli", "tokens_packets_milli")
			ts.CapacityBytes, _, _ = oneInt(tobjs, "tokens.capacity_bytes", "capacity_bytes")
			ts.CapacityPacketsMilli, _, _ = oneInt(tobjs, "tokens.capacity_packets_milli", "capacity_packets_milli")
			ts.RefillAddedBytes, _, _ = oneInt(tobjs, "tokens.refill_added_bytes", "refill_added_bytes")
			ts.RefillNumerator, _, _ = oneInt(tobjs, "tokens.refill_numerator", "refill_numerator")
			ts.RefillRemainder, _, _ = oneInt(tobjs, "tokens.refill_remainder", "refill_remainder")
			ts.Saturated, _, _ = oneBool(tobjs, "tokens.saturated", "saturated")
			st.Tokens = append(st.Tokens, ts)
		}
	}
	if raw, ok := o["delayed"]; ok {
		arr, err := array(raw, fmt.Sprintf("steps[%d].delayed", fallback))
		if err != nil {
			return st, err
		}
		for j, x := range arr {
			xo, err := object(x, fmt.Sprintf("steps[%d].delayed[%d]", fallback, j))
			if err != nil {
				return st, err
			}
			xobjs := []namedObj{{Path: fmt.Sprintf("steps[%d].delayed[%d]", fallback, j), Obj: xo}}
			d := DelayEvent{}
			d.PacketID, _ = requiredString(xobjs, "delayed.packet_id", "packet_id")
			d.ChannelID, _ = requiredString(xobjs, "delayed.channel_id", "channel_id")
			d.Reason, _ = requiredString(xobjs, "delayed.reason", "reason")
			d.DeadlineMS, _, _ = oneInt(xobjs, "delayed.deadline_ms", "deadline_ms")
			d.DelayMS, _, _ = oneInt(xobjs, "delayed.delay_ms", "delay_ms")
			switch p.SchemaVersion {
			case 1:
				t, _, err := oneInt(xobjs, "delayed.expire_tick", "expire_tick")
				if err != nil {
					return st, err
				}
				d.ExpireAtMS = t * p.TickMS
				if v, ok, _ := oneInt(xobjs, "delayed.delay_ticks", "delay_ticks"); ok {
					d.DelayMS = v * p.TickMS
				}
			case 2:
				d.ExpireAtMS, _, err = oneInt(xobjs, "delayed.expire_at_ms", "expire_at_ms")
				if err != nil {
					return st, err
				}
			case 3:
				rem, _, err := oneInt(xobjs, "delayed.remaining_ms", "remaining_ms")
				if err != nil {
					return st, err
				}
				d.ExpireAtMS = st.AtMS + rem
			}
			st.Delayed = append(st.Delayed, d)
		}
	}
	if raw, ok := o["dropped"]; ok {
		arr, err := array(raw, fmt.Sprintf("steps[%d].dropped", fallback))
		if err != nil {
			return st, err
		}
		for j, x := range arr {
			xo, err := object(x, fmt.Sprintf("steps[%d].dropped[%d]", fallback, j))
			if err != nil {
				return st, err
			}
			xobjs := []namedObj{{Path: fmt.Sprintf("steps[%d].dropped[%d]", fallback, j), Obj: xo}}
			dr := DropEvent{}
			dr.PacketID, _ = requiredString(xobjs, "dropped.packet_id", "packet_id")
			dr.ChannelID, _ = requiredString(xobjs, "dropped.channel_id", "channel_id")
			dr.SizeBytes, _, _ = oneInt(xobjs, "dropped.size_bytes", "size_bytes")
			dr.AtMS, _, _ = oneInt(xobjs, "dropped.at_ms", "at_ms")
			rc, _, _ := oneInt(xobjs, "dropped.reason_code", "reason_code")
			dr.ReasonCode = int(rc)
			dr.Reason, _ = requiredString(xobjs, "dropped.reason", "reason")
			dr.Detail, _, _ = oneString(xobjs, "dropped.detail", "detail")
			st.Dropped = append(st.Dropped, dr)
		}
	}
	if raw, ok := o["emitted"]; ok {
		arr, err := array(raw, fmt.Sprintf("steps[%d].emitted", fallback))
		if err != nil {
			return st, err
		}
		for j, x := range arr {
			xo, err := object(x, fmt.Sprintf("steps[%d].emitted[%d]", fallback, j))
			if err != nil {
				return st, err
			}
			xobjs := []namedObj{{Path: fmt.Sprintf("steps[%d].emitted[%d]", fallback, j), Obj: xo}}
			em := Emission{}
			em.PacketID, _ = requiredString(xobjs, "emitted.packet_id", "packet_id")
			em.ChannelID, _ = requiredString(xobjs, "emitted.channel_id", "channel_id")
			em.SizeBytes, _, _ = oneInt(xobjs, "emitted.size_bytes", "size_bytes")
			em.AtMS, _, _ = oneInt(xobjs, "emitted.at_ms", "at_ms")
			pp, _, _ := oneInt(xobjs, "emitted.packet_priority", "packet_priority")
			em.PacketPriority = int(pp)
			ep, _, _ := oneInt(xobjs, "emitted.effective_priority", "effective_priority")
			em.EffectivePriority = int(ep)
			co, _, _ := oneInt(xobjs, "emitted.channel_order", "channel_order")
			em.ChannelOrder = int(co)
			em.WeightBasisID, _ = requiredString(xobjs, "emitted.weight_basis_id", "weight_basis_id")
			em.EmitSeq, _, _ = oneInt(xobjs, "emitted.emit_seq", "emit_seq")
			if d, ok, _ := oneInt(xobjs, "emitted.dscp", "dscp"); ok {
				di := int(d)
				em.DSCP = &di
			}
			st.Emitted = append(st.Emitted, em)
		}
	}
	if raw, ok := o["delay_queue"]; ok {
		arr, err := array(raw, fmt.Sprintf("steps[%d].delay_queue", fallback))
		if err != nil {
			return st, err
		}
		seen := map[string]bool{}
		var prevExpiry int64 = -1
		for j, x := range arr {
			xo, err := object(x, fmt.Sprintf("steps[%d].delay_queue[%d]", fallback, j))
			if err != nil {
				return st, err
			}
			xobjs := []namedObj{{Path: fmt.Sprintf("steps[%d].delay_queue[%d]", fallback, j), Obj: xo}}
			q := QueueEntry{}
			q.PacketID, _ = requiredString(xobjs, "delay_queue.packet_id", "packet_id")
			q.ChannelID, _ = requiredString(xobjs, "delay_queue.channel_id", "channel_id")
			q.SizeBytes, _, _ = oneInt(xobjs, "delay_queue.size_bytes", "size_bytes")
			q.DeadlineMS, _, _ = oneInt(xobjs, "delay_queue.deadline_ms", "deadline_ms")
			q.ReplaySeq, _, _ = oneInt(xobjs, "delay_queue.replay_seq", "replay_seq")
			q.Reason, _ = requiredString(xobjs, "delay_queue.reason", "reason")
			switch p.SchemaVersion {
			case 1:
				t, _, err := oneInt(xobjs, "delay_queue.expire_tick", "expire_tick")
				if err != nil {
					return st, err
				}
				q.ExpireAtMS = t * p.TickMS
				rem, _, _ := oneInt(xobjs, "delay_queue.remaining_ticks", "remaining_ticks")
				q.RemainingMS = rem * p.TickMS
			case 2:
				q.ExpireAtMS, _, err = oneInt(xobjs, "delay_queue.expire_at_ms", "expire_at_ms")
				if err != nil {
					return st, err
				}
				q.RemainingMS = q.ExpireAtMS - st.AtMS
			case 3:
				q.RemainingMS, _, err = oneInt(xobjs, "delay_queue.remaining_ms", "remaining_ms")
				if err != nil {
					return st, err
				}
				q.ExpireAtMS = st.AtMS + q.RemainingMS
			}
			if seen[q.PacketID] {
				return st, NewError(EDelayQueue, "delay_queue.packet_id", "packet appears more than once in delay queue", map[string]any{"packet": q.PacketID})
			}
			seen[q.PacketID] = true
			if prevExpiry >= 0 && q.ExpireAtMS < prevExpiry {
				return st, NewError(EExpiryOrder, "delay_queue.expire", "delay queue is not ordered by expiry",
					map[string]any{"packet": q.PacketID, "expire_at_ms": q.ExpireAtMS, "previous_expire_at_ms": prevExpiry})
			}
			prevExpiry = q.ExpireAtMS
			st.Queue = append(st.Queue, q)
		}
	}
	if raw, ok := o["replayed"]; ok {
		arr, err := array(raw, fmt.Sprintf("steps[%d].replayed", fallback))
		if err != nil {
			return st, err
		}
		for j, x := range arr {
			xo, err := object(x, fmt.Sprintf("steps[%d].replayed[%d]", fallback, j))
			if err != nil {
				return st, err
			}
			xobjs := []namedObj{{Path: fmt.Sprintf("steps[%d].replayed[%d]", fallback, j), Obj: xo}}
			r := ReplayEvent{}
			r.PacketID, _ = requiredString(xobjs, "replayed.packet_id", "packet_id")
			r.ChannelID, _ = requiredString(xobjs, "replayed.channel_id", "channel_id")
			r.ReplaySeq, _, _ = oneInt(xobjs, "replayed.replay_seq", "replay_seq")
			switch p.SchemaVersion {
			case 1:
				t, _, err := oneInt(xobjs, "replayed.expire_tick", "expire_tick")
				if err != nil {
					return st, err
				}
				r.ExpireAtMS = t * p.TickMS
			case 2:
				r.ExpireAtMS, _, err = oneInt(xobjs, "replayed.expire_at_ms", "expire_at_ms")
				if err != nil {
					return st, err
				}
			case 3:
				rem, _, err := oneInt(xobjs, "replayed.remaining_ms", "remaining_ms")
				if err != nil {
					return st, err
				}
				r.ExpireAtMS = st.AtMS + rem
			}
			st.Replayed = append(st.Replayed, r)
		}
	}
	if raw, ok := o["expired"]; ok {
		arr, err := array(raw, fmt.Sprintf("steps[%d].expired", fallback))
		if err != nil {
			return st, err
		}
		for j, x := range arr {
			xo, err := object(x, fmt.Sprintf("steps[%d].expired[%d]", fallback, j))
			if err != nil {
				return st, err
			}
			xobjs := []namedObj{{Path: fmt.Sprintf("steps[%d].expired[%d]", fallback, j), Obj: xo}}
			ex := ExpiredEvent{}
			ex.PacketID, _ = requiredString(xobjs, "expired.packet_id", "packet_id")
			ex.ChannelID, _ = requiredString(xobjs, "expired.channel_id", "channel_id")
			ex.ExpireAtMS, _, _ = oneInt(xobjs, "expired.expire_at_ms", "expire_at_ms")
			ex.DeadlineMS, _, _ = oneInt(xobjs, "expired.deadline_ms", "deadline_ms")
			st.Expired = append(st.Expired, ex)
		}
	}
	if raw, ok := o["suppressed"]; ok {
		arr, err := array(raw, fmt.Sprintf("steps[%d].suppressed", fallback))
		if err != nil {
			return st, err
		}
		for j, x := range arr {
			xo, err := object(x, fmt.Sprintf("steps[%d].suppressed[%d]", fallback, j))
			if err != nil {
				return st, err
			}
			xobjs := []namedObj{{Path: fmt.Sprintf("steps[%d].suppressed[%d]", fallback, j), Obj: xo}}
			sp := SuppressedInfo{}
			sp.ChannelID, _ = requiredString(xobjs, "suppressed.channel_id", "channel_id")
			pn, _, _ := oneInt(xobjs, "suppressed.pending_packets", "pending_packets")
			sp.PendingPackets = int(pn)
			sp.PendingBytes, _, _ = oneInt(xobjs, "suppressed.pending_bytes", "pending_bytes")
			sp.HeadBytes, _, _ = oneInt(xobjs, "suppressed.head_bytes", "head_bytes")
			sp.BudgetRemaining, _, _ = oneInt(xobjs, "suppressed.budget_remaining_bytes", "budget_remaining_bytes")
			sp.WeightBasisID, _ = requiredString(xobjs, "suppressed.weight_basis_id", "weight_basis_id")
			st.Suppressed = append(st.Suppressed, sp)
		}
	}
	return st, nil
}

func TruthStepDigest(st StepResult) string {
	cp := st
	cp.SourceID = ""
	cp.Reused = false
	cp.CacheToken = ""
	raw, _ := json.Marshal(cp)
	return string(raw)
}

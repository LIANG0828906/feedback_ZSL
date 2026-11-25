package thermalcache

type GateResult struct {
	AtMs          int64  `json:"at_ms"`
	ObjectID      string `json:"object_id"`
	Kind          OpKind `json:"kind"`
	RequestedTier Tier   `json:"requested_tier,omitempty"`
	AnsweredTier  Tier   `json:"answered_tier"`
	Allowed       bool   `json:"allowed"`
	Code          string `json:"code,omitempty"`
	Version       int64  `json:"version"`
	TargetVisible bool   `json:"target_visible"`
	PendingWrites int64  `json:"pending_writes"`
	SourceVersion int64  `json:"source_version"`
	TargetVersion int64  `json:"target_version"`
}

// Gate performs a read or write against the currently committed answer layer.
// During migration reads always come from the source. Writes are accepted at
// source and queued; the target remains invisible until commit.
func (e *Engine) Gate(objectID string, kind OpKind, requested Tier) (GateResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gateLocked(e.currentAtMs, objectID, kind, requested)
}

func (e *Engine) gateLocked(at int64, objectID string, kind OpKind, requested Tier) (GateResult, error) {
	st := e.objects[objectID]
	if st == nil {
		return GateResult{}, biz(CodeObjectNotFound, "gate object does not exist", map[string]any{"object_id": objectID})
	}
	if kind != OpRead && kind != OpWrite {
		return GateResult{}, biz(CodeSchemaInvalid, "gate operation must be read or write", map[string]any{
			"object_id": objectID, "kind": kind,
		})
	}
	if requested != "" && !validateTier(requested) {
		return GateResult{}, biz(CodeSchemaInvalid, "gate requested tier is invalid", map[string]any{
			"object_id": objectID, "requested_tier": requested,
		})
	}
	allowed := true
	code := ""
	if requested != "" {
		switch {
		case st.Migrating && requested == st.TargetTier:
			allowed = false
			code = CodeGateTargetNotVisible
		case requested != st.Tier:
			allowed = false
			code = CodeGateWrongLayer
		}
	}
	version := st.SourceVersion
	if allowed && kind == OpWrite {
		st.SourceVersion++
		version = st.SourceVersion
		if st.Migrating {
			st.PendingWrites++
		}
	}
	if !allowed {
		st.GateDenials++
	}
	return GateResult{
		AtMs: at, ObjectID: objectID, Kind: kind, RequestedTier: requested,
		AnsweredTier: st.Tier, Allowed: allowed, Code: code, Version: version,
		TargetVisible: !st.Migrating, PendingWrites: st.PendingWrites,
		SourceVersion: st.SourceVersion, TargetVersion: st.TargetVersion,
	}, nil
}

func (e *Engine) applyOpEventsLocked(now int64) ([]GateResult, error) {
	var out []GateResult
	for e.opCursor < len(e.project.OpEvents) {
		ev := e.project.OpEvents[e.opCursor]
		if ev.AtMs > now {
			break
		}
		e.opCursor++
		gr, err := e.gateLocked(now, ev.ObjectID, ev.Kind, ev.RequestedTier)
		if err != nil {
			return nil, err
		}
		out = append(out, gr)
	}
	return out, nil
}

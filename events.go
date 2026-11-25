package hckernel

import (
	"encoding/json"
	"fmt"
)

func (e *Engine) processDueEvents(nowMs int64, report *RoundReport) error {
	for e.eventCursor < len(e.eventOrder) {
		ev := e.eventOrder[e.eventCursor]
		if ev.AtMs > nowMs {
			break
		}
		e.eventCursor++
		if err := e.processEvent(ev, report); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) processEvent(ev Event, report *RoundReport) error {
	switch ev.Type {
	case EventProbeResult:
		return e.applyResultEvent(ev)
	case EventInstanceAdd:
		if ev.Instance == nil {
			return newError(ErrSchemaPayloadInvalid, "instance_added requires instance", withEvent(ev.ID))
		}
		return e.addInstance(*ev.Instance, ev.ID, report)
	case EventInstanceRemove:
		return e.removeInstance(ev.InstanceID, ev.ID, report)
	case EventRackMove:
		return e.moveInstance(ev.InstanceID, ev.ToRackID, ev.ID, report)
	case EventConfigEdit:
		if ev.Edit == nil {
			return newError(ErrEditFieldUnsupported, "config_edited requires edit", withEvent(ev.ID))
		}
		return e.applyEdit(ev.InstanceID, *ev.Edit, ev.ID, report)
	default:
		return newError(ErrSchemaPayloadInvalid, "unsupported event type: "+string(ev.Type), withEvent(ev.ID))
	}
}

func (e *Engine) addInstance(inst Instance, eventID string, report *RoundReport) error {
	if _, ok := e.states[inst.ID]; ok {
		return newError(ErrInstanceDuplicate, "instance already exists", withInstance(inst.ID), withEvent(eventID))
	}
	if _, ok := e.project.rackMap()[inst.RackID]; !ok {
		return newError(ErrRackReferenceMissing, "added instance references missing rack", withInstance(inst.ID), withRack(inst.RackID), withEvent(eventID))
	}
	if inst.IntervalMs == 0 {
		return newError(ErrIntervalZero, "added instance interval is zero", withInstance(inst.ID), withEvent(eventID))
	}
	if inst.TimeoutMs < 0 {
		return newError(ErrNegativeTimeout, "added instance timeout is negative", withInstance(inst.ID), withEvent(eventID))
	}
	if inst.SuccessThreshold <= 0 || inst.SuccessThreshold > MaxThreshold ||
		inst.FailureThreshold <= 0 || inst.FailureThreshold > MaxThreshold {
		return newError(ErrThresholdOutOfRange, "added instance threshold out of range", withInstance(inst.ID), withEvent(eventID))
	}
	if inst.WeightPPM <= 0 {
		return newError(ErrWeightInvalid, "added instance weight invalid", withInstance(inst.ID), withEvent(eventID))
	}
	e.project.Instances = append(e.project.Instances, inst)
	rt := &InstanceState{Instance: inst, Health: inst.InitialState}
	if rt.Health == StateUnhealthy {
		rt.HalfOpenAt = e.project.CountPolicy.HalfOpenDelayMs
	}
	e.states[inst.ID] = rt
	if report != nil {
		report.Topology = append(report.Topology, eventID+":add:"+inst.ID)
	}
	return nil
}

func (e *Engine) removeInstance(id, eventID string, report *RoundReport) error {
	rt, ok := e.states[id]
	if !ok {
		return newError(ErrInstanceNotFound, "cannot remove missing instance", withInstance(id), withEvent(eventID))
	}
	if rt.Outstanding != nil {
		e.globalActive--
		e.rackActive[rt.Outstanding.RackID]--
		rt.Outstanding = nil
	}
	e.queue = removeQueueInstance(e.queue, id)
	delete(e.states, id)
	for i, inst := range e.project.Instances {
		if inst.ID == id {
			e.project.Instances = append(e.project.Instances[:i], e.project.Instances[i+1:]...)
			break
		}
	}
	if report != nil {
		report.Topology = append(report.Topology, eventID+":remove:"+id)
	}
	return nil
}

func removeQueueInstance(queue []DeferredEntry, id string) []DeferredEntry {
	out := queue[:0]
	for _, entry := range queue {
		if entry.InstanceID != id {
			out = append(out, entry)
		}
	}
	return out
}

func (e *Engine) moveInstance(id, toRack, eventID string, report *RoundReport) error {
	rt, ok := e.states[id]
	if !ok {
		return newError(ErrInstanceNotFound, "cannot move missing instance", withInstance(id), withEvent(eventID))
	}
	if _, ok := e.project.rackMap()[toRack]; !ok {
		return newError(ErrRackReferenceMissing, "rack migration target does not exist", withInstance(id), withRack(toRack), withEvent(eventID))
	}
	old := rt.Instance.RackID
	rt.Instance.RackID = toRack
	for i := range e.project.Instances {
		if e.project.Instances[i].ID == id {
			e.project.Instances[i].RackID = toRack
		}
	}
	for i := range e.queue {
		if e.queue[i].InstanceID == id {
			e.queue[i].RackID = toRack
			e.queue[i].ConstraintID = RackConcurrencyConstraint(toRack)
		}
	}
	if report != nil {
		report.Topology = append(report.Topology, fmt.Sprintf("%s:move:%s:%s->%s", eventID, id, old, toRack))
	}
	return nil
}

func (e *Engine) applyEdit(id string, edit Edit, eventID string, report *RoundReport) error {
	rt, ok := e.states[id]
	if !ok {
		return newError(ErrInstanceNotFound, "cannot edit missing instance", withInstance(id), withEvent(eventID))
	}
	switch edit.Field {
	case "interval_ms":
		value, err := int64Value(edit.NewValue)
		if err != nil {
			return newError(ErrEditFieldUnsupported, "interval_ms requires integer value", withInstance(id), withEvent(eventID), withCause(err))
		}
		if value == 0 {
			return newError(ErrIntervalZero, "edited interval is zero", withInstance(id), withEvent(eventID))
		}
		if value < 0 {
			return newError(ErrIntervalInvalid, "edited interval is negative", withInstance(id), withEvent(eventID))
		}
		rt.Instance.IntervalMs = value
		rt.NextPeriodAt = e.currentLogicalMs() + value
	case "timeout_ms":
		value, err := int64Value(edit.NewValue)
		if err != nil {
			return newError(ErrEditFieldUnsupported, "timeout_ms requires integer value", withInstance(id), withEvent(eventID), withCause(err))
		}
		if value < 0 {
			return newError(ErrNegativeTimeout, "edited timeout is negative", withInstance(id), withEvent(eventID))
		}
		rt.Instance.TimeoutMs = value
	case "success_threshold", "failure_threshold":
		value, err := intValue(edit.NewValue)
		if err != nil {
			return newError(ErrEditFieldUnsupported, "threshold requires integer value", withInstance(id), withEvent(eventID), withCause(err))
		}
		if value <= 0 || value > MaxThreshold {
			return newError(ErrThresholdOutOfRange, "edited threshold out of range [1,100]", withInstance(id), withEvent(eventID))
		}
		if edit.Field == "success_threshold" {
			rt.Instance.SuccessThreshold = value
		} else {
			rt.Instance.FailureThreshold = value
		}
	case "weight_ppm":
		value, err := int64Value(edit.NewValue)
		if err != nil {
			return newError(ErrEditFieldUnsupported, "weight_ppm requires integer value", withInstance(id), withEvent(eventID), withCause(err))
		}
		if value <= 0 {
			return newError(ErrWeightInvalid, "edited weight must be positive", withInstance(id), withEvent(eventID))
		}
		rt.Instance.WeightPPM = value
	case "rack_id":
		value, ok := edit.NewValue.(string)
		if !ok {
			return newError(ErrEditFieldUnsupported, "rack_id requires string", withInstance(id), withEvent(eventID))
		}
		return e.moveInstance(id, value, eventID, report)
	default:
		return newError(ErrEditFieldUnsupported, "unsupported edit field: "+edit.Field, withInstance(id), withEvent(eventID))
	}
	e.syncProjectInstance(rt.Instance)
	if report != nil {
		report.Topology = append(report.Topology, eventID+":edit:"+id+":"+edit.Field)
	}
	return nil
}

func (e *Engine) syncProjectInstance(inst Instance) {
	for i := range e.project.Instances {
		if e.project.Instances[i].ID == inst.ID {
			e.project.Instances[i] = inst
			return
		}
	}
}

func (e *Engine) currentLogicalMs() int64 {
	return e.nowMs
}

func int64Value(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	case json.Number:
		return x.Int64()
	default:
		return 0, fmt.Errorf("not an integer: %T", v)
	}
}

func intValue(v any) (int, error) {
	x, err := int64Value(v)
	return int(x), err
}

package samplegen

import (
	"sort"

	"inputkernel/internal/model"
)

func fillTruth(s *model.Scenario) error {
	contexts := sampleContexts(s)
	axisSamples := make([]model.TruthAxisSample, 0)
	actions := make([]model.TruthAction, 0)
	for i := range s.Bindings {
		b := &s.Bindings[i]
		var outputs []model.TruthAction
		var samples []model.TruthAxisSample
		switch b.Type {
		case model.BindingAxis:
			samples, outputs = truthAxis(s, contexts, b)
		case model.BindingButton:
			outputs = truthButton(contexts, b)
		case model.BindingCombo:
			outputs = truthCombo(contexts, b)
		case model.BindingLongPress:
			outputs = truthLongPress(contexts, b)
		case model.BindingDoublePress:
			outputs = truthDoublePress(contexts, b)
		}
		axisSamples = append(axisSamples, samples...)
		actions = append(actions, outputs...)
	}
	sort.SliceStable(axisSamples, func(i, j int) bool {
		if axisSamples[i].EventSeq != axisSamples[j].EventSeq {
			return axisSamples[i].EventSeq < axisSamples[j].EventSeq
		}
		return axisSamples[i].BindingID < axisSamples[j].BindingID
	})
	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].EventSeq != actions[j].EventSeq {
			return actions[i].EventSeq < actions[j].EventSeq
		}
		return actions[i].BindingID < actions[j].BindingID
	})
	s.Truth = model.Truth{
		AxisSamples: axisSamples,
		Actions:     actions,
		Unmapped:    truthUnmapped(s, contexts),
	}
	if s.Truth.Unmapped == nil {
		s.Truth.Unmapped = []model.TruthUnmapped{}
	}
	return nil
}

type sampleCtx struct {
	event  model.Event
	active string
	after  string
}

func sampleContexts(s *model.Scenario) []sampleCtx {
	current := s.InitialDeviceID
	out := make([]sampleCtx, 0, len(s.Events))
	for _, e := range s.Events {
		ctx := sampleCtx{event: e, active: current, after: current}
		if e.Kind == model.EventDeviceSwitch {
			ctx.after = e.Device
			current = e.Device
		}
		out = append(out, ctx)
	}
	return out
}

func truthButton(contexts []sampleCtx, b *model.Binding) []model.TruthAction {
	var out []model.TruthAction
	active := false
	for _, ctx := range contexts {
		e := ctx.event
		if e.Kind == model.EventDeviceSwitch {
			if ctx.active == b.DeviceID && ctx.after != b.DeviceID && active {
				out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateReleased, nil)
				active = false
			}
			continue
		}
		if ctx.active != b.DeviceID || e.Button != b.Button {
			continue
		}
		switch e.Kind {
		case model.EventButtonDown:
			out = appendTruth(out, b, e.Seq, e.TimeMS, model.StatePressed, nil)
			active = true
		case model.EventButtonRepeat:
			if active {
				out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateHeld, nil)
			}
		case model.EventButtonUp:
			if active {
				out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateReleased, nil)
				active = false
			}
		}
	}
	return out
}

func truthCombo(contexts []sampleCtx, b *model.Binding) []model.TruthAction {
	var out []model.TruthAction
	downAt := map[string]int64{}
	active := false
	for _, ctx := range contexts {
		e := ctx.event
		if e.Kind == model.EventDeviceSwitch {
			if ctx.active == b.DeviceID && ctx.after != b.DeviceID {
				if active {
					out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateReleased, nil)
				}
				downAt = map[string]int64{}
				active = false
			}
			continue
		}
		if ctx.active != b.DeviceID {
			continue
		}
		switch e.Kind {
		case model.EventButtonDown:
			if !contains(b.Buttons, e.Button) {
				continue
			}
			if _, exists := downAt[e.Button]; !exists {
				downAt[e.Button] = e.TimeMS
			}
			if len(downAt) == len(b.Buttons) {
				first, last := minMax(downAt)
				if !active && last-first <= b.Timing.WindowMS {
					out = appendTruth(out, b, e.Seq, e.TimeMS, model.StatePressed, nil)
					active = true
				}
			}
		case model.EventButtonUp:
			if !contains(b.Buttons, e.Button) {
				continue
			}
			delete(downAt, e.Button)
			if active {
				out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateReleased, nil)
				active = false
				downAt = map[string]int64{}
			}
		case model.EventButtonRepeat:
			if active && contains(b.Buttons, e.Button) {
				out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateHeld, nil)
			}
		}
	}
	return out
}

func truthLongPress(contexts []sampleCtx, b *model.Binding) []model.TruthAction {
	var out []model.TruthAction
	downAt := int64(-1)
	active, triggered := false, false
	for _, ctx := range contexts {
		e := ctx.event
		if e.Kind == model.EventDeviceSwitch {
			if ctx.active == b.DeviceID && ctx.after != b.DeviceID {
				if active {
					out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateReleased, nil)
				}
				downAt = -1
				active, triggered = false, false
			}
			continue
		}
		if ctx.active != b.DeviceID {
			continue
		}
		if e.Kind == model.EventButtonDown && e.Button == b.Button {
			downAt = e.TimeMS
			active, triggered = false, false
		}
		if downAt >= 0 && !triggered {
			triggerAt := downAt + b.Timing.ThresholdMS
			if e.TimeMS >= triggerAt {
				out = appendTruth(out, b, e.Seq, triggerAt, model.StatePressed, nil)
				triggered, active = true, true
			}
		}
		if e.Button != b.Button {
			continue
		}
		switch e.Kind {
		case model.EventButtonRepeat:
			if active {
				out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateHeld, nil)
			}
		case model.EventButtonUp:
			if active {
				out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateReleased, nil)
			}
			downAt = -1
			active, triggered = false, false
		}
	}
	return out
}

func truthDoublePress(contexts []sampleCtx, b *model.Binding) []model.TruthAction {
	var out []model.TruthAction
	const (
		none = iota
		firstDown
		waitSecond
		secondDown
	)
	phase := none
	var firstAt, firstRelease int64
	for _, ctx := range contexts {
		e := ctx.event
		if e.Kind == model.EventDeviceSwitch {
			if ctx.active == b.DeviceID && ctx.after != b.DeviceID {
				phase = none
				firstAt, firstRelease = 0, 0
			}
			continue
		}
		if ctx.active != b.DeviceID || e.Button != b.Button {
			continue
		}
		switch e.Kind {
		case model.EventButtonDown:
			switch phase {
			case none:
				phase = firstDown
				firstAt = e.TimeMS
			case waitSecond:
				if e.TimeMS-firstRelease >= b.Timing.MinGapMS && e.TimeMS-firstAt <= b.Timing.WindowMS {
					phase = secondDown
				} else {
					phase = firstDown
					firstAt = e.TimeMS
					firstRelease = 0
				}
			}
		case model.EventButtonUp:
			switch phase {
			case firstDown:
				firstRelease = e.TimeMS
				phase = waitSecond
			case secondDown:
				if e.TimeMS-firstAt <= b.Timing.WindowMS && e.TimeMS-firstRelease >= b.Timing.MinGapMS {
					out = appendTruth(out, b, e.Seq, e.TimeMS, model.StatePressed, nil)
				}
				phase = none
				firstAt, firstRelease = 0, 0
			}
		}
	}
	return out
}

func truthAxis(s *model.Scenario, contexts []sampleCtx, b *model.Binding) ([]model.TruthAxisSample, []model.TruthAction) {
	d := deviceByID(s, b.DeviceID)
	a := axisByID(d, b.Axis)
	c := curveByID(s, b.Curve)
	var samples []model.TruthAxisSample
	var out []model.TruthAction
	active := false
	var lastValue int64
	for _, ctx := range contexts {
		e := ctx.event
		if e.Kind == model.EventDeviceSwitch {
			if ctx.active == b.DeviceID && ctx.after != b.DeviceID && active {
				value := lastValue
				out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateReleased, &value)
				active = false
			}
			continue
		}
		if ctx.active != b.DeviceID || e.Kind != model.EventAxis || e.Axis != b.Axis {
			continue
		}
		q, residual := truthNormalize(*a, d.FixedScale, e.Value)
		margin := abs(abs(q) - a.Deadzone)
		inside := abs(q) <= a.Deadzone
		if inside {
			q = 0
		}
		if a.Invert {
			q = -q
			residual.Num = -residual.Num
		}
		value := truthCurve(*c, q)
		lastValue = value
		samples = append(samples, model.TruthAxisSample{
			EventSeq: e.Seq, TimeMS: e.TimeMS, DeviceID: b.DeviceID,
			BindingID: b.ID, ActionID: b.Action, AxisID: b.Axis, Raw: e.Value,
			Normalized: value, Residual: residual, ResidualBound: 1,
			DeadzoneInside: inside, DeadzoneMargin: margin,
		})
		if inside {
			if active {
				out = appendTruth(out, b, e.Seq, e.TimeMS, model.StateReleased, &value)
				active = false
			}
			continue
		}
		state := model.StateHeld
		if !active {
			state = model.StatePressed
			active = true
		}
		out = appendTruth(out, b, e.Seq, e.TimeMS, state, &value)
	}
	return samples, out
}

func truthUnmapped(s *model.Scenario, contexts []sampleCtx) []model.TruthUnmapped {
	axes := map[string]map[string]bool{}
	buttons := map[string]map[string]bool{}
	for _, b := range s.Bindings {
		if axes[b.DeviceID] == nil {
			axes[b.DeviceID] = map[string]bool{}
			buttons[b.DeviceID] = map[string]bool{}
		}
		switch b.Type {
		case model.BindingAxis:
			axes[b.DeviceID][b.Axis] = true
		case model.BindingButton, model.BindingLongPress, model.BindingDoublePress:
			buttons[b.DeviceID][b.Button] = true
		case model.BindingCombo:
			for _, button := range b.Buttons {
				buttons[b.DeviceID][button] = true
			}
		}
	}
	out := make([]model.TruthUnmapped, 0)
	for _, ctx := range contexts {
		e := ctx.event
		switch e.Kind {
		case model.EventAxis:
			if !axes[ctx.active][e.Axis] {
				out = append(out, model.TruthUnmapped{EventSeq: e.Seq, TimeMS: e.TimeMS, DeviceID: ctx.active, Control: e.Axis, Kind: "axis"})
			}
		case model.EventButtonDown, model.EventButtonUp, model.EventButtonRepeat:
			if !buttons[ctx.active][e.Button] {
				out = append(out, model.TruthUnmapped{EventSeq: e.Seq, TimeMS: e.TimeMS, DeviceID: ctx.active, Control: e.Button, Kind: string(e.Kind)})
			}
		}
	}
	return out
}

func appendTruth(out []model.TruthAction, b *model.Binding, seq int, time int64, state model.ActionState, axis *int64) []model.TruthAction {
	return append(out, model.TruthAction{
		BindingID: b.ID, ActionID: b.Action, EventSeq: seq, TriggerTimeMS: time,
		State: state, AxisValue: axis,
	})
}

func truthNormalize(a model.AxisSpec, scale, raw int64) (int64, model.Fraction) {
	span := a.Max - a.Min
	var num int64
	if a.Bipolar {
		num = 2*(raw-a.Min)*scale - span*scale
	} else {
		num = (raw - a.Min) * scale
		if a.Direction == "negative" {
			num -= span * scale
		}
	}
	q := roundDiv(num, span)
	return q, model.Fraction{Num: num - q*span, Den: span}
}

func truthCurve(c model.Curve, x int64) int64 {
	if x <= c.Points[0].X {
		return c.Points[0].Y
	}
	for i := 1; i < len(c.Points); i++ {
		p0, p1 := c.Points[i-1], c.Points[i]
		if x <= p1.X {
			return p0.Y + roundDiv((x-p0.X)*(p1.Y-p0.Y), p1.X-p0.X)
		}
	}
	return c.Points[len(c.Points)-1].Y
}

func roundDiv(num, den int64) int64 {
	if den < 0 {
		num, den = -num, -den
	}
	q, r := num/den, num%den
	if r < 0 {
		r = -r
	}
	if r*2 >= den {
		if num >= 0 {
			q++
		} else {
			q--
		}
	}
	return q
}

func minMax(values map[string]int64) (int64, int64) {
	first, last := int64(1<<62), int64(-1<<62)
	for _, v := range values {
		if v < first {
			first = v
		}
		if v > last {
			last = v
		}
	}
	return first, last
}

func deviceByID(s *model.Scenario, id string) *model.Device {
	for i := range s.Devices {
		if s.Devices[i].ID == id {
			return &s.Devices[i]
		}
	}
	return nil
}

func axisByID(d *model.Device, id string) *model.AxisSpec {
	for i := range d.Axes {
		if d.Axes[i].ID == id {
			return &d.Axes[i]
		}
	}
	return nil
}

func curveByID(s *model.Scenario, id string) *model.Curve {
	for i := range s.Curves {
		if s.Curves[i].ID == id {
			return &s.Curves[i]
		}
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, v := range values {
		if v == wanted {
			return true
		}
	}
	return false
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

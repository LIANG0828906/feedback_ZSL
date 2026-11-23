package kernel

import (
	"fmt"
	"sort"

	"inputkernel/internal/fixed"
	"inputkernel/internal/model"
)

type eventContext struct {
	Event        model.Event
	ActiveDevice string
	AfterDevice  string
}

func buildContexts(s *model.Scenario) []eventContext {
	out := make([]eventContext, 0, len(s.Events))
	current := s.InitialDeviceID
	for _, e := range s.Events {
		ctx := eventContext{Event: e, ActiveDevice: current, AfterDevice: current}
		if e.Kind == model.EventDeviceSwitch {
			ctx.AfterDevice = e.Device
			current = e.Device
		}
		out = append(out, ctx)
	}
	return out
}

func computeAxisObservations(s *model.Scenario, contexts []eventContext, deviceID, axisID string) ([]AxisObservation, error) {
	d, ok := findDevice(s, deviceID)
	if !ok {
		return nil, model.Schema("device", deviceID, "device not found while computing axis node")
	}
	a, ok := findAxis(d, axisID)
	if !ok {
		return nil, model.Schema("axis", axisID, "axis not found while computing axis node")
	}
	out := make([]AxisObservation, 0)
	for _, ctx := range contexts {
		e := ctx.Event
		if ctx.ActiveDevice != deviceID || e.Kind != model.EventAxis || e.Axis != axisID {
			continue
		}
		value, residual, inside, margin := fixed.NormalizeAxis(*a, d.FixedScale, e.Value)
		out = append(out, AxisObservation{
			EventSeq: e.Seq, TimeMS: e.TimeMS, DeviceID: deviceID, AxisID: axisID,
			Raw: e.Value, BaseNormalized: value, Residual: residual,
			DeadzoneInside: inside, DeadzoneMargin: margin,
		})
	}
	return out, nil
}

func computeActionNode(s *model.Scenario, contexts []eventContext, b *model.Binding, observations []AxisObservation) (*ActionPayload, error) {
	switch b.Type {
	case model.BindingAxis:
		return replayAxis(s, contexts, b, observations)
	case model.BindingButton:
		return replayButton(contexts, b), nil
	case model.BindingCombo:
		return replayCombo(contexts, b), nil
	case model.BindingLongPress:
		return replayLongPress(contexts, b), nil
	case model.BindingDoublePress:
		return replayDoublePress(contexts, b), nil
	default:
		return nil, model.Schema("binding.type", string(b.Type), "unsupported binding type")
	}
}

type outputBuilder struct {
	binding *model.Binding
	out     *ActionPayload
	ordinal int
}

func newOutputBuilder(b *model.Binding) *outputBuilder {
	return &outputBuilder{binding: b, out: &ActionPayload{}}
}

func (o *outputBuilder) add(seq int, triggerMS int64, state model.ActionState, axis *int64, residual *model.Fraction, curveErr *int64) {
	entry := ActionOutput{
		EventSeq: seq, TriggerTimeMS: triggerMS, BindingID: o.binding.ID,
		ActionID: o.binding.Action, DeviceID: o.binding.DeviceID,
		State: state, AxisValue: axis, Residual: residual, CurveError: curveErr,
		Ordinal: o.ordinal,
	}
	o.ordinal++
	o.out.Outputs = append(o.out.Outputs, entry)
}

func (o *outputBuilder) addTiming(check TimingCheck) {
	check.BindingID = o.binding.ID
	o.out.TimingChecks = append(o.out.TimingChecks, check)
}

func replayButton(contexts []eventContext, b *model.Binding) *ActionPayload {
	o := newOutputBuilder(b)
	active := false
	for _, ctx := range contexts {
		e := ctx.Event
		if e.Kind == model.EventDeviceSwitch {
			if ctx.ActiveDevice == b.DeviceID && ctx.AfterDevice != b.DeviceID && active {
				o.add(e.Seq, e.TimeMS, model.StateReleased, nil, nil, nil)
				active = false
			}
			continue
		}
		if ctx.ActiveDevice != b.DeviceID || e.Button != b.Button {
			continue
		}
		switch e.Kind {
		case model.EventButtonDown:
			o.add(e.Seq, e.TimeMS, model.StatePressed, nil, nil, nil)
			active = true
		case model.EventButtonRepeat:
			if active {
				o.add(e.Seq, e.TimeMS, model.StateHeld, nil, nil, nil)
			}
		case model.EventButtonUp:
			if active {
				o.add(e.Seq, e.TimeMS, model.StateReleased, nil, nil, nil)
				active = false
			}
		}
	}
	return o.out
}

func replayCombo(contexts []eventContext, b *model.Binding) *ActionPayload {
	o := newOutputBuilder(b)
	downAt := map[string]int64{}
	active := false
	var lastEventMS int64
	for _, ctx := range contexts {
		e := ctx.Event
		gap := e.TimeMS - lastEventMS
		if gap < 0 {
			gap = 0
		}
		lastEventMS = e.TimeMS
		if e.Kind == model.EventDeviceSwitch {
			if ctx.ActiveDevice == b.DeviceID && ctx.AfterDevice != b.DeviceID {
				if active {
					o.add(e.Seq, e.TimeMS, model.StateReleased, nil, nil, nil)
				}
				downAt = map[string]int64{}
				active = false
			}
			continue
		}
		if ctx.ActiveDevice != b.DeviceID {
			continue
		}
		switch e.Kind {
		case model.EventButtonDown:
			if !containsString(b.Buttons, e.Button) {
				continue
			}
			if _, exists := downAt[e.Button]; !exists {
				downAt[e.Button] = e.TimeMS
			}
			if len(downAt) == len(b.Buttons) {
				earliest, latest := comboBounds(downAt)
				margin := b.Timing.WindowMS - (latest - earliest)
				o.addTiming(TimingCheck{
					EventSeq: e.Seq, TimeMS: e.TimeMS, Kind: "combo_window",
					MarginMS: margin, WindowMS: b.Timing.WindowMS, EventGap: gap,
				})
				if !active && margin >= 0 {
					o.add(e.Seq, e.TimeMS, model.StatePressed, nil, nil, nil)
					active = true
				}
			} else {
				earliest := int64(1 << 62)
				for _, t := range downAt {
					if t < earliest {
						earliest = t
					}
				}
				o.addTiming(TimingCheck{
					EventSeq: e.Seq, TimeMS: e.TimeMS, Kind: "combo_window",
					MarginMS: b.Timing.WindowMS - (e.TimeMS - earliest),
					WindowMS: b.Timing.WindowMS, EventGap: gap,
				})
			}
		case model.EventButtonUp:
			if !containsString(b.Buttons, e.Button) {
				continue
			}
			delete(downAt, e.Button)
			if active {
				o.add(e.Seq, e.TimeMS, model.StateReleased, nil, nil, nil)
				active = false
				downAt = map[string]int64{}
			}
		case model.EventButtonRepeat:
			if active && containsString(b.Buttons, e.Button) {
				o.add(e.Seq, e.TimeMS, model.StateHeld, nil, nil, nil)
			}
		}
	}
	return o.out
}

func replayLongPress(contexts []eventContext, b *model.Binding) *ActionPayload {
	o := newOutputBuilder(b)
	var downAt int64 = -1
	var lastEventMS int64
	active, triggered := false, false
	for _, ctx := range contexts {
		e := ctx.Event
		gap := e.TimeMS - lastEventMS
		if gap < 0 {
			gap = 0
		}
		lastEventMS = e.TimeMS
		if e.Kind == model.EventDeviceSwitch {
			if ctx.ActiveDevice == b.DeviceID && ctx.AfterDevice != b.DeviceID {
				if active {
					o.add(e.Seq, e.TimeMS, model.StateReleased, nil, nil, nil)
				}
				downAt = -1
				active, triggered = false, false
			}
			continue
		}
		if ctx.ActiveDevice != b.DeviceID {
			continue
		}
		if e.Kind == model.EventButtonDown && e.Button == b.Button {
			downAt = e.TimeMS
			active, triggered = false, false
		}
		if downAt >= 0 && !triggered {
			triggerAt := downAt + b.Timing.ThresholdMS
			if e.TimeMS >= triggerAt {
				o.add(e.Seq, triggerAt, model.StatePressed, nil, nil, nil)
				triggered, active = true, true
			} else {
				o.addTiming(TimingCheck{
					EventSeq: e.Seq, TimeMS: e.TimeMS, Kind: "long_press_threshold",
					MarginMS: triggerAt - e.TimeMS, WindowMS: b.Timing.ThresholdMS, EventGap: gap,
				})
			}
		}
		if e.Button != b.Button {
			continue
		}
		switch e.Kind {
		case model.EventButtonRepeat:
			if active {
				o.add(e.Seq, e.TimeMS, model.StateHeld, nil, nil, nil)
			}
		case model.EventButtonUp:
			if active {
				o.add(e.Seq, e.TimeMS, model.StateReleased, nil, nil, nil)
			}
			downAt = -1
			active, triggered = false, false
		}
	}
	return o.out
}

func replayDoublePress(contexts []eventContext, b *model.Binding) *ActionPayload {
	o := newOutputBuilder(b)
	const (
		phaseNone = iota
		phaseFirstDown
		phaseWaitSecond
		phaseSecondDown
	)
	phase := phaseNone
	var firstDown, firstRelease int64
	var lastEventMS int64
	for _, ctx := range contexts {
		e := ctx.Event
		gap := e.TimeMS - lastEventMS
		if gap < 0 {
			gap = 0
		}
		lastEventMS = e.TimeMS
		if e.Kind == model.EventDeviceSwitch {
			if ctx.ActiveDevice == b.DeviceID && ctx.AfterDevice != b.DeviceID {
				phase = phaseNone
				firstDown, firstRelease = 0, 0
			}
			continue
		}
		if ctx.ActiveDevice != b.DeviceID || e.Button != b.Button {
			continue
		}
		switch e.Kind {
		case model.EventButtonDown:
			switch phase {
			case phaseNone:
				phase = phaseFirstDown
				firstDown = e.TimeMS
			case phaseWaitSecond:
				if e.TimeMS-firstRelease >= b.Timing.MinGapMS && e.TimeMS-firstDown <= b.Timing.WindowMS {
					phase = phaseSecondDown
					o.addTiming(TimingCheck{
						EventSeq: e.Seq, TimeMS: e.TimeMS, Kind: "double_press_window",
						MarginMS: b.Timing.WindowMS - (e.TimeMS - firstDown),
						WindowMS: b.Timing.WindowMS, EventGap: gap,
					})
				} else {
					phase = phaseFirstDown
					firstDown = e.TimeMS
					firstRelease = 0
				}
			}
		case model.EventButtonUp:
			switch phase {
			case phaseFirstDown:
				firstRelease = e.TimeMS
				phase = phaseWaitSecond
				o.addTiming(TimingCheck{
					EventSeq: e.Seq, TimeMS: e.TimeMS, Kind: "double_press_window",
					MarginMS: b.Timing.WindowMS - (e.TimeMS - firstDown),
					WindowMS: b.Timing.WindowMS, EventGap: gap,
				})
			case phaseSecondDown:
				margin := b.Timing.WindowMS - (e.TimeMS - firstDown)
				o.addTiming(TimingCheck{
					EventSeq: e.Seq, TimeMS: e.TimeMS, Kind: "double_press_window",
					MarginMS: margin, WindowMS: b.Timing.WindowMS, EventGap: gap,
				})
				if margin >= 0 && e.TimeMS-firstRelease >= b.Timing.MinGapMS {
					o.add(e.Seq, e.TimeMS, model.StatePressed, nil, nil, nil)
				}
				phase = phaseNone
				firstDown, firstRelease = 0, 0
			}
		}
	}
	return o.out
}

func replayAxis(s *model.Scenario, contexts []eventContext, b *model.Binding, observations []AxisObservation) (*ActionPayload, error) {
	c, ok := findCurve(s, b.Curve)
	if !ok {
		return nil, model.Schema("binding.curve", b.Curve, "curve not found")
	}
	o := newOutputBuilder(b)
	active := false
	var lastValue int64
	obsIndex := 0
	for _, ctx := range contexts {
		e := ctx.Event
		if e.Kind == model.EventDeviceSwitch {
			if ctx.ActiveDevice == b.DeviceID && ctx.AfterDevice != b.DeviceID && active {
				o.add(e.Seq, e.TimeMS, model.StateReleased, int64Ptr(lastValue), nil, nil)
				active = false
			}
			continue
		}
		if ctx.ActiveDevice != b.DeviceID || e.Kind != model.EventAxis || e.Axis != b.Axis {
			continue
		}
		for obsIndex < len(observations) && observations[obsIndex].EventSeq < e.Seq {
			obsIndex++
		}
		if obsIndex >= len(observations) || observations[obsIndex].EventSeq != e.Seq {
			return nil, model.Schema("axis", b.Axis, fmt.Sprintf("axis observation missing for event %d", e.Seq))
		}
		obs := observations[obsIndex]
		obsIndex++
		value := fixed.EvalCurve(*c, obs.BaseNormalized)
		lastValue = value
		curveErr := fixed.CurveLinearizationBound(*c, obs.BaseNormalized)
		residual := obs.Residual
		o.out.AxisSamples = append(o.out.AxisSamples, AxisSample{
			EventSeq: e.Seq, TimeMS: e.TimeMS, DeviceID: b.DeviceID, BindingID: b.ID,
			ActionID: b.Action, AxisID: b.Axis, Raw: obs.Raw,
			BaseNormalized: obs.BaseNormalized, Normalized: value,
			Residual: residual, ResidualBound: 1,
			DeadzoneInside: obs.DeadzoneInside, DeadzoneMargin: obs.DeadzoneMargin,
			CurveError: curveErr,
		})
		if obs.DeadzoneInside {
			if active {
				o.add(e.Seq, e.TimeMS, model.StateReleased, int64Ptr(value), &residual, &curveErr)
				active = false
			}
			continue
		}
		state := model.StateHeld
		if !active {
			state = model.StatePressed
			active = true
		}
		o.add(e.Seq, e.TimeMS, state, int64Ptr(value), &residual, &curveErr)
	}
	return o.out, nil
}

func computeUnmapped(s *model.Scenario, contexts []eventContext) ([]model.TruthUnmapped, error) {
	axisMapped := map[string]map[string]bool{}
	buttonMapped := map[string]map[string]bool{}
	for _, b := range s.Bindings {
		if axisMapped[b.DeviceID] == nil {
			axisMapped[b.DeviceID] = map[string]bool{}
			buttonMapped[b.DeviceID] = map[string]bool{}
		}
		switch b.Type {
		case model.BindingAxis:
			axisMapped[b.DeviceID][b.Axis] = true
		case model.BindingButton, model.BindingLongPress, model.BindingDoublePress:
			buttonMapped[b.DeviceID][b.Button] = true
		case model.BindingCombo:
			for _, button := range b.Buttons {
				buttonMapped[b.DeviceID][button] = true
			}
		}
	}
	out := make([]model.TruthUnmapped, 0)
	for _, ctx := range contexts {
		e := ctx.Event
		switch e.Kind {
		case model.EventAxis:
			if !axisMapped[ctx.ActiveDevice][e.Axis] {
				out = append(out, model.TruthUnmapped{
					EventSeq: e.Seq, TimeMS: e.TimeMS, DeviceID: ctx.ActiveDevice,
					Control: e.Axis, Kind: "axis",
				})
			}
		case model.EventButtonDown, model.EventButtonUp, model.EventButtonRepeat:
			if !buttonMapped[ctx.ActiveDevice][e.Button] {
				out = append(out, model.TruthUnmapped{
					EventSeq: e.Seq, TimeMS: e.TimeMS, DeviceID: ctx.ActiveDevice,
					Control: e.Button, Kind: string(e.Kind),
				})
			}
		}
	}
	return out, nil
}

func findDevice(s *model.Scenario, id string) (*model.Device, bool) {
	for i := range s.Devices {
		if s.Devices[i].ID == id {
			return &s.Devices[i], true
		}
	}
	return nil, false
}

func findAxis(d *model.Device, id string) (*model.AxisSpec, bool) {
	for i := range d.Axes {
		if d.Axes[i].ID == id {
			return &d.Axes[i], true
		}
	}
	return nil, false
}

func findCurve(s *model.Scenario, id string) (*model.Curve, bool) {
	for i := range s.Curves {
		if s.Curves[i].ID == id {
			return &s.Curves[i], true
		}
	}
	return nil, false
}

func containsString(values []string, wanted string) bool {
	for _, v := range values {
		if v == wanted {
			return true
		}
	}
	return false
}

func comboBounds(times map[string]int64) (int64, int64) {
	first := int64(1 << 62)
	last := int64(-1 << 62)
	for _, t := range times {
		if t < first {
			first = t
		}
		if t > last {
			last = t
		}
	}
	return first, last
}

func int64Ptr(v int64) *int64 {
	return &v
}

func SortOutputs(in []ActionOutput) []ActionOutput {
	out := append([]ActionOutput(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].EventSeq != out[j].EventSeq {
			return out[i].EventSeq < out[j].EventSeq
		}
		if out[i].BindingID != out[j].BindingID {
			return out[i].BindingID < out[j].BindingID
		}
		return out[i].Ordinal < out[j].Ordinal
	})
	return out
}

func SortAxisSamples(in []AxisSample) []AxisSample {
	out := append([]AxisSample(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].EventSeq != out[j].EventSeq {
			return out[i].EventSeq < out[j].EventSeq
		}
		return out[i].BindingID < out[j].BindingID
	})
	return out
}

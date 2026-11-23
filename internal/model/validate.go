package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var identifierRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ValidatePresence checks keys that cannot be distinguished from Go zero values.
func ValidatePresence(raw []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return Schema("$", "", "invalid JSON: "+err.Error())
	}
	if err := requireKeys(top, "$", "schema_version", "id", "name", "seed", "fixed_precision_version", "binding_version", "devices", "initial_device_id", "curves", "bindings", "events", "truth"); err != nil {
		return err
	}
	if err := forEachObject(top["devices"], "$.devices", func(i int, obj map[string]json.RawMessage) error {
		if err := requireKeys(obj, fmt.Sprintf("$.devices[%d]", i), "id", "name", "fixed_scale", "axes", "buttons"); err != nil {
			return err
		}
		if err := forEachObject(obj["axes"], fmt.Sprintf("$.devices[%d].axes", i), func(j int, axis map[string]json.RawMessage) error {
			return requireKeys(axis, fmt.Sprintf("$.devices[%d].axes[%d]", i, j), "id", "min", "max", "bipolar", "direction", "invert", "deadzone")
		}); err != nil {
			return err
		}
		return forEachObject(obj["buttons"], fmt.Sprintf("$.devices[%d].buttons", i), func(j int, button map[string]json.RawMessage) error {
			return requireKeys(button, fmt.Sprintf("$.devices[%d].buttons[%d]", i, j), "id")
		})
	}); err != nil {
		return err
	}
	if err := forEachObject(top["curves"], "$.curves", func(i int, curve map[string]json.RawMessage) error {
		if err := requireKeys(curve, fmt.Sprintf("$.curves[%d]", i), "id", "kind", "domain_scale", "curvature_ppm", "points"); err != nil {
			return err
		}
		return forEachObject(curve["points"], fmt.Sprintf("$.curves[%d].points", i), func(j int, point map[string]json.RawMessage) error {
			return requireKeys(point, fmt.Sprintf("$.curves[%d].points[%d]", i, j), "x", "y")
		})
	}); err != nil {
		return err
	}
	if err := forEachObject(top["bindings"], "$.bindings", func(i int, binding map[string]json.RawMessage) error {
		path := fmt.Sprintf("$.bindings[%d]", i)
		if err := requireKeys(binding, path, "id", "action", "type", "device_id", "timing"); err != nil {
			return err
		}
		var typ BindingType
		if raw, ok := binding["type"]; ok {
			_ = json.Unmarshal(raw, &typ)
		}
		switch typ {
		case BindingAxis:
			if err := requireKeys(binding, path, "axis", "curve"); err != nil {
				return err
			}
		case BindingButton, BindingLongPress, BindingDoublePress:
			if err := requireKeys(binding, path, "button"); err != nil {
				return err
			}
		case BindingCombo:
			if err := requireKeys(binding, path, "buttons"); err != nil {
				return err
			}
		}
		var timing map[string]json.RawMessage
		if err := json.Unmarshal(binding["timing"], &timing); err != nil || timing == nil {
			return Schema(path+".timing", "", "timing object is missing or invalid")
		}
		switch typ {
		case BindingCombo, BindingDoublePress:
			if err := requireKeys(timing, path+".timing", "max_window_ms", "window_ms"); err != nil {
				return err
			}
			if typ == BindingDoublePress {
				if err := requireKeys(timing, path+".timing", "min_gap_ms"); err != nil {
					return err
				}
			}
		case BindingLongPress:
			if err := requireKeys(timing, path+".timing", "threshold_ms", "max_threshold_ms"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := forEachObject(top["events"], "$.events", func(i int, event map[string]json.RawMessage) error {
		path := fmt.Sprintf("$.events[%d]", i)
		if err := requireKeys(event, path, "seq", "time_ms", "kind"); err != nil {
			return err
		}
		var kind EventKind
		if raw, ok := event["kind"]; ok {
			_ = json.Unmarshal(raw, &kind)
		}
		switch kind {
		case EventAxis:
			if err := requireKeys(event, path, "axis", "value"); err != nil {
				return err
			}
		case EventButtonDown, EventButtonUp, EventButtonRepeat:
			if err := requireKeys(event, path, "button"); err != nil {
				return err
			}
		case EventDeviceSwitch:
			return requireKeys(event, path, "device")
		}
		return nil
	}); err != nil {
		return err
	}
	var truth map[string]json.RawMessage
	if err := json.Unmarshal(top["truth"], &truth); err != nil {
		return Schema("$.truth", "", "truth must be an object")
	}
	if err := requireKeys(truth, "$.truth", "axis_samples", "actions", "unmapped"); err != nil {
		return err
	}
	if err := forEachObject(truth["axis_samples"], "$.truth.axis_samples", func(i int, obj map[string]json.RawMessage) error {
		path := fmt.Sprintf("$.truth.axis_samples[%d]", i)
		if err := requireKeys(obj, path, "event_seq", "time_ms", "device_id", "binding_id", "action_id", "axis_id", "raw", "normalized", "quantization_residual", "quantization_error_bound", "deadzone_inside", "deadzone_margin"); err != nil {
			return err
		}
		var residual map[string]json.RawMessage
		if err := json.Unmarshal(obj["quantization_residual"], &residual); err != nil {
			return Schema(path+".quantization_residual", "", "residual must be an object")
		}
		return requireKeys(residual, path+".quantization_residual", "num", "den")
	}); err != nil {
		return err
	}
	if err := forEachObject(truth["actions"], "$.truth.actions", func(i int, obj map[string]json.RawMessage) error {
		return requireKeys(obj, fmt.Sprintf("$.truth.actions[%d]", i), "binding_id", "action_id", "event_seq", "trigger_time_ms", "state")
	}); err != nil {
		return err
	}
	return forEachObject(truth["unmapped"], "$.truth.unmapped", func(i int, obj map[string]json.RawMessage) error {
		return requireKeys(obj, fmt.Sprintf("$.truth.unmapped[%d]", i), "event_seq", "time_ms", "device_id", "control", "kind")
	})
}

func requireKeys(obj map[string]json.RawMessage, path string, keys ...string) error {
	for _, key := range keys {
		if _, ok := obj[key]; !ok {
			return Schema(path+"."+key, "", "required field is missing")
		}
	}
	return nil
}

func forEachObject(raw json.RawMessage, path string, fn func(int, map[string]json.RawMessage) error) error {
	var objects []json.RawMessage
	if err := json.Unmarshal(raw, &objects); err != nil {
		return Schema(path, "", "expected an array")
	}
	for i, item := range objects {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			return Schema(fmt.Sprintf("%s[%d]", path, i), "", "expected an object")
		}
		if err := fn(i, obj); err != nil {
			return err
		}
	}
	return nil
}

func DecodeScenario(raw []byte) (*Scenario, error) {
	if err := ValidatePresence(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var scenario Scenario
	if err := dec.Decode(&scenario); err != nil {
		return nil, Schema("$", "", "strict decode failed: "+err.Error())
	}
	if err := ValidateScenario(&scenario); err != nil {
		return nil, err
	}
	return &scenario, nil
}

func DecodeManifest(raw []byte) (*Manifest, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, Schema("$", "", "invalid manifest JSON: "+err.Error())
	}
	if err := requireKeys(top, "$", "schema_version", "seed", "scenarios"); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var manifest Manifest
	if err := dec.Decode(&manifest); err != nil {
		return nil, Schema("$", "", "strict manifest decode failed: "+err.Error())
	}
	if manifest.SchemaVersion != SchemaVersion {
		return nil, Schema("$.schema_version", "", fmt.Sprintf("unsupported schema version %d", manifest.SchemaVersion))
	}
	if len(manifest.Scenarios) == 0 {
		return nil, Schema("$.scenarios", "", "at least one scenario is required")
	}
	seen := map[string]bool{}
	for i, entry := range manifest.Scenarios {
		if entry.ID == "" || entry.File == "" {
			return nil, Schema(fmt.Sprintf("$.scenarios[%d]", i), "", "id and file are required")
		}
		if seen[entry.ID] {
			return nil, Schema(fmt.Sprintf("$.scenarios[%d].id", i), entry.ID, "duplicate scenario id")
		}
		seen[entry.ID] = true
	}
	return &manifest, nil
}

func ValidateScenario(s *Scenario) error {
	if s.SchemaVersion != SchemaVersion {
		return Schema("$.schema_version", "", fmt.Sprintf("unsupported schema version %d", s.SchemaVersion))
	}
	if err := validID("$.id", s.ID); err != nil {
		return err
	}
	if s.Name == "" {
		return Schema("$.name", "", "name is required")
	}
	if s.FixedPrecisionVersion == "" {
		return Schema("$.fixed_precision_version", "", "fixed precision version is required")
	}
	if s.BindingVersion == "" {
		return Schema("$.binding_version", "", "binding version is required")
	}
	if len(s.Devices) == 0 {
		return Schema("$.devices", "", "at least one device is required")
	}
	devices := map[string]*Device{}
	for i := range s.Devices {
		d := &s.Devices[i]
		path := fmt.Sprintf("$.devices[%d]", i)
		if err := validateDevice(d, path); err != nil {
			return err
		}
		if _, exists := devices[d.ID]; exists {
			return Schema(path+".id", d.ID, "duplicate device id")
		}
		devices[d.ID] = d
	}
	if _, ok := devices[s.InitialDeviceID]; !ok {
		return Schema("$.initial_device_id", s.InitialDeviceID, "initial device is not declared")
	}
	curves := map[string]*Curve{}
	for i := range s.Curves {
		c := &s.Curves[i]
		path := fmt.Sprintf("$.curves[%d]", i)
		if err := validateCurve(c, path); err != nil {
			return err
		}
		if _, exists := curves[c.ID]; exists {
			return Schema(path+".id", c.ID, "duplicate curve id")
		}
		curves[c.ID] = c
	}
	bindings := map[string]*Binding{}
	actions := map[string]bool{}
	for i := range s.Bindings {
		b := &s.Bindings[i]
		path := fmt.Sprintf("$.bindings[%d]", i)
		if err := validateBinding(b, path, devices, curves); err != nil {
			return err
		}
		if _, exists := bindings[b.ID]; exists {
			return Schema(path+".id", b.ID, "duplicate binding id")
		}
		if actions[b.Action] {
			return Schema(path+".action", b.Action, "duplicate action id")
		}
		bindings[b.ID] = b
		actions[b.Action] = true
	}
	if err := validateEvents(s, devices); err != nil {
		return err
	}
	if err := validateTruth(&s.Truth, s.Events, bindings); err != nil {
		return err
	}
	return ValidateParameters(s)
}

func validID(path, id string) error {
	if id == "" {
		return Schema(path, "", "identifier is required")
	}
	if !identifierRE.MatchString(id) {
		return Schema(path, id, "identifier may contain only letters, digits, dot, underscore and dash")
	}
	return nil
}

func validateDevice(d *Device, path string) error {
	if err := validID(path+".id", d.ID); err != nil {
		return err
	}
	if d.Name == "" {
		return Schema(path+".name", "", "device name is required")
	}
	if d.FixedScale <= 0 || d.FixedScale > 1_000_000_000 {
		return Schema(path+".fixed_scale", d.FixedScale, "fixed scale must be in [1, 1000000000]")
	}
	if len(d.Axes) == 0 {
		return Schema(path+".axes", "", "device must declare at least one axis")
	}
	axes := map[string]bool{}
	buttons := map[string]bool{}
	for i, a := range d.Axes {
		ap := fmt.Sprintf("%s.axes[%d]", path, i)
		if err := validID(ap+".id", a.ID); err != nil {
			return err
		}
		if axes[a.ID] {
			return Schema(ap+".id", a.ID, "duplicate axis identifier")
		}
		axes[a.ID] = true
		if a.Min >= a.Max {
			return Schema(ap+".min", a.Min, "axis min must be less than max")
		}
		if a.Min < -1_000_000_000 || a.Max > 1_000_000_000 {
			return Schema(ap+".min", map[string]int64{"min": a.Min, "max": a.Max}, "raw axis range must stay within [-1000000000, 1000000000] to keep fixed-point arithmetic exact")
		}
		if a.Deadzone < 0 || a.Deadzone > d.FixedScale {
			return Schema(ap+".deadzone", a.Deadzone, "deadzone must be in [0, fixed_scale]")
		}
		switch a.Direction {
		case "both":
			if !a.Bipolar {
				return Schema(ap+".direction", a.Direction, "direction both requires bipolar=true")
			}
		case "positive", "negative":
			if a.Bipolar {
				return Schema(ap+".direction", a.Direction, "bipolar axes require direction=both")
			}
		default:
			return Schema(ap+".direction", a.Direction, "direction must be both, positive or negative")
		}
	}
	for i, b := range d.Buttons {
		bp := fmt.Sprintf("%s.buttons[%d]", path, i)
		if err := validID(bp+".id", b.ID); err != nil {
			return err
		}
		if buttons[b.ID] {
			return Schema(bp+".id", b.ID, "duplicate button identifier")
		}
		if axes[b.ID] {
			return Schema(bp+".id", b.ID, "button identifier conflicts with axis identifier")
		}
		buttons[b.ID] = true
	}
	return nil
}

func validateCurve(c *Curve, path string) error {
	if err := validID(path+".id", c.ID); err != nil {
		return err
	}
	if c.Kind != "piecewise_linear" {
		return Schema(path+".kind", c.Kind, "supported curve kind is piecewise_linear")
	}
	if c.DomainScale <= 0 || c.DomainScale > 1_000_000_000 {
		return Schema(path+".domain_scale", c.DomainScale, "domain scale must be in [1, 1000000000]")
	}
	if len(c.Points) < 2 {
		return Schema(path+".points", len(c.Points), "at least two curve points are required")
	}
	lastX := int64(0)
	for i, p := range c.Points {
		pp := fmt.Sprintf("%s.points[%d]", path, i)
		if p.X < -c.DomainScale || p.X > c.DomainScale {
			return Schema(pp+".x", p.X, "curve x is outside its domain")
		}
		if p.Y < -c.DomainScale || p.Y > c.DomainScale {
			return Schema(pp+".y", p.Y, "curve y is outside its domain")
		}
		if i > 0 && p.X <= lastX {
			return Schema(pp+".x", p.X, "curve x values must be strictly increasing")
		}
		lastX = p.X
	}
	if c.CurvaturePPM < 0 {
		return Schema(path+".curvature_ppm", c.CurvaturePPM, "curvature bound must be non-negative")
	}
	return nil
}

func validateBinding(b *Binding, path string, devices map[string]*Device, curves map[string]*Curve) error {
	if err := validID(path+".id", b.ID); err != nil {
		return err
	}
	if b.Action == "" {
		return Schema(path+".action", "", "action id is required")
	}
	d, ok := devices[b.DeviceID]
	if !ok {
		return Schema(path+".device_id", b.DeviceID, "binding references an undeclared device")
	}
	axisSet := map[string]bool{}
	for _, a := range d.Axes {
		axisSet[a.ID] = true
	}
	buttonSet := map[string]bool{}
	for _, button := range d.Buttons {
		buttonSet[button.ID] = true
	}
	switch b.Type {
	case BindingAxis:
		if !axisSet[b.Axis] {
			return Schema(path+".axis", b.Axis, "axis is not declared by the binding device")
		}
		c, ok := curves[b.Curve]
		if !ok {
			return Schema(path+".curve", b.Curve, "curve is not declared")
		}
		if c.DomainScale != d.FixedScale {
			return Schema(path+".curve", b.Curve, "curve domain_scale must equal the device fixed_scale")
		}
	case BindingButton, BindingLongPress, BindingDoublePress:
		if !buttonSet[b.Button] {
			return Schema(path+".button", b.Button, "button is not declared by the binding device")
		}
	case BindingCombo:
		if len(b.Buttons) < 2 {
			return Schema(path+".buttons", len(b.Buttons), "combo requires at least two buttons")
		}
		seen := map[string]bool{}
		for _, button := range b.Buttons {
			if !buttonSet[button] {
				return Schema(path+".buttons", button, "combo member is not declared by the binding device")
			}
			if seen[button] {
				return Schema(path+".buttons", button, "duplicate combo member")
			}
			seen[button] = true
		}
	default:
		return Schema(path+".type", b.Type, "unknown binding type")
	}
	return nil
}

func validateEvents(s *Scenario, devices map[string]*Device) error {
	lastTime := int64(-1 << 62)
	lastSeq := -1
	seqs := map[int]bool{}
	currentDevice := s.InitialDeviceID
	for i := range s.Events {
		e := &s.Events[i]
		path := fmt.Sprintf("$.events[%d]", i)
		if e.Seq <= lastSeq {
			return Schema(path+".seq", e.Seq, "event sequence numbers must strictly increase")
		}
		lastSeq = e.Seq
		seqs[e.Seq] = true
		if e.TimeMS < lastTime {
			return Schema(path+".time_ms", e.TimeMS, "event logical timestamps must be non-decreasing")
		}
		lastTime = e.TimeMS
		d := devices[currentDevice]
		switch e.Kind {
		case EventAxis:
			a, ok := findAxis(d, e.Axis)
			if !ok {
				return Schema(path+".axis", e.Axis, "axis is not declared by the active device "+currentDevice)
			}
			if e.Value < a.Min || e.Value > a.Max {
				return Schema(path+".value", e.Value, fmt.Sprintf("axis value is outside [%d, %d]", a.Min, a.Max))
			}
		case EventButtonDown, EventButtonUp, EventButtonRepeat:
			if !hasButton(d, e.Button) {
				return Schema(path+".button", e.Button, "button is not declared by the active device "+currentDevice)
			}
		case EventDeviceSwitch:
			if _, ok := devices[e.Device]; !ok {
				return Schema(path+".device", e.Device, "device switch references an undeclared device")
			}
			currentDevice = e.Device
		default:
			return Schema(path+".kind", e.Kind, "unknown event kind")
		}
	}
	return nil
}

func validateTruth(t *Truth, events []Event, bindings map[string]*Binding) error {
	seqs := map[int]bool{}
	for _, e := range events {
		seqs[e.Seq] = true
	}
	for i, sample := range t.AxisSamples {
		path := fmt.Sprintf("$.truth.axis_samples[%d]", i)
		if !seqs[sample.EventSeq] {
			return Schema(path+".event_seq", sample.EventSeq, "truth axis sample references an unknown event")
		}
		b, ok := bindings[sample.BindingID]
		if !ok || b.Type != BindingAxis {
			return Schema(path+".binding_id", sample.BindingID, "truth axis sample must reference an axis binding")
		}
		if sample.Residual.Den <= 0 {
			return Schema(path+".quantization_residual.den", sample.Residual.Den, "residual denominator must be positive")
		}
		if sample.ResidualBound <= 0 {
			return Schema(path+".quantization_error_bound", sample.ResidualBound, "quantization error bound must be positive")
		}
	}
	for i, action := range t.Actions {
		path := fmt.Sprintf("$.truth.actions[%d]", i)
		if !seqs[action.EventSeq] {
			return Schema(path+".event_seq", action.EventSeq, "truth action references an unknown event")
		}
		b, ok := bindings[action.BindingID]
		if !ok {
			return Schema(path+".binding_id", action.BindingID, "truth action references an unknown binding")
		}
		if action.ActionID != b.Action {
			return Schema(path+".action_id", action.ActionID, "truth action id does not match its binding")
		}
		switch action.State {
		case StatePressed, StateHeld, StateReleased:
		default:
			return Schema(path+".state", action.State, "unknown action state")
		}
	}
	for i, u := range t.Unmapped {
		path := fmt.Sprintf("$.truth.unmapped[%d]", i)
		if !seqs[u.EventSeq] {
			return Schema(path+".event_seq", u.EventSeq, "truth unmapped entry references an unknown event")
		}
	}
	return nil
}

func ValidateParameters(s *Scenario) error {
	for i := range s.Bindings {
		b := &s.Bindings[i]
		path := fmt.Sprintf("bindings[%d]", i)
		switch b.Type {
		case BindingCombo, BindingDoublePress:
			if b.Timing.MaxWindowMS <= 0 {
				return Param(path+".timing.max_window_ms", b.Timing.MaxWindowMS, "max_window_ms must be positive")
			}
			if b.Timing.WindowMS <= 0 {
				return Param(path+".timing.window_ms", b.Timing.WindowMS, "window_ms must be positive")
			}
			if b.Timing.WindowMS > b.Timing.MaxWindowMS {
				return Param(path+".timing.window_ms", map[string]int64{"window_ms": b.Timing.WindowMS, "max_window_ms": b.Timing.MaxWindowMS}, "time window exceeds its declared limit")
			}
			if b.Type == BindingDoublePress {
				if b.Timing.MinGapMS < 0 {
					return Param(path+".timing.min_gap_ms", b.Timing.MinGapMS, "min_gap_ms must be non-negative")
				}
				if b.Timing.MinGapMS > b.Timing.WindowMS {
					return Param(path+".timing.min_gap_ms", map[string]int64{"min_gap_ms": b.Timing.MinGapMS, "window_ms": b.Timing.WindowMS}, "minimum gap exceeds double-press window (inverted threshold)")
				}
			}
		case BindingLongPress:
			if b.Timing.ThresholdMS <= 0 {
				return Param(path+".timing.threshold_ms", b.Timing.ThresholdMS, "threshold_ms must be positive")
			}
			if b.Timing.MaxThresholdMS <= 0 {
				return Param(path+".timing.max_threshold_ms", b.Timing.MaxThresholdMS, "max_threshold_ms must be positive")
			}
			if b.Timing.ThresholdMS > b.Timing.MaxThresholdMS {
				return Param(path+".timing.threshold_ms", map[string]int64{"threshold_ms": b.Timing.ThresholdMS, "max_threshold_ms": b.Timing.MaxThresholdMS}, "long-press threshold is inverted")
			}
		}
	}
	return nil
}

func findAxis(d *Device, id string) (*AxisSpec, bool) {
	for i := range d.Axes {
		if d.Axes[i].ID == id {
			return &d.Axes[i], true
		}
	}
	return nil, false
}

func hasButton(d *Device, id string) bool {
	for _, b := range d.Buttons {
		if b.ID == id {
			return true
		}
	}
	return false
}

func SortedActions(actions []TruthAction) []TruthAction {
	out := append([]TruthAction(nil), actions...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].EventSeq != out[j].EventSeq {
			return out[i].EventSeq < out[j].EventSeq
		}
		return strings.Compare(out[i].BindingID, out[j].BindingID) < 0
	})
	return out
}

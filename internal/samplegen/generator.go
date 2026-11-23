package samplegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"inputkernel/internal/model"
)

const DefaultSeed int64 = 20261124

type generator struct {
	seed int64
	rng  rand64
}

type rand64 struct{ state uint64 }

func newRand(seed int64) rand64 {
	state := uint64(seed)
	if state == 0 {
		state = 0x9e3779b97f4a7c15
	}
	return rand64{state: state}
}

func (r *rand64) next() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 2685821657736338717
}

func (r *rand64) rangeInt(lo, hi int64) int64 {
	if hi <= lo {
		return lo
	}
	return lo + int64(r.next()%uint64(hi-lo+1))
}

func (g *generator) jitter(value, spread int64) int64 {
	if spread <= 0 {
		return value
	}
	return value + g.rng.rangeInt(-spread, spread)
}

func Generate(seed int64) ([]*model.Scenario, error) {
	g := &generator{seed: seed, rng: newRand(seed ^ 0x5deece66d)}
	scenarios := []*model.Scenario{
		g.dualStickScenario(),
		g.singleStickScenario(),
		g.triggerGyroScenario(),
		g.hotSwitchScenario(),
	}
	for _, scenario := range scenarios {
		if err := fillTruth(scenario); err != nil {
			return nil, err
		}
		if err := model.ValidateScenario(scenario); err != nil {
			return nil, fmt.Errorf("%s: %w", scenario.ID, err)
		}
	}
	return scenarios, nil
}

func Write(dir string, seed int64) (*model.Manifest, error) {
	scenarios, err := Generate(seed)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	manifest := &model.Manifest{SchemaVersion: model.SchemaVersion, Seed: seed}
	for _, scenario := range scenarios {
		file := scenario.ID + ".json"
		data, err := json.MarshalIndent(scenario, "", "  ")
		if err != nil {
			return nil, err
		}
		data = append(data, '\n')
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o644); err != nil {
			return nil, err
		}
		manifest.Scenarios = append(manifest.Scenarios, model.ManifestScenario{ID: scenario.ID, File: file})
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func (g *generator) dualStickScenario() *model.Scenario {
	scale := int64(10000)
	device := model.Device{
		ID: "dual_pad", Name: "Dual stick deterministic pad", FixedScale: scale,
		Axes: []model.AxisSpec{
			{ID: "left_x", Min: -32768, Max: 32767, Bipolar: true, Direction: "both", Deadzone: 1200},
			{ID: "left_y", Min: -32768, Max: 32767, Bipolar: true, Direction: "both", Deadzone: 1200},
			{ID: "right_x", Min: -32768, Max: 32767, Bipolar: true, Direction: "both", Invert: true, Deadzone: 900},
		},
		Buttons: []model.ButtonSpec{{ID: "A"}, {ID: "B"}, {ID: "X"}, {ID: "Y"}, {ID: "LB"}, {ID: "RB"}, {ID: "START"}, {ID: "BACK"}},
	}
	curves := []model.Curve{
		{ID: "aim", Kind: "piecewise_linear", DomainScale: scale, CurvaturePPM: 90000, Points: []model.CurvePoint{
			{X: -10000, Y: -7000}, {X: -4000, Y: -3000}, {X: 0, Y: 0}, {X: 4000, Y: 3000}, {X: 10000, Y: 7000},
		}},
		{ID: "linear", Kind: "piecewise_linear", DomainScale: scale, CurvaturePPM: 10000, Points: []model.CurvePoint{
			{X: -10000, Y: -10000}, {X: 0, Y: 0}, {X: 10000, Y: 10000},
		}},
	}
	bindings := []model.Binding{
		axisBinding("b_move_x", "MoveX", "dual_pad", "left_x", "aim"),
		axisBinding("b_move_y", "MoveY", "dual_pad", "left_y", "aim"),
		axisBinding("b_look_x", "LookX", "dual_pad", "right_x", "linear"),
		{ID: "b_jump", Action: "Jump", Type: model.BindingButton, DeviceID: "dual_pad", Button: "A"},
		{ID: "b_overdrive", Action: "Overdrive", Type: model.BindingCombo, DeviceID: "dual_pad", Buttons: []string{"LB", "RB"},
			Timing: model.TimingSpec{WindowMS: 80, MaxWindowMS: 500}},
		{ID: "b_charge", Action: "Charge", Type: model.BindingLongPress, DeviceID: "dual_pad", Button: "X",
			Timing: model.TimingSpec{ThresholdMS: 120, MaxThresholdMS: 500}},
		{ID: "b_dash", Action: "Dash", Type: model.BindingDoublePress, DeviceID: "dual_pad", Button: "Y",
			Timing: model.TimingSpec{WindowMS: 200, MinGapMS: 25, MaxWindowMS: 500}},
	}
	events := []model.Event{
		axisEvent(1, 0, "left_x", 0),
		buttonEvent(2, 10, model.EventButtonDown, "A"),
		buttonEvent(3, 20, model.EventButtonRepeat, "A"),
		axisEvent(4, 40, "left_x", g.jitter(3500, 120)),
		axisEvent(5, 60, "left_y", g.jitter(-8000, 250)),
		buttonEvent(6, 80, model.EventButtonDown, "LB"),
		buttonEvent(7, 100, model.EventButtonDown, "RB"),
		axisEvent(8, 120, "right_x", g.jitter(15000, 300)),
		buttonEvent(9, 130, model.EventButtonRepeat, "RB"),
		buttonEvent(10, 150, model.EventButtonDown, "X"),
		buttonEvent(11, 280, model.EventButtonRepeat, "X"),
		buttonEvent(12, 300, model.EventButtonUp, "LB"),
		buttonEvent(13, 320, model.EventButtonUp, "RB"),
		buttonEvent(14, 340, model.EventButtonUp, "X"),
		buttonEvent(15, 360, model.EventButtonUp, "A"),
		axisEvent(16, 380, "left_x", g.jitter(-9000, 300)),
		axisEvent(17, 400, "left_y", 0),
		buttonEvent(18, 430, model.EventButtonDown, "Y"),
		buttonEvent(19, 460, model.EventButtonUp, "Y"),
		buttonEvent(20, 500, model.EventButtonDown, "Y"),
		buttonEvent(21, 530, model.EventButtonUp, "Y"),
		axisEvent(22, 560, "right_x", 0),
		buttonEvent(23, 600, model.EventButtonDown, "X"),
		buttonEvent(24, 650, model.EventButtonUp, "X"),
		axisEvent(25, 700, "left_x", 0),
	}
	return &model.Scenario{
		SchemaVersion: model.SchemaVersion, ID: "dual_stick", Name: "Dual stick with combo, long press and double press",
		Seed: g.seed, FixedPrecisionVersion: "fx-1e4-v1", BindingVersion: "bind-v1",
		Devices: []model.Device{device}, InitialDeviceID: "dual_pad",
		Curves: curves, Bindings: bindings, Events: events,
	}
}

func (g *generator) singleStickScenario() *model.Scenario {
	scale := int64(1000)
	device := model.Device{
		ID: "single_pad", Name: "Single stick with positive trigger", FixedScale: scale,
		Axes: []model.AxisSpec{
			{ID: "stick", Min: -1000, Max: 1000, Bipolar: true, Direction: "both", Deadzone: 80},
			{ID: "trigger", Min: 0, Max: 255, Bipolar: false, Direction: "positive", Deadzone: 80},
		},
		Buttons: []model.ButtonSpec{{ID: "A"}, {ID: "B"}, {ID: "X"}, {ID: "Y"}},
	}
	curves := []model.Curve{
		{ID: "combat", Kind: "piecewise_linear", DomainScale: scale, CurvaturePPM: 70000, Points: []model.CurvePoint{
			{X: -1000, Y: -700}, {X: -300, Y: -250}, {X: 0, Y: 0}, {X: 300, Y: 250}, {X: 1000, Y: 700},
		}},
		{ID: "trigger", Kind: "piecewise_linear", DomainScale: scale, CurvaturePPM: 40000, Points: []model.CurvePoint{
			{X: 0, Y: 0}, {X: 500, Y: 400}, {X: 1000, Y: 1000},
		}},
	}
	bindings := []model.Binding{
		axisBinding("b_look", "Look", "single_pad", "stick", "combat"),
		axisBinding("b_throttle", "Throttle", "single_pad", "trigger", "trigger"),
		{ID: "b_fire", Action: "Fire", Type: model.BindingButton, DeviceID: "single_pad", Button: "A"},
		{ID: "b_boost", Action: "Boost", Type: model.BindingLongPress, DeviceID: "single_pad", Button: "B",
			Timing: model.TimingSpec{ThresholdMS: 100, MaxThresholdMS: 400}},
		{ID: "b_dodge", Action: "Dodge", Type: model.BindingDoublePress, DeviceID: "single_pad", Button: "X",
			Timing: model.TimingSpec{WindowMS: 120, MinGapMS: 10, MaxWindowMS: 300}},
	}
	events := []model.Event{
		axisEvent(1, 0, "stick", 0),
		axisEvent(2, 20, "trigger", 0),
		buttonEvent(3, 40, model.EventButtonDown, "A"),
		buttonEvent(4, 70, model.EventButtonRepeat, "A"),
		axisEvent(5, 100, "stick", g.jitter(600, 30)),
		buttonEvent(6, 120, model.EventButtonUp, "A"),
		axisEvent(7, 140, "trigger", g.jitter(180, 3)),
		axisEvent(8, 180, "trigger", 255),
		buttonEvent(9, 200, model.EventButtonDown, "B"),
		buttonEvent(10, 330, model.EventButtonRepeat, "B"),
		buttonEvent(11, 360, model.EventButtonUp, "B"),
		axisEvent(12, 380, "stick", -1000),
		buttonEvent(13, 420, model.EventButtonDown, "X"),
		buttonEvent(14, 440, model.EventButtonUp, "X"),
		buttonEvent(15, 460, model.EventButtonDown, "X"),
		buttonEvent(16, 480, model.EventButtonUp, "X"),
		axisEvent(17, 520, "trigger", 0),
		axisEvent(18, 560, "stick", 0),
	}
	return &model.Scenario{
		SchemaVersion: model.SchemaVersion, ID: "single_stick", Name: "Single stick and unipolar trigger",
		Seed: g.seed, FixedPrecisionVersion: "fx-1e3-v1", BindingVersion: "bind-v1",
		Devices: []model.Device{device}, InitialDeviceID: "single_pad",
		Curves: curves, Bindings: bindings, Events: events,
	}
}

func (g *generator) triggerGyroScenario() *model.Scenario {
	scale := int64(10000)
	device := model.Device{
		ID: "gyro_pad", Name: "Trigger and gyro variant", FixedScale: scale,
		Axes: []model.AxisSpec{
			{ID: "lt", Min: 0, Max: 255, Bipolar: false, Direction: "positive", Deadzone: 500},
			{ID: "rt", Min: 0, Max: 255, Bipolar: false, Direction: "negative", Deadzone: 500},
			{ID: "gyro_x", Min: -32768, Max: 32767, Bipolar: true, Direction: "both", Invert: true, Deadzone: 300},
		},
		Buttons: []model.ButtonSpec{{ID: "A"}, {ID: "B"}, {ID: "GYRO"}},
	}
	curves := []model.Curve{
		{ID: "throttle", Kind: "piecewise_linear", DomainScale: scale, CurvaturePPM: 50000, Points: []model.CurvePoint{
			{X: 0, Y: 0}, {X: 5000, Y: 3500}, {X: 10000, Y: 10000},
		}},
		{ID: "brake", Kind: "piecewise_linear", DomainScale: scale, CurvaturePPM: 50000, Points: []model.CurvePoint{
			{X: -10000, Y: -10000}, {X: -5000, Y: -4000}, {X: 0, Y: 0},
		}},
		{ID: "gyro", Kind: "piecewise_linear", DomainScale: scale, CurvaturePPM: 120000, Points: []model.CurvePoint{
			{X: -10000, Y: -9000}, {X: -2000, Y: -1500}, {X: 0, Y: 0}, {X: 2000, Y: 1500}, {X: 10000, Y: 9000},
		}},
	}
	bindings := []model.Binding{
		axisBinding("b_accel", "Accelerate", "gyro_pad", "lt", "throttle"),
		axisBinding("b_brake", "Brake", "gyro_pad", "rt", "brake"),
		axisBinding("b_tilt", "Tilt", "gyro_pad", "gyro_x", "gyro"),
		{ID: "b_fire", Action: "Fire", Type: model.BindingButton, DeviceID: "gyro_pad", Button: "A"},
		{ID: "b_charge", Action: "Charge", Type: model.BindingLongPress, DeviceID: "gyro_pad", Button: "A",
			Timing: model.TimingSpec{ThresholdMS: 100, MaxThresholdMS: 500}},
		{ID: "b_recenter", Action: "Recenter", Type: model.BindingDoublePress, DeviceID: "gyro_pad", Button: "B",
			Timing: model.TimingSpec{WindowMS: 100, MinGapMS: 5, MaxWindowMS: 300}},
	}
	events := []model.Event{
		axisEvent(1, 0, "lt", 0),
		axisEvent(2, 20, "rt", 0),
		axisEvent(3, 40, "gyro_x", 0),
		axisEvent(4, 60, "lt", g.jitter(80, 3)),
		axisEvent(5, 100, "gyro_x", g.jitter(12000, 200)),
		buttonEvent(6, 140, model.EventButtonDown, "A"),
		buttonEvent(7, 200, model.EventButtonRepeat, "A"),
		buttonEvent(8, 260, model.EventButtonUp, "A"),
		axisEvent(9, 280, "lt", 255),
		axisEvent(10, 320, "rt", g.jitter(200, 3)),
		axisEvent(11, 360, "gyro_x", g.jitter(-9000, 200)),
		buttonEvent(12, 400, model.EventButtonDown, "B"),
		buttonEvent(13, 402, model.EventButtonUp, "B"),
		buttonEvent(14, 440, model.EventButtonDown, "B"),
		buttonEvent(15, 445, model.EventButtonUp, "B"),
		buttonEvent(16, 480, model.EventButtonDown, "B"),
		buttonEvent(17, 500, model.EventButtonUp, "B"),
		buttonEvent(18, 530, model.EventButtonDown, "B"),
		buttonEvent(19, 550, model.EventButtonUp, "B"),
		axisEvent(20, 580, "rt", 0),
		axisEvent(21, 600, "lt", 0),
		axisEvent(22, 620, "gyro_x", 0),
	}
	return &model.Scenario{
		SchemaVersion: model.SchemaVersion, ID: "trigger_gyro", Name: "Unipolar triggers, inversion and gyro curve",
		Seed: g.seed, FixedPrecisionVersion: "fx-1e4-v2", BindingVersion: "bind-v2",
		Devices: []model.Device{device}, InitialDeviceID: "gyro_pad",
		Curves: curves, Bindings: bindings, Events: events,
	}
}

func (g *generator) hotSwitchScenario() *model.Scenario {
	scale := int64(10000)
	oldPad := model.Device{
		ID: "pad_dual", Name: "Dual pad before switch", FixedScale: scale,
		Axes: []model.AxisSpec{
			{ID: "lx", Min: -32768, Max: 32767, Bipolar: true, Direction: "both", Deadzone: 800},
			{ID: "ly", Min: -32768, Max: 32767, Bipolar: true, Direction: "both", Deadzone: 800},
		},
		Buttons: []model.ButtonSpec{{ID: "A"}, {ID: "B"}, {ID: "LB"}, {ID: "RB"}},
	}
	gyroPad := model.Device{
		ID: "pad_trigger_gyro", Name: "Trigger gyro pad after switch", FixedScale: scale,
		Axes: []model.AxisSpec{
			{ID: "trigger", Min: 0, Max: 255, Bipolar: false, Direction: "positive", Deadzone: 400},
			{ID: "gyro_x", Min: -32768, Max: 32767, Bipolar: true, Direction: "both", Invert: true, Deadzone: 200},
			{ID: "gyro_y", Min: -32768, Max: 32767, Bipolar: true, Direction: "both", Deadzone: 200},
		},
		Buttons: []model.ButtonSpec{{ID: "X"}, {ID: "Y"}, {ID: "MENU"}},
	}
	curves := []model.Curve{
		{ID: "aim", Kind: "piecewise_linear", DomainScale: scale, CurvaturePPM: 90000, Points: []model.CurvePoint{
			{X: -10000, Y: -7000}, {X: -4000, Y: -3000}, {X: 0, Y: 0}, {X: 4000, Y: 3000}, {X: 10000, Y: 7000},
		}},
		{ID: "throttle", Kind: "piecewise_linear", DomainScale: scale, CurvaturePPM: 50000, Points: []model.CurvePoint{
			{X: 0, Y: 0}, {X: 5000, Y: 3500}, {X: 10000, Y: 10000},
		}},
	}
	bindings := []model.Binding{
		axisBinding("b_move_x", "MoveX", "pad_dual", "lx", "aim"),
		axisBinding("b_move_y", "MoveY", "pad_dual", "ly", "aim"),
		{ID: "b_jump", Action: "Jump", Type: model.BindingButton, DeviceID: "pad_dual", Button: "A"},
		{ID: "b_dash_old", Action: "DashOld", Type: model.BindingDoublePress, DeviceID: "pad_dual", Button: "A",
			Timing: model.TimingSpec{WindowMS: 120, MinGapMS: 10, MaxWindowMS: 300}},
		axisBinding("b_throttle", "Throttle", "pad_trigger_gyro", "trigger", "throttle"),
		{ID: "b_fire", Action: "Fire", Type: model.BindingButton, DeviceID: "pad_trigger_gyro", Button: "X"},
		{ID: "b_dash_new", Action: "DashNew", Type: model.BindingDoublePress, DeviceID: "pad_trigger_gyro", Button: "X",
			Timing: model.TimingSpec{WindowMS: 120, MinGapMS: 10, MaxWindowMS: 300}},
	}
	events := []model.Event{
		axisEvent(1, 0, "lx", 0),
		buttonEvent(2, 10, model.EventButtonDown, "A"),
		buttonEvent(3, 20, model.EventButtonRepeat, "A"),
		axisEvent(4, 40, "lx", g.jitter(5000, 150)),
		axisEvent(5, 60, "ly", g.jitter(-4000, 120)),
		buttonEvent(6, 80, model.EventButtonDown, "B"),
		buttonEvent(7, 90, model.EventButtonUp, "B"),
		buttonEvent(8, 100, model.EventButtonUp, "A"),
		axisEvent(9, 120, "lx", 0),
		axisEvent(10, 150, "ly", 0),
		buttonEvent(11, 200, model.EventButtonDown, "A"),
		buttonEvent(12, 210, model.EventButtonUp, "A"),
		{Seq: 13, TimeMS: 240, Kind: model.EventDeviceSwitch, Device: "pad_trigger_gyro"},
		axisEvent(14, 260, "trigger", g.jitter(100, 2)),
		axisEvent(15, 280, "gyro_x", g.jitter(6000, 150)),
		buttonEvent(16, 300, model.EventButtonDown, "MENU"),
		buttonEvent(17, 320, model.EventButtonDown, "X"),
		buttonEvent(18, 340, model.EventButtonRepeat, "X"),
		buttonEvent(19, 360, model.EventButtonUp, "X"),
		buttonEvent(20, 370, model.EventButtonUp, "MENU"),
		axisEvent(21, 380, "trigger", 255),
		axisEvent(22, 400, "gyro_y", g.jitter(-3000, 100)),
		axisEvent(23, 420, "gyro_x", 0),
		axisEvent(24, 440, "trigger", 0),
		buttonEvent(25, 460, model.EventButtonDown, "X"),
		buttonEvent(26, 470, model.EventButtonUp, "X"),
		buttonEvent(27, 490, model.EventButtonDown, "X"),
		buttonEvent(28, 500, model.EventButtonUp, "X"),
	}
	return &model.Scenario{
		SchemaVersion: model.SchemaVersion, ID: "hot_switch", Name: "Deterministic hot device switch with unmapped controls",
		Seed: g.seed, FixedPrecisionVersion: "fx-1e4-v3", BindingVersion: "bind-v3",
		Devices: []model.Device{oldPad, gyroPad}, InitialDeviceID: "pad_dual",
		Curves: curves, Bindings: bindings, Events: events,
	}
}

func axisBinding(id, action, device, axis, curve string) model.Binding {
	return model.Binding{ID: id, Action: action, Type: model.BindingAxis, DeviceID: device, Axis: axis, Curve: curve}
}

func axisEvent(seq int, t int64, axis string, value int64) model.Event {
	return model.Event{Seq: seq, TimeMS: t, Kind: model.EventAxis, Axis: axis, Value: value}
}

func buttonEvent(seq int, t int64, kind model.EventKind, button string) model.Event {
	return model.Event{Seq: seq, TimeMS: t, Kind: kind, Button: button}
}

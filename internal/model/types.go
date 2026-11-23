package model

const (
	SchemaVersion = 1

	CodeSchema = "E_SCHEMA"
	CodeParam  = "E_PARA"
	CodeDiff   = "E_DIFF"
	CodeVer    = "E_VER"
	CodeStore  = "E_STORE"
)

type ActionState string

const (
	StatePressed  ActionState = "pressed"
	StateHeld     ActionState = "held"
	StateReleased ActionState = "released"
)

type BindingType string

const (
	BindingButton      BindingType = "button"
	BindingAxis        BindingType = "axis"
	BindingCombo       BindingType = "combo"
	BindingLongPress   BindingType = "long_press"
	BindingDoublePress BindingType = "double_press"
)

type EventKind string

const (
	EventAxis         EventKind = "axis"
	EventButtonDown   EventKind = "button_down"
	EventButtonUp     EventKind = "button_up"
	EventButtonRepeat EventKind = "button_repeat"
	EventDeviceSwitch EventKind = "device_switch"
)

// Fraction is an exact rational value in fixed-point units after normalization.
// Den is the raw axis span, so the value can be audited without floating point.
type Fraction struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

type AxisSpec struct {
	ID        string `json:"id"`
	Min       int64  `json:"min"`
	Max       int64  `json:"max"`
	Bipolar   bool   `json:"bipolar"`
	Direction string `json:"direction"`
	Invert    bool   `json:"invert"`
	Deadzone  int64  `json:"deadzone"`
}

type ButtonSpec struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type Device struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	FixedScale int64        `json:"fixed_scale"`
	Axes       []AxisSpec   `json:"axes"`
	Buttons    []ButtonSpec `json:"buttons"`
}

type CurvePoint struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type Curve struct {
	ID           string       `json:"id"`
	Kind         string       `json:"kind"`
	DomainScale  int64        `json:"domain_scale"`
	CurvaturePPM int64        `json:"curvature_ppm"`
	Points       []CurvePoint `json:"points"`
}

type TimingSpec struct {
	WindowMS       int64 `json:"window_ms"`
	MinGapMS       int64 `json:"min_gap_ms"`
	ThresholdMS    int64 `json:"threshold_ms"`
	MaxThresholdMS int64 `json:"max_threshold_ms"`
	MaxWindowMS    int64 `json:"max_window_ms"`
}

type Binding struct {
	ID       string      `json:"id"`
	Action   string      `json:"action"`
	Type     BindingType `json:"type"`
	DeviceID string      `json:"device_id"`
	Axis     string      `json:"axis,omitempty"`
	Button   string      `json:"button,omitempty"`
	Buttons  []string    `json:"buttons,omitempty"`
	Curve    string      `json:"curve,omitempty"`
	Timing   TimingSpec  `json:"timing"`
}

type Event struct {
	Seq    int       `json:"seq"`
	TimeMS int64     `json:"time_ms"`
	Kind   EventKind `json:"kind"`
	Axis   string    `json:"axis,omitempty"`
	Value  int64     `json:"value"`
	Button string    `json:"button,omitempty"`
	Device string    `json:"device,omitempty"`
}

type TruthAxisSample struct {
	EventSeq       int      `json:"event_seq"`
	TimeMS         int64    `json:"time_ms"`
	DeviceID       string   `json:"device_id"`
	BindingID      string   `json:"binding_id"`
	ActionID       string   `json:"action_id"`
	AxisID         string   `json:"axis_id"`
	Raw            int64    `json:"raw"`
	Normalized     int64    `json:"normalized"`
	Residual       Fraction `json:"quantization_residual"`
	ResidualBound  int64    `json:"quantization_error_bound"`
	DeadzoneInside bool     `json:"deadzone_inside"`
	DeadzoneMargin int64    `json:"deadzone_margin"`
}

type TruthAction struct {
	BindingID     string      `json:"binding_id"`
	ActionID      string      `json:"action_id"`
	EventSeq      int         `json:"event_seq"`
	TriggerTimeMS int64       `json:"trigger_time_ms"`
	State         ActionState `json:"state"`
	AxisValue     *int64      `json:"axis_value,omitempty"`
}

type TruthUnmapped struct {
	EventSeq int    `json:"event_seq"`
	TimeMS   int64  `json:"time_ms"`
	DeviceID string `json:"device_id"`
	Control  string `json:"control"`
	Kind     string `json:"kind"`
}

type Truth struct {
	AxisSamples []TruthAxisSample `json:"axis_samples"`
	Actions     []TruthAction     `json:"actions"`
	Unmapped    []TruthUnmapped   `json:"unmapped"`
}

type Scenario struct {
	SchemaVersion         int       `json:"schema_version"`
	ID                    string    `json:"id"`
	Name                  string    `json:"name"`
	Seed                  int64     `json:"seed"`
	FixedPrecisionVersion string    `json:"fixed_precision_version"`
	BindingVersion        string    `json:"binding_version"`
	Devices               []Device  `json:"devices"`
	InitialDeviceID       string    `json:"initial_device_id"`
	Curves                []Curve   `json:"curves"`
	Bindings              []Binding `json:"bindings"`
	Events                []Event   `json:"events"`
	Truth                 Truth     `json:"truth"`
}

type ManifestScenario struct {
	ID   string `json:"id"`
	File string `json:"file"`
}

type Manifest struct {
	SchemaVersion int                `json:"schema_version"`
	Seed          int64              `json:"seed"`
	Scenarios     []ManifestScenario `json:"scenarios"`
}

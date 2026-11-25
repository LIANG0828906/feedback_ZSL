package protocol

const (
	CurrentSchemaVersion    = 1
	CurrentAlgorithmVersion = "castmap-v1"
)

type SessionState string

const (
	StateEstablished SessionState = "established"
	StateNegotiating SessionState = "negotiating"
	StateCasting     SessionState = "casting"
	StatePaused      SessionState = "paused"
	StateEnded       SessionState = "ended"
)

type EventKind string

const (
	EventNegotiate EventKind = "negotiate"
	EventStart     EventKind = "start"
	EventJoin      EventKind = "join"
	EventLeave     EventKind = "leave"
	EventTopology  EventKind = "topology"
	EventPause     EventKind = "pause"
	EventResume    EventKind = "resume"
	EventEnd       EventKind = "end"
)

func (k EventKind) IsEndpoint() bool {
	return k == EventJoin || k == EventLeave
}

func (k EventKind) IsTopologyAffecting() bool {
	return k == EventJoin || k == EventLeave || k == EventTopology
}

type CropMode string

const (
	CropFloor   CropMode = "floor"
	CropCeil    CropMode = "ceil"
	CropNearest CropMode = "nearest"
	CropOutward CropMode = "outward"
	CropInward  CropMode = "inward"
)

func (m CropMode) Valid() bool {
	switch m {
	case CropFloor, CropCeil, CropNearest, CropOutward, CropInward:
		return true
	default:
		return false
	}
}

type Resolution struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (r Rect) Empty() bool {
	return r.Width <= 0 || r.Height <= 0
}

type Topology struct {
	X          int        `json:"x"`
	Y          int        `json:"y"`
	Width      int        `json:"width"`
	Height     int        `json:"height"`
	Resolution Resolution `json:"resolution"`
	Scale      Ratio      `json:"scale"`
}

type Capability struct {
	Resolution Resolution `json:"resolution"`
	Scale      Ratio      `json:"scale"`
	CropMode   CropMode   `json:"crop_mode"`
}

type Endpoint struct {
	ID         string     `json:"id"`
	Topology   Topology   `json:"topology"`
	Capability Capability `json:"capability"`
}

type Event struct {
	Seq        int64       `json:"seq"`
	Time       int64       `json:"time"`
	Kind       EventKind   `json:"kind"`
	DisplayID  string      `json:"display_id,omitempty"`
	Topology   *Topology   `json:"topology,omitempty"`
	Capability *Capability `json:"capability,omitempty"`
}

type Interaction struct {
	Seq      int64  `json:"seq"`
	Time     int64  `json:"time"`
	Kind     string `json:"kind"`
	LogicalX int    `json:"logical_x"`
	LogicalY int    `json:"logical_y"`
}

type FrameWindow struct {
	Index        int           `json:"index"`
	Time         int64         `json:"time"`
	Region       Rect          `json:"region"`
	Targets      []string      `json:"targets,omitempty"`
	Interactions []Interaction `json:"interactions,omitempty"`
}

type Session struct {
	SchemaVersion    int           `json:"schema_version"`
	ID               string        `json:"session_id"`
	AlgorithmVersion string        `json:"algorithm_version"`
	Master           Endpoint      `json:"master"`
	Displays         []Endpoint    `json:"displays"`
	InitialDisplays  []string      `json:"initial_displays"`
	Events           []Event       `json:"events"`
	Windows          []FrameWindow `json:"windows"`
	TotalWindows     int           `json:"total_windows"`
}

type SampleSet struct {
	Seed     int64     `json:"seed"`
	Screens  int       `json:"screens"`
	Windows  int       `json:"windows"`
	Sessions []Session `json:"sessions"`
	Truths   []Truth   `json:"truths"`
}

type ScreenTruth struct {
	DisplayID    string `json:"display_id"`
	Pixel        Rect   `json:"pixel"`
	World        Rect   `json:"world"`
	ScreenWorld  Rect   `json:"screen_world"`
	SourceWindow int    `json:"source_window"`
}

type WindowTruth struct {
	Window    int           `json:"window"`
	Time      int64         `json:"time"`
	Endpoints []string      `json:"endpoints"`
	Screens   []ScreenTruth `json:"screens"`
}

type EventTruth struct {
	Seq             int64        `json:"seq"`
	Time            int64        `json:"time"`
	Kind            EventKind    `json:"kind"`
	DisplayID       string       `json:"display_id,omitempty"`
	EffectiveTime   int64        `json:"effective_time"`
	CommittedWindow int          `json:"committed_window"`
	ReplayWindow    int          `json:"replay_window,omitempty"`
	EndpointsAfter  []string     `json:"endpoints_after"`
	StateAfter      SessionState `json:"state_after"`
	Replay          *ScreenTruth `json:"replay,omitempty"`
}

type Truth struct {
	SessionID      string        `json:"session_id"`
	Windows        []WindowTruth `json:"windows"`
	EventEffects   []EventTruth  `json:"event_effects"`
	FinalEndpoints []string      `json:"final_endpoints"`
	FinalState     SessionState  `json:"final_state"`
}

type ActionKind string

const (
	ActionFrame ActionKind = "frame"
)

type Action struct {
	Time        int64
	Phase       int
	Seq         int64
	Kind        EventKind
	Frame       bool
	EventIndex  int
	WindowIndex int
}

func (a Action) Path() string {
	if a.Frame {
		return "$.windows[" + itoa(a.WindowIndex) + "]"
	}
	return "$.events[" + itoa(a.EventIndex) + "]"
}

func (a Action) DisplayID(session *Session) string {
	if a.Frame {
		return ""
	}
	if a.EventIndex >= 0 && a.EventIndex < len(session.Events) {
		return session.Events[a.EventIndex].DisplayID
	}
	return ""
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

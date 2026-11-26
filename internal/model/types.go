package model

const (
	AlgorithmVersion  = "mapping-v1.0.0"
	InternalCoordUnit = int64(256)
	TicksPerSecond    = int64(90000)
	PixelBytes        = int64(4)
)

type ScaleTier string

const (
	ScaleAuto ScaleTier = "auto"
	Scale100  ScaleTier = "100"
	Scale125  ScaleTier = "125"
	Scale150  ScaleTier = "150"
	Scale200  ScaleTier = "200"
)

func (s ScaleTier) Percent() (int64, bool) {
	switch s {
	case Scale100:
		return 100, true
	case Scale125:
		return 125, true
	case Scale150:
		return 150, true
	case Scale200:
		return 200, true
	default:
		return 0, false
	}
}

func (s ScaleTier) Rational() (num, den int64, ok bool) {
	pct, valid := s.Percent()
	if !valid {
		return 0, 0, false
	}
	return pct, 100, true
}

type Chroma string

const (
	Chroma420 Chroma = "4:2:0"
	Chroma422 Chroma = "4:2:2"
	Chroma444 Chroma = "4:4:4"
)

type Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type Rect struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	W int64 `json:"w"`
	H int64 `json:"h"`
}

func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

func (r Rect) Right() (int64, error) {
	if r.W > 0 && r.X > (1<<62)-r.W {
		return 0, OvfError("rect.right", r.X, r.W)
	}
	return r.X + r.W, nil
}

func (r Rect) Bottom() (int64, error) {
	if r.H > 0 && r.Y > (1<<62)-r.H {
		return 0, OvfError("rect.bottom", r.Y, r.H)
	}
	return r.Y + r.H, nil
}

type CoordBasis struct {
	InternalUnitsPerLogical int64  `json:"internal_units_per_logical"`
	Expression              string `json:"expression"`
}

type TimeUnit struct {
	SourceName        string `json:"source_name"`
	SourceTicksPerSec int64  `json:"source_ticks_per_sec"`
	InternalPerSource int64  `json:"internal_per_source_tick"`
}

type Screen struct {
	ID               string      `json:"id"`
	Revision         int64       `json:"revision"`
	Origin           Point       `json:"origin"`
	PixelW           int64       `json:"pixel_w"`
	PixelH           int64       `json:"pixel_h"`
	Scale            ScaleTier   `json:"scale"`
	SupportedScales  []ScaleTier `json:"supported_scales"`
	Chroma           Chroma      `json:"chroma"`
	SupportedChromas []Chroma    `json:"supported_chromas"`
}

type EventKind string

const (
	EventJoin     EventKind = "join"
	EventLeave    EventKind = "leave"
	EventTopology EventKind = "topology"
)

type TopologyEvent struct {
	ID       string    `json:"id"`
	Seq      int       `json:"seq"`
	Tick     int64     `json:"tick"`
	Kind     EventKind `json:"kind"`
	ScreenID string    `json:"screen_id"`
	Revision int64     `json:"revision"`
	Screen   *Screen   `json:"screen,omitempty"`
}

type WindowInstruction struct {
	Seq            int    `json:"seq"`
	ID             string `json:"id"`
	Tick           int64  `json:"tick"`
	Region         Rect   `json:"region"`
	MaxPacketBytes int64  `json:"max_packet_bytes"`
	ContentSeed    int64  `json:"content_seed"`
}

type Project struct {
	SchemaMajor   int                 `json:"schema_major"`
	SourceVersion string              `json:"source_version"`
	SessionID     string              `json:"session_id"`
	Seed          int64               `json:"seed"`
	Master        Rect                `json:"master"`
	CoordBasis    CoordBasis          `json:"coord_basis"`
	TimeBases     map[string]TimeUnit `json:"time_bases"`
	Screens       []Screen            `json:"screens"`
	Events        []TopologyEvent     `json:"events"`
	Windows       []WindowInstruction `json:"windows"`
}

type DefaultFill struct {
	Path         string `json:"path"`
	OldValue     string `json:"old_value"`
	DefaultValue string `json:"default_value"`
	Reason       string `json:"reason"`
}

type LossyItem struct {
	Path        string `json:"path"`
	Value       string `json:"value"`
	Code        Code   `json:"code"`
	Disposition string `json:"disposition"`
	Replacement string `json:"replacement"`
}

type ConvertReport struct {
	SourceVersion string        `json:"source_version"`
	TargetVersion string        `json:"target_version,omitempty"`
	Fills         []DefaultFill `json:"fills"`
	Lossy         []LossyItem   `json:"lossy"`
}

type TruthCheck struct {
	Name   string `json:"name"`
	Expect string `json:"expect"`
}

type WindowTruth struct {
	Seq             int      `json:"seq"`
	Tick            int64    `json:"tick"`
	ActiveScreenIDs []string `json:"active_screen_ids"`
	WinnerEventIDs  []string `json:"winner_event_ids"`
	ReplayScreenIDs []string `json:"replay_screen_ids"`
}

type TruthManifest struct {
	Seed          int64         `json:"seed"`
	FixedSeed     int64         `json:"fixed_seed"`
	Versions      []string      `json:"versions"`
	ScreenIDs     []string      `json:"screen_ids"`
	EventIDs      []string      `json:"event_ids"`
	WindowIDs     []string      `json:"window_ids"`
	WindowCount   int           `json:"window_count"`
	EventCount    int           `json:"event_count"`
	WindowTruths  []WindowTruth `json:"window_truths"`
	Checks        []TruthCheck  `json:"checks"`
	CanonicalHash string        `json:"canonical_hash"`
}

type SampleSet struct {
	Seed    int64                     `json:"seed"`
	Project *Project                  `json:"project"`
	Raw     map[string][]byte         `json:"-"`
	Reports map[string]*ConvertReport `json:"reports,omitempty"`
	Truth   *TruthManifest            `json:"truth"`
}

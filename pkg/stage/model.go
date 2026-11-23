package stage

import (
	"encoding/json"
	"math"
	"sort"
)

const (
	SchemaVersion    = 1
	AlgorithmVersion = "stage-engine/v1"
)

type RGB struct {
	R float64 `json:"r"`
	G float64 `json:"g"`
	B float64 `json:"b"`
}

type Vec2 struct {
	Pan  float64 `json:"pan"`
	Tilt float64 `json:"tilt"`
}

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type Range struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

func (r Range) Contains(v float64) bool {
	return v >= r.Min && v <= r.Max
}

type ParamRanges struct {
	Red       Range `json:"red"`
	Green     Range `json:"green"`
	Blue      Range `json:"blue"`
	Intensity Range `json:"intensity"`
	Pan       Range `json:"pan"`
	Tilt      Range `json:"tilt"`
	Focus     Range `json:"focus"`
	ColorTemp Range `json:"color_temp"`
	StrobeHz  Range `json:"strobe_hz"`
}

type LightValues struct {
	RGB       RGB     `json:"rgb"`
	Intensity float64 `json:"intensity"`
	Pan       float64 `json:"pan"`
	Tilt      float64 `json:"tilt"`
	Focus     float64 `json:"focus"`
	ColorTemp float64 `json:"color_temp"`
	StrobeHz  float64 `json:"strobe_hz"`
}

func (v LightValues) Clone() LightValues { return v }

type LightKind string

const (
	LightPar    LightKind = "par"
	LightMoving LightKind = "moving"
	LightWash   LightKind = "wash"
)

type Light struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Kind   LightKind   `json:"kind"`
	Layout Vec3        `json:"layout"`
	Base   LightValues `json:"base"`
	Ranges ParamRanges `json:"ranges"`
}

type Group struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Members  []string `json:"members,omitempty"`
	ParentID string   `json:"parent_id,omitempty"`
	Priority int      `json:"priority"`
}

type CueKind string

const (
	CueColorTemp CueKind = "color_temp"
	CueMove      CueKind = "move"
	CueStrobe    CueKind = "strobe"
	CueFade      CueKind = "fade"
	CueGroup     CueKind = "group"
	CueSet       CueKind = "set"
)

type Easing string

const (
	EaseLinear Easing = "linear"
	EaseStep   Easing = "step"
	EaseInOut  Easing = "ease_in_out"
)

// CueValues is intentionally sparse: nil means "this cue does not touch the field".
// The first non-nil update at a logical timestamp wins according to Cue.Scope rules.
type CueValues struct {
	RGB       *RGB     `json:"rgb,omitempty"`
	Intensity *float64 `json:"intensity,omitempty"`
	Pan       *float64 `json:"pan,omitempty"`
	Tilt      *float64 `json:"tilt,omitempty"`
	Focus     *float64 `json:"focus,omitempty"`
	ColorTemp *float64 `json:"color_temp,omitempty"`
	StrobeHz  *float64 `json:"strobe_hz,omitempty"`
}

func (v CueValues) IsEmpty() bool {
	return v.RGB == nil && v.Intensity == nil && v.Pan == nil && v.Tilt == nil &&
		v.Focus == nil && v.ColorTemp == nil && v.StrobeHz == nil
}

type Cue struct {
	ID           string    `json:"id"`
	Label        string    `json:"label"`
	TimeMS       int64     `json:"time_ms"`
	LightID      string    `json:"light_id,omitempty"`
	GroupID      string    `json:"group_id,omitempty"`
	Kind         CueKind   `json:"kind"`
	Values       CueValues `json:"values"`
	TransitionMS int64     `json:"transition_ms"`
	Easing       Easing    `json:"easing"`
	CurvePower   float64   `json:"curve_power"`
	Priority     int       `json:"priority"`
}

type Scene struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	CueIDs []string `json:"cue_ids"`
}

type TruthLight struct {
	LightID   string  `json:"light_id"`
	RGB       RGB     `json:"rgb"`
	Intensity float64 `json:"intensity"`
	Pan       float64 `json:"pan"`
	Tilt      float64 `json:"tilt"`
	Focus     float64 `json:"focus"`
	ColorTemp float64 `json:"color_temp"`
	StrobeHz  float64 `json:"strobe_hz"`
}

type TruthSample struct {
	Frame  int          `json:"frame"`
	TimeMS int64        `json:"time_ms"`
	Lights []TruthLight `json:"lights"`
}

type Tolerance struct {
	Color     float64 `json:"color"`
	Position  float64 `json:"position"`
	Intensity float64 `json:"intensity"`
	Focus     float64 `json:"focus"`
	ColorTemp float64 `json:"color_temp"`
	StrobeHz  float64 `json:"strobe_hz"`
}

func DefaultTolerance() Tolerance {
	return Tolerance{
		Color:     1.5,
		Position:  0.5,
		Intensity: 0.01,
		Focus:     0.5,
		ColorTemp: 5,
		StrobeHz:  0.05,
	}
}

type Truth struct {
	TotalDurationMS int64         `json:"total_duration_ms"`
	SampleTimesMS   []int64       `json:"sample_times_ms"`
	Samples         []TruthSample `json:"samples"`
	GroupOrder      []string      `json:"group_order"`
	Tolerance       Tolerance     `json:"tolerance"`
}

type Project struct {
	SchemaVersion int     `json:"schema_version"`
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Seed          int64   `json:"seed"`
	FPS           int     `json:"fps"`
	DurationMS    int64   `json:"duration_ms"`
	WindowMS      int64   `json:"window_ms"`
	Lights        []Light `json:"lights"`
	Groups        []Group `json:"groups"`
	Cues          []Cue   `json:"cues"`
	Scenes        []Scene `json:"scenes"`
	Truth         Truth   `json:"truth"`
}

type LightOutput struct {
	LightID    string  `json:"light_id"`
	RGB        RGB     `json:"rgb"`
	RawRGB     RGB     `json:"raw_rgb"`
	Intensity  float64 `json:"intensity"`
	Pan        float64 `json:"pan"`
	Tilt       float64 `json:"tilt"`
	Focus      float64 `json:"focus"`
	ColorTemp  float64 `json:"color_temp"`
	StrobeHz   float64 `json:"strobe_hz"`
	StrobeOpen bool    `json:"strobe_open"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Z          float64 `json:"z"`
}

type FrameOutput struct {
	Frame  int           `json:"frame"`
	TimeUS int64         `json:"time_us"`
	Lights []LightOutput `json:"lights"`
}

type Candidate struct {
	SourceType string  `json:"source_type"`
	SourceID   string  `json:"source_id"`
	CueID      string  `json:"cue_id"`
	Priority   int     `json:"priority"`
	Value      float64 `json:"value"`
}

type Arbitration struct {
	TimeUS     int64       `json:"time_us"`
	LightID    string      `json:"light_id"`
	Field      string      `json:"field"`
	Winner     string      `json:"winner"`
	Rule       string      `json:"rule"`
	Candidates []Candidate `json:"candidates"`
}

type ConflictSide struct {
	Object string  `json:"object"`
	CueID  string  `json:"cue_id"`
	Field  string  `json:"field"`
	Value  float64 `json:"value"`
}

type GroupSnapshot struct {
	GroupID  string    `json:"group_id"`
	Members  []string  `json:"members"`
	Priority int       `json:"priority"`
	LastCue  string    `json:"last_cue,omitempty"`
	LastTime int64     `json:"last_time_ms"`
	Values   CueValues `json:"values"`
}

type MergeSpan struct {
	FirstCueIndex int     `json:"first_cue_index"`
	LastCueIndex  int     `json:"last_cue_index"`
	FirstCueID    string  `json:"first_cue_id"`
	LastCueID     string  `json:"last_cue_id"`
	StartUS       int64   `json:"start_us"`
	EndUS         int64   `json:"end_us"`
	KeyFrames     []int   `json:"key_frames"`
	ErrorBound    float64 `json:"error_bound"`
}

type MergeInfo struct {
	Mode             Mode        `json:"mode"`
	Basis            string      `json:"basis"`
	PreviewFPS       int         `json:"preview_fps"`
	FrameStride      int         `json:"frame_stride"`
	Spans            []MergeSpan `json:"spans"`
	MaxObservedError float64     `json:"max_observed_error"`
}

type WindowState string

const (
	WindowPending    WindowState = "pending"
	WindowScheduling WindowState = "scheduling"
	WindowCommitted  WindowState = "committed"
	WindowFailed     WindowState = "failed"
)

type WindowResult struct {
	Index            int             `json:"index"`
	StartUS          int64           `json:"start_us"`
	EndUS            int64           `json:"end_us"`
	State            WindowState     `json:"state"`
	Frames           []FrameOutput   `json:"frames"`
	CueStart         int             `json:"cue_start"`
	CueOffset        int             `json:"cue_offset"`
	ConsumedCues     int             `json:"consumed_cues"`
	Groups           []GroupSnapshot `json:"groups"`
	Decisions        []Arbitration   `json:"decisions"`
	Merge            *MergeInfo      `json:"merge,omitempty"`
	ContentHash      string          `json:"content_hash"`
	ParamVersion     string          `json:"param_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
}

type Mode string

const (
	ModeExact  Mode = "exact"
	ModeMerged Mode = "merged"
)

type ModeDecision struct {
	Mode            Mode     `json:"mode"`
	CueCount        int      `json:"cue_count"`
	LightCount      int      `json:"light_count"`
	FrameCount      int      `json:"frame_count"`
	EstimatedOps    int64    `json:"estimated_ops"`
	PreviewCapacity int64    `json:"preview_capacity"`
	MaxExactCues    int      `json:"max_exact_cues"`
	MaxExactOps     int64    `json:"max_exact_ops"`
	PreviewFPS      int      `json:"preview_fps"`
	Reasons         []string `json:"reasons"`
	Forced          bool     `json:"forced"`
}

type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

func (p Progress) Ratio() float64 {
	if p.Total == 0 {
		return 0
	}
	return float64(p.Committed) / float64(p.Total)
}

func FrameTimeUS(frame, fps int) int64 {
	if fps <= 0 {
		return 0
	}
	return int64(math.Round(float64(frame) * 1_000_000 / float64(fps)))
}

func FrameCount(durationMS int64, fps int) int {
	if durationMS < 0 || fps <= 0 {
		return 0
	}
	return int(durationMS*int64(fps)/1000) + 1
}

func WindowCount(durationMS, windowMS int64) int {
	if windowMS <= 0 {
		return 0
	}
	if durationMS < 0 {
		return 0
	}
	return int(durationMS/windowMS) + 1
}

func WindowBounds(durationMS, windowMS int64, index int) (int64, int64) {
	start := int64(index) * windowMS * 1000
	end := start + windowMS*1000
	limit := durationMS*1000 + 1
	if end > limit {
		end = limit
	}
	return start, end
}

func windowIndexForTime(timeUS, windowUS int64, total int) int {
	if total <= 0 || windowUS <= 0 {
		return 0
	}
	index := int(timeUS / windowUS)
	if index >= total {
		index = total - 1
	}
	if index < 0 {
		index = 0
	}
	return index
}

func cloneJSON[T any](v T) T {
	var out T
	raw, _ := json.Marshal(v)
	_ = json.Unmarshal(raw, &out)
	return out
}

func round6(v float64) float64 {
	if math.Abs(v) < 0.0000005 {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sortedLightIDs(project *Project) []string {
	ids := make([]string, 0, len(project.Lights))
	for _, light := range project.Lights {
		ids = append(ids, light.ID)
	}
	sort.Strings(ids)
	return ids
}

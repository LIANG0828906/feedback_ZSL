package wb

import (
	"encoding/json"
	"strconv"
	"strings"
)

// FixedScale is the declared decimal resolution for all dB and normalized
// parameter arithmetic. Fixed values store milli-units.
const FixedScale int64 = 1000

// Fixed is a deterministic milli-unit fixed-point number.
type Fixed int64

func FixedFromFloat(v float64) Fixed {
	return Fixed(v * float64(FixedScale))
}

func ParseFixed(s string) (Fixed, error) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, err
	}
	value := whole * FixedScale
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) > 3 {
			frac = frac[:3]
		}
		for len(frac) < 3 {
			frac += "0"
		}
		n, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, err
		}
		value += n
	}
	if neg {
		value = -value
	}
	return Fixed(value), nil
}

func (f Fixed) Float64() float64 { return float64(f) / float64(FixedScale) }

func (f Fixed) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(f.Float64(), 'f', 3, 64)), nil
}

func (f *Fixed) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		parsed, err := ParseFixed(text)
		if err != nil {
			return err
		}
		*f = parsed
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	parsed, err := ParseFixed(number.String())
	if err != nil {
		return err
	}
	*f = parsed
	return nil
}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Rect struct {
	X1 int `json:"x1"`
	Y1 int `json:"y1"`
	X2 int `json:"x2"`
	Y2 int `json:"y2"`
}

func (r Rect) Width() int  { return r.X2 - r.X1 }
func (r Rect) Height() int { return r.Y2 - r.Y1 }

func (r Rect) Contains(p Point) bool {
	return p.X >= r.X1 && p.X <= r.X2 && p.Y >= r.Y1 && p.Y <= r.Y2
}

func (r Rect) ContainsInterior(p Point) bool {
	return p.X > r.X1 && p.X < r.X2 && p.Y > r.Y1 && p.Y < r.Y2
}

type RoomParams struct {
	RT60MS                 int64 `json:"rt60Ms"`
	EarlyReflectionDensity Fixed `json:"earlyReflectionDensity"`
	GainDB                 Fixed `json:"gainDb"`
}

type Room struct {
	ID     string     `json:"id"`
	Rect   Rect       `json:"rect"`
	Params RoomParams `json:"params"`
}

type Wall struct {
	ID    string `json:"id"`
	X1    int    `json:"x1"`
	Y1    int    `json:"y1"`
	X2    int    `json:"x2"`
	Y2    int    `json:"y2"`
	RoomA string `json:"roomA,omitempty"`
	RoomB string `json:"roomB,omitempty"`
}

func (w Wall) Horizontal() bool { return w.Y1 == w.Y2 }
func (w Wall) Vertical() bool   { return w.X1 == w.X2 }

type Opening struct {
	ID     string `json:"id"`
	WallID string `json:"wallId"`
	Kind   string `json:"kind"` // door or opening
	X1     int    `json:"x1"`
	Y1     int    `json:"y1"`
	X2     int    `json:"x2"`
	Y2     int    `json:"y2"`
	RoomA  string `json:"roomA,omitempty"`
	RoomB  string `json:"roomB,omitempty"`
}

func (o Opening) Horizontal() bool { return o.Y1 == o.Y2 }
func (o Opening) Vertical() bool   { return o.X1 == o.X2 }

func (o Opening) Width() int {
	if o.Horizontal() {
		return absInt(o.X2 - o.X1)
	}
	return absInt(o.Y2 - o.Y1)
}

func (o Opening) Midpoint() Point {
	return Point{X: (o.X1 + o.X2) / 2, Y: (o.Y1 + o.Y2) / 2}
}

type Source struct {
	ID      string `json:"id"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	PowerDB Fixed  `json:"powerDb"`
}

type Listener struct {
	ID string `json:"id"`
	X  int    `json:"x"`
	Y  int    `json:"y"`
}

type PrecisionSpec struct {
	Scale            int64 `json:"scale"`
	SampleCount      int   `json:"sampleCount"`
	DeltaTMS         int64 `json:"dtMs"`
	ToleranceDB      Fixed `json:"toleranceDb"`
	MaxParallel      int   `json:"maxParallel"`
	BufferSampleCap  int   `json:"bufferSampleCap"`
	PathAttenPerUnit Fixed `json:"pathAttenPerUnitDb"`
	OcclusionAttenDB Fixed `json:"occlusionAttenDb"`
	EarlyBoostMaxDB  Fixed `json:"earlyBoostMaxDb"`
	EarlyWindowMS    int64 `json:"earlyWindowMs"`
}

type TruthPath struct {
	SourceID       string   `json:"sourceId"`
	ListenerID     string   `json:"listenerId"`
	RoomChain      []string `json:"roomChain"`
	DoorChain      []string `json:"doorChain"`
	Length         int      `json:"length"`
	OcclusionCount int      `json:"occlusionCount"`
	OcclusionDB    Fixed    `json:"occlusionDb"`
	ExpectedSPLDB  Fixed    `json:"expectedSplDb"`
	DecaySamplesDB []Fixed  `json:"decaySamplesDb"`
}

type Truth struct {
	ListenerRooms map[string]string `json:"listenerRooms"`
	Paths         []TruthPath       `json:"paths"`
}

type Sample struct {
	Seed      int64         `json:"seed"`
	Name      string        `json:"name"`
	Bounds    Rect          `json:"bounds"`
	Precision PrecisionSpec `json:"precision"`
	Rooms     []Room        `json:"rooms"`
	Walls     []Wall        `json:"walls"`
	Openings  []Opening     `json:"openings"`
	Sources   []Source      `json:"sources"`
	Listeners []Listener    `json:"listeners"`
	Truth     Truth         `json:"truth"`
}

type CurveSample struct {
	TMS     int64 `json:"tMs"`
	LevelDB Fixed `json:"levelDb"`
}

type PathResult struct {
	SourceID        string   `json:"sourceId"`
	ListenerID      string   `json:"listenerId"`
	SourceRoom      string   `json:"sourceRoom"`
	ListenerRoom    string   `json:"listenerRoom"`
	RoomChain       []string `json:"roomChain"`
	DoorChain       []string `json:"doorChain"`
	Length          int      `json:"length"`
	Polyline        []Point  `json:"polyline"`
	OcclusionCount  int      `json:"occlusionCount"`
	OcclusionPoints []Point  `json:"occlusionPoints"`
	OcclusionDB     Fixed    `json:"occlusionDb"`
}

type PairResult struct {
	Path          PathResult    `json:"path"`
	Samples       []CurveSample `json:"samples"`
	ExpectedSPLDB Fixed         `json:"expectedSplDb"`
	TruthSPLDB    Fixed         `json:"truthSplDb,omitempty"`
	SPLMatch      bool          `json:"splMatch"`
	CurveMatch    bool          `json:"curveMatch"`
	FirstMismatch string        `json:"firstMismatch,omitempty"`
	Error         *APIError     `json:"error,omitempty"`
}

type ListenerResult struct {
	ListenerID string       `json:"listenerId"`
	RoomID     string       `json:"roomId"`
	Pairs      []PairResult `json:"pairs"`
}

type RoomStatus string

const (
	StatusPending   RoomStatus = "pending"
	StatusAnalyzing RoomStatus = "analyzing"
	StatusSolving   RoomStatus = "solving"
	StatusCommitted RoomStatus = "committed"
	StatusFailed    RoomStatus = "failed"
)

type RoomState struct {
	RoomID           string     `json:"roomId"`
	Status           RoomStatus `json:"status"`
	CommitOrder      int        `json:"commitOrder"`
	ContentHash      string     `json:"contentHash,omitempty"`
	ParamVersion     int        `json:"paramVersion"`
	AlgorithmVersion int        `json:"algorithmVersion"`
	LastError        *APIError  `json:"lastError,omitempty"`
	Stale            bool       `json:"stale"`
}

type InvalidationItem struct {
	Kind       string   `json:"kind"`
	ID         string   `json:"id"`
	RoomID     string   `json:"roomId,omitempty"`
	Reason     string   `json:"reason"`
	Status     string   `json:"status"`
	Reused     bool     `json:"reused"`
	DependsOn  []string `json:"dependsOn,omitempty"`
	AffectedBy []string `json:"affectedBy,omitempty"`
}

type InvalidationReport struct {
	Reason    string             `json:"reason"`
	Items     []InvalidationItem `json:"items"`
	TopoOrder []string           `json:"topoOrder"`
}

type DiffReport struct {
	Match           bool   `json:"match"`
	FirstLocator    string `json:"firstLocator,omitempty"`
	Expected        any    `json:"expected,omitempty"`
	Actual          any    `json:"actual,omitempty"`
	IncrementalHash string `json:"incrementalHash,omitempty"`
	FullHash        string `json:"fullHash,omitempty"`
}

type QueueEvent struct {
	TaskID      string `json:"taskId"`
	Reason      string `json:"reason"`
	WaitMS      int64  `json:"waitMs"`
	AtUnixMilli int64  `json:"atUnixMilli"`
}

type SchedulerDiagnostics struct {
	MaxParallel     int          `json:"maxParallel"`
	BufferSampleCap int          `json:"bufferSampleCap"`
	ActiveTasks     int          `json:"activeTasks"`
	CompletedTasks  int64        `json:"completedTasks"`
	QueuedEvents    []QueueEvent `json:"queuedEvents"`
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

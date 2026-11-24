package model

const (
	CodeSchema  = "E_SCHEMA"
	CodeGeom    = "E_GEOM"
	CodeUnreach = "E_UNREACH"
	CodePrec    = "E_PREC"
	CodeState   = "E_STATE"
	CodeScope   = "E_SCOPE"
	CodeDiff    = "E_DIFF"
)

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Ref     string         `json:"ref,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

type ReverbParamsSnapshot struct {
	RT60Seconds      float64 `json:"rt60_seconds"`
	EarlyDensity     float64 `json:"early_reflection_density"`
	ReverbGain       float64 `json:"reverb_gain"`
	OcclusionPerDoor float64 `json:"occlusion_per_door_db,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Ref != "" {
		return e.Code + " " + e.Ref + ": " + e.Message
	}
	return e.Code + ": " + e.Message
}

func NewError(code, ref, message string) *Error {
	return &Error{Code: code, Ref: ref, Message: message}
}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Rect struct {
	Min Point `json:"min"`
	Max Point `json:"max"`
}

func (r Rect) Width() int  { return r.Max.X - r.Min.X }
func (r Rect) Height() int { return r.Max.Y - r.Min.Y }

type Room struct {
	ID               string  `json:"id"`
	Rect             Rect    `json:"rect"`
	RT60Seconds      float64 `json:"rt60_seconds"`
	EarlyDensity     float64 `json:"early_reflection_density"`
	ReverbGain       float64 `json:"reverb_gain"`
	OcclusionPerDoor float64 `json:"occlusion_per_door_db,omitempty"`
}

type Wall struct {
	ID      string `json:"id"`
	A       Point  `json:"a"`
	B       Point  `json:"b"`
	RoomAID string `json:"room_a_id"`
	RoomBID string `json:"room_b_id,omitempty"`
}

type Door struct {
	ID     string `json:"id"`
	WallID string `json:"wall_id"`
	A      Point  `json:"a"`
	B      Point  `json:"b"`
	Kind   string `json:"kind"`
}

func (d Door) Width() int {
	return abs(d.A.X-d.B.X) + abs(d.A.Y-d.B.Y)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

type Source struct {
	ID      string  `json:"id"`
	Pos     Point   `json:"pos"`
	LevelDB float64 `json:"level_db"`
}

type Listener struct {
	ID  string `json:"id"`
	Pos Point  `json:"pos"`
}

type ListenerTruth struct {
	ListenerID string `json:"listener_id"`
	RoomID     string `json:"room_id"`
}

type PathTruth struct {
	SourceID   string    `json:"source_id"`
	ListenerID string    `json:"listener_id"`
	RoomID     string    `json:"room_id"`
	RoomChain  []string  `json:"room_chain"`
	DoorChain  []string  `json:"door_chain"`
	Polyline   []Point   `json:"polyline"`
	Length     float64   `json:"length"`
	CurveDB    []float64 `json:"curve_db"`
	SPLDB      float64   `json:"spl_db"`
}

type Truth struct {
	ListenerRooms []ListenerTruth `json:"listener_rooms"`
	Paths         []PathTruth     `json:"paths"`
}

type Level struct {
	SchemaVersion string     `json:"schema_version"`
	Seed          int64      `json:"seed"`
	Name          string     `json:"name"`
	UnitsPerMeter float64    `json:"units_per_meter"`
	Bounds        Rect       `json:"bounds"`
	SampleDT      float64    `json:"sample_dt_seconds"`
	SampleCount   int        `json:"sample_count"`
	Rooms         []Room     `json:"rooms"`
	Walls         []Wall     `json:"walls"`
	Doors         []Door     `json:"doors"`
	Sources       []Source   `json:"sources"`
	Listeners     []Listener `json:"listeners"`
	Truth         Truth      `json:"truth"`
}

type Truncation struct {
	Step      string `json:"step"`
	RoomID    string `json:"room_id,omitempty"`
	PathKey   string `json:"path_key,omitempty"`
	SampleIdx int    `json:"sample_idx,omitempty"`
	At        string `json:"at"`
	Raw       string `json:"raw,omitempty"`
	Truncated string `json:"truncated"`
}

type PathResult struct {
	Key            string       `json:"key"`
	SourceID       string       `json:"source_id"`
	ListenerID     string       `json:"listener_id"`
	ListenerRoomID string       `json:"listener_room_id"`
	RoomChain      []string     `json:"room_chain"`
	DoorChain      []string     `json:"door_chain"`
	Polyline       []Point      `json:"polyline"`
	Length         float64      `json:"length"`
	CurveDB        []float64    `json:"curve_db"`
	SPLDB          float64      `json:"spl_db"`
	WallClear      bool         `json:"wall_clear"`
	Truncations    []Truncation `json:"truncations,omitempty"`
}

type ListenerResult struct {
	ListenerID string       `json:"listener_id"`
	RoomID     string       `json:"room_id"`
	Paths      []PathResult `json:"paths"`
}

type RoomResult struct {
	RoomID          string               `json:"room_id"`
	Revision        int                  `json:"revision"`
	RoomHash        string               `json:"room_hash"`
	DoorHash        string               `json:"door_hash"`
	PathSummary     []string             `json:"path_summary"`
	ListenerIDs     []string             `json:"listener_ids"`
	SPLSummary      []float64            `json:"spl_summary"`
	ParamSnapshot   ReverbParamsSnapshot `json:"param_snapshot"`
	DoorSnapshot    []Door               `json:"door_snapshot"`
	ListenerResults []ListenerResult     `json:"listener_results"`
	ContentHash     string               `json:"content_hash"`
	ParamVersion    string               `json:"param_version"`
	AlgoVersion     string               `json:"algo_version"`
	CommitOrder     int                  `json:"commit_order"`
}

type FullResult struct {
	Paths     []PathResult     `json:"paths"`
	Listeners []ListenerResult `json:"listeners"`
	Rooms     []RoomResult     `json:"rooms"`
	Order     []string         `json:"order"`
}

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	SchemaVersion          = "level-acoustic/v1"
	AlgorithmVersion       = "fixed-acoustic/v1"
	ParameterVersion       = "room-parameter/v1"
	TruthAlgorithmVersion  = "analytic-truth/v1"
	DefaultCurveSamples    = 64
	DefaultCurveStepMicros = 20_000
	DefaultPathTolerance   = int64(1)
	DefaultCurveTolerance  = int64(5_000)
	DefaultSPLTolerance    = int64(5_000)
)

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) Right() int  { return r.X + r.W }
func (r Rect) Bottom() int { return r.Y + r.H }

func (r Rect) ContainsPoint(x, y int) bool {
	return x >= r.X && x <= r.Right() && y >= r.Y && y <= r.Bottom()
}

func (r Rect) ContainsInterior(x, y int) bool {
	return x > r.X && x < r.Right() && y > r.Y && y < r.Bottom()
}

type Room struct {
	ID                string `json:"id"`
	Rect              Rect   `json:"rect"`
	RT60Milliseconds  int64  `json:"rt60_ms"`
	EarlyDensityMilli int64  `json:"early_reflection_density_milli"`
	ReverbGainMilli   int64  `json:"reverb_gain_milli"`
}

type Wall struct {
	ID    string `json:"id"`
	X1    int    `json:"x1"`
	Y1    int    `json:"y1"`
	X2    int    `json:"x2"`
	Y2    int    `json:"y2"`
	RoomA string `json:"room_a"`
	RoomB string `json:"room_b"`
}

func (w Wall) Horizontal() bool { return w.Y1 == w.Y2 }
func (w Wall) Vertical() bool   { return w.X1 == w.X2 }

func (w Wall) Length() int {
	if w.Horizontal() {
		return abs(w.X2 - w.X1)
	}
	return abs(w.Y2 - w.Y1)
}

func (w Wall) Start() int {
	if w.Horizontal() {
		return min(w.X1, w.X2)
	}
	return min(w.Y1, w.Y2)
}

func (w Wall) End() int {
	if w.Horizontal() {
		return max(w.X1, w.X2)
	}
	return max(w.Y1, w.Y2)
}

type Door struct {
	ID     string `json:"id"`
	WallID string `json:"wall_id"`
	Start  int    `json:"start"`
	Width  int    `json:"width"`
}

type Point struct {
	ID string `json:"id"`
	X  int    `json:"x"`
	Y  int    `json:"y"`
}

type Source = Point
type Listener = Point

type Tolerance struct {
	PathLengthMicro int64 `json:"path_length_micro"`
	CurveDBMicro    int64 `json:"curve_db_micro"`
	SPLDBMicro      int64 `json:"spl_db_micro"`
}

func DefaultTolerance() Tolerance {
	return Tolerance{
		PathLengthMicro: DefaultPathTolerance,
		CurveDBMicro:    DefaultCurveTolerance,
		SPLDBMicro:      DefaultSPLTolerance,
	}
}

type Project struct {
	SchemaVersion string     `json:"schema_version"`
	Seed          int64      `json:"seed"`
	Bounds        Rect       `json:"bounds"`
	Rooms         []Room     `json:"rooms"`
	Walls         []Wall     `json:"walls"`
	Doors         []Door     `json:"doors"`
	Sources       []Source   `json:"sources"`
	Listeners     []Listener `json:"listeners"`
	Tolerance     Tolerance  `json:"tolerance"`
	Truth         *Truth     `json:"truth,omitempty"`
}

type EditPlan struct {
	Name string   `json:"name"`
	Ops  []EditOp `json:"ops"`
}

type EditOp struct {
	Op                string `json:"op"`
	RoomID            string `json:"room_id,omitempty"`
	RT60Milliseconds  *int64 `json:"rt60_ms,omitempty"`
	EarlyDensityMilli *int64 `json:"early_reflection_density_milli,omitempty"`
	ReverbGainMilli   *int64 `json:"reverb_gain_milli,omitempty"`
	Door              *Door  `json:"door,omitempty"`
	DoorID            string `json:"door_id,omitempty"`
	Axis              string `json:"axis,omitempty"`
	OldCoord          int    `json:"old_coord,omitempty"`
	NewCoord          int    `json:"new_coord,omitempty"`
	SpanStart         int    `json:"span_start,omitempty"`
	SpanEnd           int    `json:"span_end,omitempty"`
}

type Truncation struct {
	Stage     string `json:"stage"`
	Step      int    `json:"step"`
	Remainder int64  `json:"remainder"`
	Fixed     int64  `json:"fixed"`
}

type TruthSegment struct {
	RoomID      string `json:"room_id"`
	From        string `json:"from"`
	To          string `json:"to"`
	LengthMicro int64  `json:"length_micro"`
}

type TruthEntry struct {
	SourceID         string         `json:"source_id"`
	ListenerID       string         `json:"listener_id"`
	ListenerRoomID   string         `json:"listener_room_id"`
	Reachable        bool           `json:"reachable"`
	RoomChain        []string       `json:"room_chain"`
	DoorChain        []string       `json:"door_chain"`
	PathLengthMicro  int64          `json:"path_length_micro"`
	Segments         []TruthSegment `json:"segments,omitempty"`
	CurveDBMicro     []int64        `json:"curve_db_micro,omitempty"`
	SPLDBMicro       int64          `json:"spl_db_micro"`
	OcclusionDBMicro int64          `json:"occlusion_db_micro"`
}

type Truth struct {
	AlgorithmVersion   string       `json:"algorithm_version"`
	ProjectFingerprint string       `json:"project_fingerprint"`
	CurveSamples       int          `json:"curve_samples"`
	CurveStepMicros    int64        `json:"curve_step_micros"`
	Entries            []TruthEntry `json:"entries"`
}

type ChainSummary struct {
	SourceID        string         `json:"source_id"`
	ListenerID      string         `json:"listener_id"`
	ListenerRoomID  string         `json:"listener_room_id"`
	Reachable       bool           `json:"reachable"`
	RoomChain       []string       `json:"room_chain"`
	DoorChain       []string       `json:"door_chain"`
	PathLengthMicro int64          `json:"path_length_micro"`
	Segments        []TruthSegment `json:"segments,omitempty"`
	Truncations     []Truncation   `json:"truncations,omitempty"`
}

type ListenerResult struct {
	ListenerID       string         `json:"listener_id"`
	SourceID         string         `json:"source_id"`
	ListenerRoomID   string         `json:"listener_room_id"`
	Reachable        bool           `json:"reachable"`
	RoomChain        []string       `json:"room_chain"`
	DoorChain        []string       `json:"door_chain"`
	PathLengthMicro  int64          `json:"path_length_micro"`
	CurveDBMicro     []int64        `json:"curve_db_micro"`
	SPLDBMicro       int64          `json:"spl_db_micro"`
	OcclusionDBMicro int64          `json:"occlusion_db_micro"`
	Segments         []TruthSegment `json:"segments,omitempty"`
	Truncations      []Truncation   `json:"truncations,omitempty"`
}

type ParamSnapshot struct {
	RT60Milliseconds  int64 `json:"rt60_ms"`
	EarlyDensityMilli int64 `json:"early_reflection_density_milli"`
	ReverbGainMilli   int64 `json:"reverb_gain_milli"`
}

func (r Room) Params() ParamSnapshot {
	return ParamSnapshot{
		RT60Milliseconds:  r.RT60Milliseconds,
		EarlyDensityMilli: r.EarlyDensityMilli,
		ReverbGainMilli:   r.ReverbGainMilli,
	}
}

type RoomCheckpoint struct {
	RoomID           string           `json:"room_id"`
	Revision         string           `json:"revision"`
	Params           Room             `json:"params"`
	Doors            []Door           `json:"doors"`
	Sources          []Source         `json:"sources"`
	Listeners        []Listener       `json:"listeners"`
	Chains           []ChainSummary   `json:"chains"`
	Results          []ListenerResult `json:"results"`
	ContentHash      string           `json:"content_hash,omitempty"`
	ParameterVersion string           `json:"parameter_version"`
	AlgorithmVersion string           `json:"algorithm_version"`
	CommitOrder      int              `json:"commit_order"`
}

type RoomState string

const (
	StatePending               RoomState = "pending"
	StateAnalyzingConnectivity RoomState = "analyzing_connectivity"
	StateSolvingParameters     RoomState = "solving_parameters"
	StateCommitted             RoomState = "committed"
	StateFailed                RoomState = "failed"
)

var stateOrder = map[RoomState]int{
	StatePending:               0,
	StateAnalyzingConnectivity: 1,
	StateSolvingParameters:     2,
	StateCommitted:             3,
	StateFailed:                4,
}

func AllStates() []RoomState {
	return []RoomState{
		StatePending,
		StateAnalyzingConnectivity,
		StateSolvingParameters,
		StateCommitted,
		StateFailed,
	}
}

func LegalTransition(from, to RoomState) bool {
	switch from {
	case StatePending:
		return to == StateAnalyzingConnectivity || to == StateFailed
	case StateAnalyzingConnectivity:
		return to == StateSolvingParameters || to == StateFailed
	case StateSolvingParameters:
		return to == StateCommitted || to == StateFailed
	case StateFailed:
		return to == StatePending
	case StateCommitted:
		return false
	default:
		return false
	}
}

type RoomStateSnapshot struct {
	RoomID     string    `json:"room_id"`
	State      RoomState `json:"state"`
	Changes    int       `json:"generation"`
	LastReason string    `json:"last_reason,omitempty"`
}

type ResourceEvent struct {
	JobID         string `json:"job_id"`
	Reason        string `json:"reason"`
	WaitMicros    int64  `json:"wait_micros"`
	WaitMillis    int64  `json:"wait_ms"`
	BufferSamples int    `json:"buffer_samples"`
}

type ResourceSummary struct {
	MaxParallel      int             `json:"max_parallel"`
	BufferCount      int             `json:"buffer_count"`
	MaxBufferSamples int             `json:"max_buffer_samples"`
	WorkerCount      int             `json:"worker_count"`
	Jobs             int             `json:"jobs"`
	QueuedJobs       int             `json:"queued_jobs"`
	TotalWaitMicros  int64           `json:"total_wait_micros"`
	TotalWaitMillis  int64           `json:"total_wait_ms"`
	MaxWaitMillis    int64           `json:"max_wait_ms"`
	Events           []ResourceEvent `json:"events,omitempty"`
}

type InvalidationEntry struct {
	RoomID         string    `json:"room_id"`
	Invalidated    bool      `json:"invalidated"`
	Reused         bool      `json:"reused"`
	Reason         string    `json:"reason"`
	AffectedPairs  []string  `json:"affected_pairs,omitempty"`
	PriorState     RoomState `json:"prior_state"`
	ResultingState RoomState `json:"resulting_state"`
}

type InvalidationReport struct {
	Operation        string              `json:"operation"`
	Revision         string              `json:"revision"`
	Entries          []InvalidationEntry `json:"entries"`
	ReusedRooms      []string            `json:"reused_rooms"`
	InvalidatedRooms []string            `json:"invalidated_rooms"`
}

type SolveResult struct {
	Revision        string              `json:"revision"`
	RoomStates      []RoomStateSnapshot `json:"room_states"`
	Progress        string              `json:"progress"`
	CommittedRooms  int                 `json:"committed_rooms"`
	TotalRooms      int                 `json:"total_rooms"`
	CommitSequence  []string            `json:"commit_sequence"`
	RecomputedRooms []string            `json:"recomputed_rooms"`
	ReusedRooms     []string            `json:"reused_rooms"`
	Invalidation    *InvalidationReport `json:"invalidation,omitempty"`
	RoomCheckpoints []RoomCheckpoint    `json:"room_checkpoints"`
	ListenerResults []ListenerResult    `json:"listener_results"`
	Resources       ResourceSummary     `json:"resources"`
}

func PairID(sourceID, listenerID string) string {
	return sourceID + "/" + listenerID
}

func SortedPairIDs(entries []ChainSummary) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, PairID(e.SourceID, e.ListenerID))
	}
	sort.Strings(out)
	return out
}

func CanonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func ContentHash(v any) (string, error) {
	data, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (cp RoomCheckpoint) HashPayload() (map[string]any, error) {
	cp.ContentHash = ""
	data, err := CanonicalJSON(cp)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (cp RoomCheckpoint) ComputeHash() (string, error) {
	payload, err := cp.HashPayload()
	if err != nil {
		return "", err
	}
	return ContentHash(payload)
}

func (cp *RoomCheckpoint) Seal() error {
	h, err := cp.ComputeHash()
	if err != nil {
		return err
	}
	cp.ContentHash = h
	return nil
}

func (cp RoomCheckpoint) VerifyHash() bool {
	if cp.ContentHash == "" {
		return false
	}
	h, err := cp.ComputeHash()
	return err == nil && h == cp.ContentHash
}

func ProjectFingerprint(p *Project) (string, error) {
	clone := *p
	clone.Truth = nil
	clone.Tolerance = Tolerance{}
	data, err := CanonicalJSON(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func ProjectRevision(p *Project) (string, error) {
	clone := *p
	clone.Truth = nil
	return ContentHash(clone)
}

func (t *Truth) VerifyFingerprint(p *Project) bool {
	if t == nil || t.AlgorithmVersion != TruthAlgorithmVersion {
		return false
	}
	fp, err := ProjectFingerprint(p)
	return err == nil && fp == t.ProjectFingerprint
}

func FindRoom(rooms []Room, id string) (Room, int, bool) {
	for i, room := range rooms {
		if room.ID == id {
			return room, i, true
		}
	}
	return Room{}, -1, false
}

func FindWall(walls []Wall, id string) (Wall, int, bool) {
	for i, wall := range walls {
		if wall.ID == id {
			return wall, i, true
		}
	}
	return Wall{}, -1, false
}

func FindDoor(doors []Door, id string) (Door, int, bool) {
	for i, door := range doors {
		if door.ID == id {
			return door, i, true
		}
	}
	return Door{}, -1, false
}

func FindPoint(points []Point, id string) (Point, int, bool) {
	for i, p := range points {
		if p.ID == id {
			return p, i, true
		}
	}
	return Point{}, -1, false
}

func RoomForPoint(rooms []Room, x, y int) (Room, bool) {
	for _, room := range rooms {
		if room.Rect.ContainsInterior(x, y) {
			return room, true
		}
	}
	return Room{}, false
}

func DoorCenter(w Wall, d Door) Point {
	if w.Horizontal() {
		return Point{ID: d.ID, X: d.Start + d.Width/2, Y: w.Y1}
	}
	return Point{ID: d.ID, X: w.X1, Y: d.Start + d.Width/2}
}

func OtherRoom(w Wall, roomID string) (string, bool) {
	switch roomID {
	case w.RoomA:
		return w.RoomB, true
	case w.RoomB:
		return w.RoomA, true
	default:
		return "", false
	}
}

func FormatFixed(v int64) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%06d", sign, v/1_000_000, v%1_000_000)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

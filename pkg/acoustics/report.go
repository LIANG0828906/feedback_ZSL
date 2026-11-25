package acoustics

// PairResult is one source-to-listener acoustic solve.
type PairResult struct {
	SourceID       string          `json:"source_id"`
	ListenerID     string          `json:"listener_id"`
	ListenerRoomID string          `json:"listener_room_id"`
	Status         string          `json:"status"`
	Chain          *Chain          `json:"chain,omitempty"`
	Reflections    []ReflectionHit `json:"reflections"`
	ValidPathCount int             `json:"valid_path_count"`
	ExpectedSPLDB  *float64        `json:"expected_spl_db,omitempty"`
	Error          *DomainError    `json:"error,omitempty"`
}

// RoomResult is the atomic checkpoint/report unit for one room chunk.
type RoomResult struct {
	RoomID            string       `json:"room_id"`
	State             State        `json:"state"`
	CommitIndex       int          `json:"commit_index"`
	ContentHash       string       `json:"content_hash"`
	SurfaceSnapshot   []Reflector  `json:"surface_snapshot"`
	DoorSnapshot      []Door       `json:"door_snapshot"`
	WallSnapshot      []Wall       `json:"wall_snapshot,omitempty"`
	PairResults       []PairResult `json:"pair_results"`
	ChainSummaries    []Chain      `json:"chain_summaries"`
	Committed         bool         `json:"committed"`
	Reused            bool         `json:"reused"`
	SolveMilliseconds int64        `json:"solve_milliseconds,omitempty"`
}

// Report is the complete deterministic output for a window/revision.
type Report struct {
	SchemaVersion     string              `json:"schema_version"`
	Seed              int64               `json:"seed"`
	GlobalHash        string              `json:"global_hash"`
	RoomContentHashes map[string]string   `json:"room_content_hashes"`
	Params            Params              `json:"params"`
	Rooms             []RoomResult        `json:"rooms"`
	Pairs             []PairResult        `json:"pairs"`
	Progress          Progress            `json:"progress"`
	CommitSequence    []string            `json:"commit_sequence"`
	QueueEvents       []QueueEvent        `json:"queue_events,omitempty"`
	Invalidation      *InvalidationReport `json:"invalidation,omitempty"`
	Dependencies      *DependencyGraph    `json:"dependencies,omitempty"`
	Verification      *VerificationReport `json:"verification,omitempty"`
}

// InvalidationItem identifies a precise stale object.
type InvalidationItem struct {
	RoomID   string `json:"room_id"`
	ObjectID string `json:"object_id"`
	Kind     string `json:"kind"`
	Reason   string `json:"reason"`
}

// InvalidationReport is the required stale-scope manifest.
type InvalidationReport struct {
	Items           []InvalidationItem `json:"items"`
	InvalidRooms    []string           `json:"invalid_rooms"`
	RecomputedRooms []string           `json:"recomputed_rooms"`
	ReusedRooms     []string           `json:"reused_rooms"`
	Reason          string             `json:"reason,omitempty"`
}

// DependencyGraph explicitly records solve dependency edges.
type DependencyGraph struct {
	Nodes []DependencyNode `json:"nodes"`
	Edges []DependencyEdge `json:"edges"`
}

type DependencyNode struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	RoomID string `json:"room_id,omitempty"`
	Label  string `json:"label,omitempty"`
}

type DependencyEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

// VerificationReport compares solver output with analytic truth.
type VerificationReport struct {
	OK            bool           `json:"ok"`
	CheckedPairs  int            `json:"checked_pairs"`
	CheckedRooms  int            `json:"checked_rooms"`
	FirstMismatch *DiffMismatch  `json:"first_mismatch,omitempty"`
	Mismatches    []DiffMismatch `json:"mismatches,omitempty"`
}

// DiffMismatch locates the first inconsistent room/field.
type DiffMismatch struct {
	RoomID     string  `json:"room_id"`
	ListenerID string  `json:"listener_id,omitempty"`
	SourceID   string  `json:"source_id,omitempty"`
	Field      string  `json:"field"`
	Expected   any     `json:"expected,omitempty"`
	Actual     any     `json:"actual,omitempty"`
	Difference float64 `json:"difference,omitempty"`
}

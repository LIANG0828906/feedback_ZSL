package domain

type RequestKind string

const (
	RequestRead  RequestKind = "read"
	RequestWrite RequestKind = "write"
)

type Request struct {
	ID      string      `json:"id"`
	Tick    int64       `json:"tick"`
	Kind    RequestKind `json:"kind"`
	ShardID string      `json:"shard_id"`
	Version int64       `json:"version,omitempty"`
	Fresh   bool        `json:"fresh,omitempty"`
	Payload string      `json:"payload,omitempty"`
}

type GateResult struct {
	RequestID       string      `json:"request_id"`
	Tick            int64       `json:"tick"`
	Kind            RequestKind `json:"kind"`
	ShardID         string      `json:"shard_id"`
	Accepted        bool        `json:"accepted"`
	Rejected        bool        `json:"rejected"`
	Source          string      `json:"source"`
	ReturnedVersion int64       `json:"returned_version"`
	VisibleOnEdge   bool        `json:"visible_on_edge"`
	PendingReplay   int         `json:"pending_replay"`
	Reason          string      `json:"reason,omitempty"`
	Code            string      `json:"code,omitempty"`
}

type EditKind string

const (
	EditBumpShard     EditKind = "bump_shard"
	EditRetireNode    EditKind = "retire_node"
	EditJoinNode      EditKind = "join_node"
	EditSetBudget     EditKind = "set_budget"
	EditRetireShard   EditKind = "retire_shard"
	EditSetNodeLimits EditKind = "set_node_limits"
)

type Edit struct {
	ID                 string   `json:"id"`
	Kind               EditKind `json:"kind"`
	AtTick             int64    `json:"at_tick"`
	ShardID            string   `json:"shard_id,omitempty"`
	NodeID             string   `json:"node_id,omitempty"`
	BudgetMilli        int64    `json:"budget_milli,omitempty"`
	Digest             uint64   `json:"digest,omitempty"`
	SourceVersion      int64    `json:"source_version,omitempty"`
	PreviousVersion    int64    `json:"previous_version,omitempty"`
	JoinTick           int64    `json:"join_tick,omitempty"`
	MaxConcurrent      int      `json:"max_concurrent,omitempty"`
	BufferLimit        int      `json:"buffer_limit,omitempty"`
	ClaimIntervalTicks int64    `json:"claim_interval_ticks,omitempty"`
	LeaseTicks         int64    `json:"lease_ticks,omitempty"`
}

type Options struct {
	Ticks            int64       `json:"ticks"`
	Workers          int         `json:"workers"`
	MaxWorkers       int         `json:"max_workers"`
	MaxBufferPerNode int         `json:"max_buffer_per_node"`
	Interrupts       []Interrupt `json:"interrupts,omitempty"`
	Edits            []Edit      `json:"edits,omitempty"`
	FullRecompute    bool        `json:"full_recompute,omitempty"`
}

type Interrupt struct {
	ID         string `json:"id"`
	Tick       int64  `json:"tick"`
	ShardID    string `json:"shard_id"`
	Reason     string `json:"reason"`
	ExitSignal bool   `json:"exit_signal,omitempty"`
}

type LeaseRecord struct {
	LeaseID       string `json:"lease_id"`
	ShardID       string `json:"shard_id"`
	NodeID        string `json:"node_id"`
	AcquiredTick  int64  `json:"acquired_tick"`
	ExpiresTick   int64  `json:"expires_tick"`
	ReleasedTick  int64  `json:"released_tick,omitempty"`
	ReleaseReason string `json:"release_reason,omitempty"`
	Generation    int    `json:"generation"`
}

type ShardState struct {
	ShardID           string   `json:"shard_id"`
	NodeID            string   `json:"node_id"`
	TargetVersion     int64    `json:"target_version"`
	VisibleVersion    int64    `json:"visible_version"`
	SourceVersion     int64    `json:"source_version"`
	Progress          int64    `json:"progress"`
	CommittedProgress int64    `json:"committed_progress"`
	PreheatTicks      int64    `json:"preheat_ticks"`
	Available         bool     `json:"available"`
	LeaseID           string   `json:"lease_id,omitempty"`
	LeaseNode         string   `json:"lease_node,omitempty"`
	LeaseExpiresTick  int64    `json:"lease_expires_tick,omitempty"`
	Generation        int      `json:"generation"`
	Rebirths          int      `json:"rebirths"`
	QueuePosition     int      `json:"queue_position,omitempty"`
	QueueConstraint   string   `json:"queue_constraint,omitempty"`
	PendingReplay     int      `json:"pending_replay"`
	ReplayQueue       []string `json:"replay_queue,omitempty"`
	LastCommittedTick int64    `json:"last_committed_tick,omitempty"`
	Retired           bool     `json:"retired,omitempty"`
}

type NodeTickState struct {
	NodeID        string   `json:"node_id"`
	Active        bool     `json:"active"`
	Holdings      []string `json:"holdings"`
	ActiveShards  []string `json:"active_shards"`
	UsedBandwidth int64    `json:"used_bandwidth_milli"`
	Concurrency   int      `json:"concurrency"`
	PendingBuffer int      `json:"pending_buffer"`
	Retired       bool     `json:"retired"`
}

type QueueEntry struct {
	ShardID    string `json:"shard_id"`
	NodeID     string `json:"node_id"`
	Position   int    `json:"position"`
	Constraint string `json:"constraint"`
	SinceTick  int64  `json:"since_tick"`
}

type TickState struct {
	Tick           int64           `json:"tick"`
	Nodes          []NodeTickState `json:"nodes"`
	Shards         []ShardState    `json:"shards"`
	Queue          []QueueEntry    `json:"queue"`
	Commits        []string        `json:"commits,omitempty"`
	LeasesAcquired []LeaseRecord   `json:"leases_acquired,omitempty"`
	LeasesReleased []LeaseRecord   `json:"leases_released,omitempty"`
	Requests       []GateResult    `json:"requests,omitempty"`
	UsedBandwidth  int64           `json:"used_bandwidth_milli"`
	Capacity       int64           `json:"capacity_milli"`
	CacheHit       bool            `json:"cache_hit,omitempty"`
	Invalidation   []Invalidation  `json:"invalidation,omitempty"`
	Issues         []Issue         `json:"issues,omitempty"`
}

type Invalidation struct {
	EditID       string   `json:"edit_id"`
	Kind         EditKind `json:"kind"`
	Source       string   `json:"source"`
	Closure      []string `json:"closure"`
	AtTick       int64    `json:"at_tick"`
	ReusedBefore int64    `json:"reused_before_tick"`
}

type NodeDiagnostic struct {
	NodeID     string  `json:"node_id"`
	ObservedMS float64 `json:"observed_ms"`
	WorkerSlot int     `json:"worker_slot"`
	TickOps    int     `json:"tick_ops"`
}

type Diagnostics struct {
	WorkersRequested int              `json:"workers_requested"`
	WorkersUsed      int              `json:"workers_used"`
	MaxWorkers       int              `json:"max_workers"`
	Nodes            []NodeDiagnostic `json:"nodes"`
	Backpressure     []Issue          `json:"backpressure,omitempty"`
	RecomputedTicks  []int64          `json:"recomputed_ticks,omitempty"`
	ReusedTicks      int              `json:"reused_ticks"`
}

type RunResult struct {
	RunID          string        `json:"run_id"`
	Schema         SchemaVersion `json:"schema"`
	SemanticHash   string        `json:"semantic_hash"`
	Project        Project       `json:"project"`
	Edits          []Edit        `json:"edits,omitempty"`
	InputRequests  []Request     `json:"input_requests,omitempty"`
	Timeline       []TickState   `json:"timeline"`
	Leases         []LeaseRecord `json:"leases"`
	Shards         []ShardState  `json:"shards"`
	Requests       []GateResult  `json:"requests"`
	Diagnostics    Diagnostics   `json:"diagnostics"`
	Backpressure   []Issue       `json:"backpressure,omitempty"`
	Issues         []Issue       `json:"issues,omitempty"`
	Incremental    bool          `json:"incremental"`
	ReusedTicks    int           `json:"reused_ticks"`
	MatchesFull    bool          `json:"matches_full,omitempty"`
	FullComparison string        `json:"full_comparison,omitempty"`
	StoppedAtTick  int64         `json:"stopped_at_tick,omitempty"`
	Interrupted    bool          `json:"interrupted,omitempty"`
}

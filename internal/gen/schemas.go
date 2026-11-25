package gen

// This file defines the three on-disk schema dialects of the same logical
// project. The loader maps each dialect onto one identical model.Project.

// ---------- Schema v1: ticks, absolute concurrency, flat warm pool ----------

// SnapshotV1 uses tick durations, absolute concurrency limits, a flat warm
// pool table and a per-mille (‰) optional cold budget.
type SnapshotV1 struct {
	SchemaVersion   string           `json:"schema_version"` // "coldkernel/v1"
	TickUnit        string           `json:"tick_unit"`      // "tick"
	TickMs          int64            `json:"tick_ms"`
	HorizonTicks    int64            `json:"horizon_ticks"`
	Seed            uint64           `json:"seed"`
	FixedWidth      int              `json:"fixed_width"`
	Tolerance       int64            `json:"tolerance"`
	Functions       []V1Function     `json:"functions"`
	WarmPool        []V1WarmEntry    `json:"warm_pool"` // flat table
	Requests        []V1Request      `json:"requests"`
}

type V1Function struct {
	FunctionID       string `json:"function_id"`
	ConcurrencyLimit int64  `json:"concurrency_limit"` // absolute
	ReservedSlots    int64  `json:"reserved_slots"`
	ColdMinTicks     int64  `json:"cold_min_ticks"`
	ColdMaxTicks     int64  `json:"cold_max_ticks"`
	ColdTimeoutTicks int64  `json:"cold_timeout_ticks"`
	ColdBudgetPermille *int64 `json:"cold_budget_permille,omitempty"` // optional ‰
	QueueLimit       int64  `json:"queue_limit"`
	WaitLimitTicks   int64  `json:"wait_limit_ticks"`
}

type V1WarmEntry struct {
	FunctionID string `json:"function_id"`
	InstanceID string `json:"instance_id"`
}

type V1Request struct {
	RequestID  string `json:"request_id"`
	FunctionID string `json:"function_id"`
	ArriveTick int64  `json:"arrive_tick"`
	Slots      int64  `json:"slots"`
	Priority   string `json:"priority"` // RESERVED / SHARED
	Tenant     string `json:"tenant"`
	HoldTicks  int64  `json:"hold_ticks"`
}

// ---------- Schema v2: milliseconds, remaining concurrency, grouped pool ----

// SnapshotV2 expresses concurrency as "remaining available" slots instead of
// absolute limits, times in milliseconds and the warm pool as a nested tree.
type SnapshotV2 struct {
	SchemaVersion string       `json:"schema_version"` // "coldkernel/v2"
	TimeUnit      string       `json:"time_unit"`      // "millisecond"
	TickMs        int64        `json:"tick_ms"`
	HorizonMs     int64        `json:"horizon_ms"`
	Seed          uint64       `json:"seed"`
	FixedWidth    int          `json:"fixed_width"`
	Tolerance     int64        `json:"tolerance"`
	Functions     []V2Function `json:"functions"`
	WarmPools     []V2Pool     `json:"warm_pools"` // grouped nested tree
	Requests      []V2Request  `json:"requests"`
}

type V2Function struct {
	FunctionID string `json:"function_id"`
	// RemainingAvailable = absolute limit - currently occupied baseline.
	// The baseline is always 0 at load time, so absolute == remaining here;
	// the field still exercises the reverse mapping in the loader.
	RemainingAvailable int64  `json:"remaining_available"`
	BaselineOccupied   int64  `json:"baseline_occupied"`
	ReservedSlots      int64  `json:"reserved_slots"`
	ColdMinMs          int64  `json:"cold_min_ms"`
	ColdMaxMs          int64  `json:"cold_max_ms"`
	ColdTimeoutMs      int64  `json:"cold_timeout_ms"`
	ColdBudgetPermyriad *int64 `json:"cold_budget_permyriad,omitempty"` // ‱
	QueueLimit         int64  `json:"queue_limit"`
	WaitLimitMs        int64  `json:"wait_limit_ms"`
}

type V2Pool struct {
	FunctionID string   `json:"function_id"`
	Instances  []string `json:"instances"`
}

type V2Request struct {
	RequestID  string `json:"request_id"`
	FunctionID string `json:"function_id"`
	ArriveMs   int64  `json:"arrive_ms"`
	Slots      int64  `json:"slots"`
	Class      string `json:"class"` // RESERVED / SHARED
	Tenant     string `json:"tenant"`
	HoldMs     int64  `json:"hold_ms"`
}

// ---------- Schema v3: decimal seconds, absolute limit, grouped pool --------

// SnapshotV3 uses integer-milli second strings for durations and a per-ten-
// thousand budget. Seconds are encoded as scaled integers (3 decimals) to
// avoid floating point entirely.
type SnapshotV3 struct {
	SchemaVersion string       `json:"schema_version"` // "coldkernel/v3"
	TimeUnit      string       `json:"time_unit"`      // "second_milli"
	TickMs        int64        `json:"tick_ms"`
	HorizonSecMilli int64      `json:"horizon_sec_milli"` // seconds * 1000
	Seed          uint64       `json:"seed"`
	FixedWidth    int          `json:"fixed_width"`
	Tolerance     int64        `json:"tolerance"`
	Functions     []V3Function `json:"functions"`
	WarmPools     []V2Pool     `json:"warm_pools"` // nested tree reused
	Requests      []V3Request  `json:"requests"`
}

type V3Function struct {
	FunctionID       string `json:"function_id"`
	ConcurrencyLimit int64  `json:"concurrency_limit"` // absolute
	ReservedSlots    int64  `json:"reserved_slots"`
	ColdMinSecMilli  int64  `json:"cold_min_sec_milli"`
	ColdMaxSecMilli  int64  `json:"cold_max_sec_milli"`
	ColdTimeoutSecMilli int64 `json:"cold_timeout_sec_milli"`
	ColdBudgetPermyriad *int64 `json:"cold_budget_permyriad,omitempty"`
	QueueLimit       int64  `json:"queue_limit"`
	WaitLimitSecMilli int64 `json:"wait_limit_sec_milli"`
}

type V3Request struct {
	RequestID    string `json:"request_id"`
	FunctionID   string `json:"function_id"`
	ArriveSecMilli int64 `json:"arrive_sec_milli"`
	Slots        int64  `json:"slots"`
	Priority     string `json:"priority"`
	Tenant       string `json:"tenant"`
	HoldSecMilli int64  `json:"hold_sec_milli"`
}

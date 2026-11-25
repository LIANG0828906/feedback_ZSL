package domain

type TenantObservation struct {
	TenantID              string   `json:"tenant_id"`
	Weight                int64    `json:"weight"`
	QuotaMilli            int64    `json:"quota_milli"`
	UsedMilli             int64    `json:"used_milli"`
	AvailableMilli        int64    `json:"available_milli"`
	ReserveMilli          int64    `json:"reserve_milli"`
	ReserveRemainingMilli int64    `json:"reserve_remaining_milli"`
	EncroachedMilli       int64    `json:"encroached_milli"`
	WaitingMilli          int64    `json:"waiting_milli"`
	BlockedBy             []string `json:"blocked_by,omitempty"`
}

type NodeObservation struct {
	NodeID               string `json:"node_id"`
	CapacityMilli        int64  `json:"capacity_milli"`
	UsedMilli            int64  `json:"used_milli"`
	NonReservedUsedMilli int64  `json:"non_reserved_used_milli"`
	NonReservedFreeMilli int64  `json:"non_reserved_free_milli"`
	PausedIncomingMilli  int64  `json:"paused_incoming_milli"`
	Failed               bool   `json:"failed"`
}

type PendingObservation struct {
	ID          string   `json:"id"`
	TenantID    string   `json:"tenant_id"`
	NodeID      string   `json:"node_id"`
	AmountMilli int64    `json:"amount_milli"`
	Reason      string   `json:"reason"`
	BlockedBy   []string `json:"blocked_by,omitempty"`
}

type MigrationObservation struct {
	ID          string   `json:"id"`
	TenantID    string   `json:"tenant_id"`
	FromNode    string   `json:"from_node"`
	ToNode      string   `json:"to_node"`
	AmountMilli int64    `json:"amount_milli"`
	StartTick   int64    `json:"start_tick"`
	EndTick     int64    `json:"end_tick"`
	Status      string   `json:"status"`
	Constraints []string `json:"constraints"`
	EnableBasis string   `json:"enable_basis"`
}

type PauseObservation struct {
	TenantID        string `json:"tenant_id"`
	FromNode        string `json:"from_node"`
	ToNode          string `json:"to_node"`
	StartTick       int64  `json:"start_tick"`
	ExpectedEndTick int64  `json:"expected_end_tick"`
	ReserveMilli    int64  `json:"reserve_milli"`
	EnableBasis     string `json:"enable_basis"`
}

type TickObservation struct {
	Tick            int64                  `json:"tick"`
	Tenants         []TenantObservation    `json:"tenants"`
	Nodes           []NodeObservation      `json:"nodes"`
	Migrations      []MigrationObservation `json:"migrations"`
	Pauses          []PauseObservation     `json:"pauses"`
	Pending         []PendingObservation   `json:"pending"`
	EncroachedTotal int64                  `json:"encroached_total_milli"`
}

type Truth struct {
	Seed               int64                  `json:"seed"`
	ProjectID          string                 `json:"project_id"`
	ProjectFingerprint string                 `json:"project_fingerprint"`
	Ticks              []TickObservation      `json:"ticks"`
	Migrations         []MigrationObservation `json:"migrations"`
	PauseHistory       []PauseObservation     `json:"pause_history"`
}

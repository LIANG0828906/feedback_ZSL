package session

const (
	SchemaV1 = "v1"
	SchemaV2 = "v2"
	SchemaV3 = "v3"

	ScaleBasePermyriad int64 = 10000
	ScaleBasePermille  int64 = 1000
	DeclaredFrameRate  int64 = 60

	MinThreads = 1
	MaxThreads = 64
	// MaxBufferBudget is deliberately generous but finite so malformed wire
	// values cannot silently wrap.
	MaxBufferBudget int64 = 1 << 40
)

// DeclaredScaleLadderPermyriad contains the only legal downgrade landing
// rungs. Values are parts per ten thousand.
var DeclaredScaleLadderPermyriad = []int64{10000, 8750, 7500, 6250, 5000, 3750, 2500}

type Rect struct {
	X      int64 `json:"x"`
	Y      int64 `json:"y"`
	Width  int64 `json:"width"`
	Height int64 `json:"height"`
}

type ChromaFormat string

const (
	Chroma444 ChromaFormat = "444"
	Chroma422 ChromaFormat = "422"
	Chroma420 ChromaFormat = "420"
)

type ChromaInfo struct {
	Format             ChromaFormat `json:"format"`
	PlaneEnum          string       `json:"plane_enum"`
	BitsPerPixel       int64        `json:"bits_per_pixel"`
	LumaBitsPerPixel   int64        `json:"luma_bits_per_pixel"`
	ChromaBitsPerPixel int64        `json:"chroma_bits_per_pixel"`
	BitDepth           int64        `json:"bit_depth"`
	MinUnitWidth       int64        `json:"min_unit_width"`
	MinUnitHeight      int64        `json:"min_unit_height"`
	WidthDivisor       int64        `json:"width_divisor"`
	HeightDivisor      int64        `json:"height_divisor"`
}

type Screen struct {
	ID                string       `json:"id"`
	Host              string       `json:"host"`
	Width             int64        `json:"width"`
	Height            int64        `json:"height"`
	ScalePermyriad    int64        `json:"scale_permyriad"`
	MinScalePermyriad int64        `json:"min_scale_permyriad"`
	Chroma            ChromaFormat `json:"chroma"`
	Priority          int64        `json:"priority"`
}

type EventType string

const (
	EventJoin     EventType = "join"
	EventLeave    EventType = "leave"
	EventTopology EventType = "topology"
	EventBudget   EventType = "budget"
	EventUpdate   EventType = "update"
)

type ScreenUpdate struct {
	ScalePermyriad    *int64        `json:"scale_permyriad,omitempty"`
	MinScalePermyriad *int64        `json:"min_scale_permyriad,omitempty"`
	Chroma            *ChromaFormat `json:"chroma,omitempty"`
}

type TopologyHost struct {
	ID        string   `json:"id"`
	ScreenIDs []string `json:"screen_ids"`
}

type Topology struct {
	Version int64          `json:"version"`
	Hosts   []TopologyHost `json:"hosts"`
}

type Event struct {
	ID       string        `json:"id"`
	Tick     int64         `json:"tick"`
	Type     EventType     `json:"type"`
	ScreenID string        `json:"screen_id,omitempty"`
	Budget   int64         `json:"budget_kbps,omitempty"`
	Topology *Topology     `json:"topology,omitempty"`
	Update   *ScreenUpdate `json:"update,omitempty"`
}

type Project struct {
	SchemaVersion string   `json:"schema_version"`
	ProjectID     string   `json:"project_id"`
	Seed          int64    `json:"seed"`
	Master        Rect     `json:"master"`
	TotalBudget   int64    `json:"total_budget_kbps"`
	Threads       int      `json:"threads"`
	BufferBudget  int64    `json:"buffer_budget_units"`
	Screens       []Screen `json:"screens"`
	Events        []Event  `json:"events"`
}

type Mapping struct {
	Source              Rect         `json:"source"`
	Destination         Rect         `json:"destination"`
	ScaledWidth         int64        `json:"scaled_width"`
	ScaledHeight        int64        `json:"scaled_height"`
	PlaneWidth          int64        `json:"plane_width"`
	PlaneHeight         int64        `json:"plane_height"`
	Chroma              ChromaFormat `json:"chroma"`
	ChromaBitsPerPixel  int64        `json:"chroma_bits_per_pixel"`
	NominalBitsPerPixel int64        `json:"nominal_bits_per_pixel"`
	FrameBits           int64        `json:"frame_bits"`
	BudgetCostKbps      int64        `json:"budget_cost_kbps"`
}

type Allocation struct {
	ScreenID        string  `json:"screen_id"`
	TopologyVersion int64   `json:"topology_version"`
	RequestedScale  int64   `json:"requested_scale_permyriad"`
	SettledScale    int64   `json:"settled_scale_permyriad"`
	Mapping         Mapping `json:"mapping"`
	BudgetCostKbps  int64   `json:"budget_cost_kbps"`
	CommittedTick   int64   `json:"committed_tick"`
}

type Decision string

const (
	DecisionActive     Decision = "active"
	DecisionAdmitted   Decision = "admitted"
	DecisionDowngraded Decision = "downgraded"
	DecisionRejected   Decision = "rejected"
	DecisionUnchanged  Decision = "unchanged"
	DecisionLeft       Decision = "left"
	DecisionEvicted    Decision = "evicted"
	DecisionPending    Decision = "pending"
	DecisionUpdated    Decision = "updated"
)

type ScreenResult struct {
	Tick                int64    `json:"tick"`
	ScreenID            string   `json:"screen_id"`
	Host                string   `json:"host"`
	Active              bool     `json:"active"`
	Decision            Decision `json:"decision"`
	Reason              string   `json:"reason,omitempty"`
	Constraint          string   `json:"constraint,omitempty"`
	TriggerSource       string   `json:"trigger_source,omitempty"`
	ErrorCode           string   `json:"error_code,omitempty"`
	RequestedScale      int64    `json:"requested_scale_permyriad"`
	SettledScale        int64    `json:"settled_scale_permyriad,omitempty"`
	RequestedMapping    *Mapping `json:"requested_mapping,omitempty"`
	Mapping             *Mapping `json:"mapping,omitempty"`
	BudgetCostKbps      int64    `json:"budget_cost_kbps"`
	BudgetMarginKbps    int64    `json:"budget_margin_kbps,omitempty"`
	BudgetShortfallKbps int64    `json:"budget_shortfall_kbps,omitempty"`
	RemainingBudget     int64    `json:"remaining_budget_kbps"`
	JoinVerdict         Decision `json:"join_verdict,omitempty"`
	JoinScale           int64    `json:"join_scale_permyriad,omitempty"`
	JoinCost            int64    `json:"join_cost_kbps,omitempty"`
}

type TickResult struct {
	Tick            int64          `json:"tick"`
	TopologyVersion int64          `json:"topology_version"`
	TotalBudget     int64          `json:"total_budget_kbps"`
	UsedBudget      int64          `json:"used_budget_kbps"`
	RemainingBudget int64          `json:"remaining_budget_kbps"`
	Results         []ScreenResult `json:"results"`
}

type TruthFile struct {
	SchemaVersion string       `json:"schema_version"`
	ProjectID     string       `json:"project_id"`
	Seed          int64        `json:"seed"`
	Tolerances    Tolerances   `json:"tolerances"`
	Ticks         []TickResult `json:"ticks"`
}

type Tolerances struct {
	RectanglePixels int64 `json:"rectangle_pixels"`
	PlanePixels     int64 `json:"plane_pixels"`
	BudgetKbps      int64 `json:"budget_kbps"`
}

func DeclaredTolerances() Tolerances {
	return Tolerances{RectanglePixels: 0, PlanePixels: 0, BudgetKbps: 0}
}

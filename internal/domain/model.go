package domain

import "sort"

const (
	SchemaV1 = "maintenance.project/v1"
	SchemaV2 = "maintenance.project/v2"
	SchemaV3 = "maintenance.project/v3"

	CanonicalTimeBase = "canonical_logical_tick"

	StateOnline      = "online"
	StateMaintenance = "maintenance"

	EventMarkPending       = "marked_pending"
	EventMigrationComplete = "migration_complete"
	EventReturned          = "returned"
	EventLoanStart         = "loan_start"
	EventLoanRecalled      = "loan_recalled"
	EventDegradation       = "degradation"
)

type Tick int64

type Window struct {
	Start Tick `json:"start"`
	End   Tick `json:"end"`
}

func (w Window) Contains(t Tick) bool {
	return t >= w.Start && t < w.End
}

type Policies struct {
	MaxConcurrentMigrations int    `json:"max_concurrent_migrations"`
	MaxMigrationTicks       Tick   `json:"max_migration_ticks"`
	ThreadBudget            int    `json:"thread_budget"`
	PerNodeQueueLimit       int    `json:"per_node_queue_limit"`
	CheckpointVersion       int    `json:"checkpoint_version"`
	RecallRule              string `json:"recall_rule"`
}

type Node struct {
	ID                   string `json:"id"`
	CapacityMilli        int64  `json:"capacity_milli"`
	InitialUsedMilli     int64  `json:"initial_used_milli"`
	WatermarkBasisPoints int    `json:"watermark_basis_points"`
	BorrowCapMilli       int64  `json:"borrow_cap_milli"`
	State                string `json:"state"`
	Window               Window `json:"window"`
}

func (n Node) SafeLimitMilli() int64 {
	return n.CapacityMilli * int64(n.WatermarkBasisPoints) / 10000
}

func (n Node) WatermarkHeadroom(usedMilli int64) int64 {
	return n.SafeLimitMilli() - usedMilli
}

type Instance struct {
	ID                string `json:"id"`
	NodeID            string `json:"node_id"`
	MilliCores        int64  `json:"milli_cores"`
	Priority          int    `json:"priority"`
	DeclaredOrder     int    `json:"declared_order"`
	MigrationTicks    Tick   `json:"migration_ticks"`
	ReturnExempt      bool   `json:"return_exempt"`
	DegradeMilliCores int64  `json:"degrade_milli_cores"`
}

type Batch struct {
	ID            string   `json:"id"`
	Wave          int      `json:"wave"`
	NodeIDs       []string `json:"node_ids"`
	Priority      int      `json:"priority"`
	DeclaredOrder int      `json:"declared_order"`
	Forced        bool     `json:"forced"`
	ForceDeclared bool     `json:"force_declared"`
}

type Wave struct {
	Seq      int      `json:"seq"`
	BatchIDs []string `json:"batch_ids"`
}

type MaintenanceEvent struct {
	ID     string `json:"id"`
	NodeID string `json:"node_id"`
	Kind   string `json:"kind"`
	Tick   Tick   `json:"tick"`
}

type LoanSpec struct {
	ID             string `json:"id"`
	LotID          string `json:"lot_id"`
	LenderNodeID   string `json:"lender_node_id"`
	BorrowerNodeID string `json:"borrower_node_id"`
	AmountMilli    int64  `json:"amount_milli"`
	StartTick      Tick   `json:"start_tick"`
	ExpiryTick     Tick   `json:"expiry_tick"`
	Priority       int    `json:"priority"`
	DeclaredOrder  int    `json:"declared_order"`
	RecallRule     string `json:"recall_rule"`
}

type Project struct {
	SchemaVersion string             `json:"schema_version"`
	ProjectID     string             `json:"project_id"`
	Seed          int64              `json:"seed"`
	TimeBase      string             `json:"time_base"`
	ForceArmed    bool               `json:"force_armed"`
	Window        Window             `json:"window"`
	ProbeTicks    []Tick             `json:"probe_ticks"`
	Policies      Policies           `json:"policies"`
	Nodes         []Node             `json:"nodes"`
	Instances     []Instance         `json:"instances"`
	Batches       []Batch            `json:"batches"`
	Waves         []Wave             `json:"waves"`
	Events        []MaintenanceEvent `json:"events"`
	Loans         []LoanSpec         `json:"loans"`
}

func (p *Project) Node(id string) (Node, bool) {
	for _, n := range p.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func (p *Project) InstancesForNode(nodeID string) []Instance {
	var out []Instance
	for _, in := range p.Instances {
		if in.NodeID == nodeID {
			out = append(out, in)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].DeclaredOrder < out[j].DeclaredOrder
	})
	return out
}

func (p *Project) Batch(id string) (Batch, bool) {
	for _, b := range p.Batches {
		if b.ID == id {
			return b, true
		}
	}
	return Batch{}, false
}

func SortedNodeIDs(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	sort.Strings(out)
	return out
}

func CloneProject(in Project) Project {
	out := in
	out.ProbeTicks = append([]Tick(nil), in.ProbeTicks...)
	out.Nodes = append([]Node(nil), in.Nodes...)
	out.Instances = append([]Instance(nil), in.Instances...)
	out.Batches = append([]Batch(nil), in.Batches...)
	out.Waves = append([]Wave(nil), in.Waves...)
	out.Events = append([]MaintenanceEvent(nil), in.Events...)
	out.Loans = append([]LoanSpec(nil), in.Loans...)
	for i := range out.Batches {
		out.Batches[i].NodeIDs = append([]string(nil), in.Batches[i].NodeIDs...)
	}
	for i := range out.Waves {
		out.Waves[i].BatchIDs = append([]string(nil), in.Waves[i].BatchIDs...)
	}
	return out
}

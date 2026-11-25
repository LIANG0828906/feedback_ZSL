package schema

import (
	"encoding/json"

	"maintenanceorchestrator/internal/domain"
)

type V1Document struct {
	Schema            string          `json:"schema"`
	ProjectID         string          `json:"project_id"`
	Seed              int64           `json:"seed"`
	TimeBase          string          `json:"time_base"`
	ForceArmed        bool            `json:"force_maintenance_armed"`
	Window            V1Window        `json:"window"`
	ProbeLogicalTicks []int64         `json:"probe_logical_ticks"`
	Policies          V1Policies      `json:"policies"`
	Hosts             []V1Host        `json:"hosts"`
	Workloads         []V1Workload    `json:"workloads"`
	BatchSchedule     V1BatchSchedule `json:"batch_schedule"`
	MaintenanceEvents []V1Event       `json:"maintenance_events"`
	CapacityLeases    []V1Lease       `json:"capacity_leases"`
}

type V1Window struct {
	StartLogicalTick  int64 `json:"start_logical_tick"`
	FinishLogicalTick int64 `json:"finish_logical_tick"`
}

type V1Policies struct {
	MaxConcurrentMigrations int    `json:"max_concurrent_migrations"`
	MaxMigrationTicks       int64  `json:"max_migration_ticks"`
	ThreadBudget            int    `json:"thread_budget"`
	NodeQueueLimit          int    `json:"node_queue_limit"`
	CheckpointVersion       int    `json:"checkpoint_version"`
	RecallRule              string `json:"recall_rule"`
}

type V1Host struct {
	HostID            string    `json:"host_id"`
	CoreCapacity      int64     `json:"core_capacity"`
	InitialUsedCores  int64     `json:"initial_used_cores"`
	WatermarkPermille int       `json:"watermark_permille"`
	BorrowCoreLimit   int64     `json:"borrow_core_limit"`
	State             string    `json:"state"`
	MaintenanceWindow *V1Window `json:"maintenance_window,omitempty"`
}

type V1Workload struct {
	WorkloadID     string `json:"workload_id"`
	HostID         string `json:"host_id"`
	Cores          int64  `json:"cores"`
	Priority       int    `json:"priority"`
	DeclaredOrder  int    `json:"declared_order"`
	MigrationTicks int64  `json:"migration_ticks"`
	ReturnExempt   bool   `json:"return_exempt"`
	DegradeCores   int64  `json:"degrade_cores"`
}

type V1BatchSchedule struct {
	FlatBatches []V1Batch `json:"flat_batches"`
}

type V1Batch struct {
	BatchID       string   `json:"batch_id"`
	WaveSequence  int      `json:"wave_sequence"`
	HostIDs       []string `json:"host_ids"`
	Priority      int      `json:"priority"`
	DeclaredOrder int      `json:"declared_order"`
	ForceMarker   bool     `json:"force_marker"`
}

type V1Event struct {
	EventID      string `json:"event_id"`
	HostID       string `json:"host_id"`
	Kind         string `json:"kind"`
	LogicalTick  *int64 `json:"logical_tick,omitempty"`
	WindowOffset *int64 `json:"window_offset,omitempty"`
}

type V1Lease struct {
	LeaseID        string `json:"lease_id"`
	LotID          string `json:"lot_id"`
	LenderHostID   string `json:"lender_host_id"`
	BorrowerHostID string `json:"borrower_host_id"`
	Cores          int64  `json:"cores"`
	StartTick      *int64 `json:"start_logical_tick,omitempty"`
	StartOffset    *int64 `json:"start_offset,omitempty"`
	ExpiryTick     *int64 `json:"expiry_logical_tick,omitempty"`
	ExpiryOffset   *int64 `json:"expiry_offset,omitempty"`
	Priority       int    `json:"priority"`
	DeclaredOrder  int    `json:"declared_order"`
	RecallRule     string `json:"recall_rule"`
}

func loadV1(data []byte) (*domain.Project, error) {
	var doc V1Document
	if err := decodeVersion(data, &doc); err != nil {
		return nil, err
	}
	p := &domain.Project{
		SchemaVersion: domain.SchemaV1,
		ProjectID:     doc.ProjectID,
		Seed:          doc.Seed,
		TimeBase:      domain.CanonicalTimeBase,
		ForceArmed:    doc.ForceArmed,
		Window: domain.Window{
			Start: domain.Tick(doc.Window.StartLogicalTick),
			End:   domain.Tick(doc.Window.FinishLogicalTick),
		},
		Policies: domain.Policies{
			MaxConcurrentMigrations: doc.Policies.MaxConcurrentMigrations,
			MaxMigrationTicks:       domain.Tick(doc.Policies.MaxMigrationTicks),
			ThreadBudget:            doc.Policies.ThreadBudget,
			PerNodeQueueLimit:       doc.Policies.NodeQueueLimit,
			CheckpointVersion:       doc.Policies.CheckpointVersion,
			RecallRule:              doc.Policies.RecallRule,
		},
	}
	for _, t := range doc.ProbeLogicalTicks {
		p.ProbeTicks = append(p.ProbeTicks, domain.Tick(t))
	}
	for i, h := range doc.Hosts {
		n := domain.Node{
			ID: h.HostID, CapacityMilli: h.CoreCapacity * 1000,
			InitialUsedMilli:     h.InitialUsedCores * 1000,
			WatermarkBasisPoints: h.WatermarkPermille * 10,
			BorrowCapMilli:       h.BorrowCoreLimit * 1000,
			State:                h.State,
		}
		if h.MaintenanceWindow != nil {
			n.Window = domain.Window{Start: domain.Tick(h.MaintenanceWindow.StartLogicalTick), End: domain.Tick(h.MaintenanceWindow.FinishLogicalTick)}
		}
		if n.State == "" {
			n.State = domain.StateOnline
		}
		p.Nodes = append(p.Nodes, n)
		_ = i
	}
	for _, w := range doc.Workloads {
		p.Instances = append(p.Instances, domain.Instance{
			ID: w.WorkloadID, NodeID: w.HostID, MilliCores: w.Cores * 1000,
			Priority: w.Priority, DeclaredOrder: w.DeclaredOrder,
			MigrationTicks: domain.Tick(w.MigrationTicks), ReturnExempt: w.ReturnExempt,
			DegradeMilliCores: w.DegradeCores * 1000,
		})
	}
	waveBatches := map[int][]string{}
	for _, b := range doc.BatchSchedule.FlatBatches {
		p.Batches = append(p.Batches, domain.Batch{
			ID: b.BatchID, Wave: b.WaveSequence, NodeIDs: append([]string(nil), b.HostIDs...),
			Priority: b.Priority, DeclaredOrder: b.DeclaredOrder,
			ForceDeclared: b.ForceMarker, Forced: b.ForceMarker && doc.ForceArmed,
		})
		waveBatches[b.WaveSequence] = append(waveBatches[b.WaveSequence], b.BatchID)
	}
	for seq, ids := range waveBatches {
		p.Waves = append(p.Waves, domain.Wave{Seq: seq, BatchIDs: ids})
	}
	sortWaves(p.Waves)
	for i, e := range doc.MaintenanceEvents {
		tick, err := resolveTick(e.LogicalTick, e.WindowOffset, p.Window.Start,
			"$.maintenance_events["+itoaSchema(i)+"]")
		if err != nil {
			return nil, err
		}
		p.Events = append(p.Events, domain.MaintenanceEvent{ID: e.EventID, NodeID: e.HostID, Kind: e.Kind, Tick: tick})
	}
	for i, l := range doc.CapacityLeases {
		path := "$.capacity_leases[" + itoaSchema(i) + "]"
		start, err := resolveTick(l.StartTick, l.StartOffset, p.Window.Start, domain.JoinPath(path, "start"))
		if err != nil {
			return nil, err
		}
		expiry, err := resolveTick(l.ExpiryTick, l.ExpiryOffset, p.Window.Start, domain.JoinPath(path, "expiry"))
		if err != nil {
			return nil, err
		}
		rule := l.RecallRule
		if rule == "" {
			rule = p.Policies.RecallRule
		}
		p.Loans = append(p.Loans, domain.LoanSpec{
			ID: l.LeaseID, LotID: l.LotID, LenderNodeID: l.LenderHostID,
			BorrowerNodeID: l.BorrowerHostID, AmountMilli: l.Cores * 1000,
			StartTick: start, ExpiryTick: expiry, Priority: l.Priority,
			DeclaredOrder: l.DeclaredOrder, RecallRule: rule,
		})
	}
	return p, nil
}

func EncodeV1(p domain.Project) ([]byte, error) {
	doc := V1Document{
		Schema: domain.SchemaV1, ProjectID: p.ProjectID, Seed: p.Seed,
		TimeBase: "logical_sequence", ForceArmed: p.ForceArmed,
		Window: V1Window{StartLogicalTick: int64(p.Window.Start), FinishLogicalTick: int64(p.Window.End)},
		Policies: V1Policies{
			MaxConcurrentMigrations: p.Policies.MaxConcurrentMigrations,
			MaxMigrationTicks:       int64(p.Policies.MaxMigrationTicks),
			ThreadBudget:            p.Policies.ThreadBudget, NodeQueueLimit: p.Policies.PerNodeQueueLimit,
			CheckpointVersion: p.Policies.CheckpointVersion, RecallRule: p.Policies.RecallRule,
		},
	}
	for _, t := range p.ProbeTicks {
		doc.ProbeLogicalTicks = append(doc.ProbeLogicalTicks, int64(t))
	}
	for _, n := range p.Nodes {
		h := V1Host{HostID: n.ID, CoreCapacity: n.CapacityMilli / 1000,
			InitialUsedCores:  n.InitialUsedMilli / 1000,
			WatermarkPermille: n.WatermarkBasisPoints / 10,
			BorrowCoreLimit:   n.BorrowCapMilli / 1000, State: n.State}
		if n.Window.Start != 0 || n.Window.End != 0 {
			h.MaintenanceWindow = &V1Window{StartLogicalTick: int64(n.Window.Start), FinishLogicalTick: int64(n.Window.End)}
		}
		doc.Hosts = append(doc.Hosts, h)
	}
	for _, in := range p.Instances {
		doc.Workloads = append(doc.Workloads, V1Workload{
			WorkloadID: in.ID, HostID: in.NodeID, Cores: in.MilliCores / 1000,
			Priority: in.Priority, DeclaredOrder: in.DeclaredOrder,
			MigrationTicks: int64(in.MigrationTicks), ReturnExempt: in.ReturnExempt,
			DegradeCores: in.DegradeMilliCores / 1000,
		})
	}
	doc.BatchSchedule.FlatBatches = make([]V1Batch, 0, len(p.Batches))
	for _, b := range p.Batches {
		doc.BatchSchedule.FlatBatches = append(doc.BatchSchedule.FlatBatches, V1Batch{
			BatchID: b.ID, WaveSequence: b.Wave, HostIDs: b.NodeIDs,
			Priority: b.Priority, DeclaredOrder: b.DeclaredOrder, ForceMarker: b.ForceDeclared,
		})
	}
	for _, e := range p.Events {
		tick := int64(e.Tick)
		doc.MaintenanceEvents = append(doc.MaintenanceEvents, V1Event{
			EventID: e.ID, HostID: e.NodeID, Kind: e.Kind, LogicalTick: &tick,
		})
	}
	for _, l := range p.Loans {
		start := int64(l.StartTick)
		expiry := int64(l.ExpiryTick)
		doc.CapacityLeases = append(doc.CapacityLeases, V1Lease{
			LeaseID: l.ID, LotID: l.LotID, LenderHostID: l.LenderNodeID,
			BorrowerHostID: l.BorrowerNodeID, Cores: l.AmountMilli / 1000,
			StartTick: &start, ExpiryTick: &expiry, Priority: l.Priority,
			DeclaredOrder: l.DeclaredOrder, RecallRule: l.RecallRule,
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

package schema

import (
	"encoding/json"

	"maintenanceorchestrator/internal/domain"
)

type V2Document struct {
	Schema        string       `json:"schema"`
	Metadata      V2Metadata   `json:"metadata"`
	TimelineBasis string       `json:"timeline_basis"`
	WindowSpec    V2WindowSpec `json:"maintenance_window"`
	ProbeOffsets  []int64      `json:"probe_offsets"`
	Force         V2Force      `json:"force"`
	RuntimePolicy V2Policy     `json:"runtime_policy"`
	Nodes         []V2Node     `json:"nodes"`
	Instances     []V2Instance `json:"instances"`
	WavePlan      V2WavePlan   `json:"wave_plan"`
	Events        []V2Event    `json:"events"`
	Leases        []V2Lease    `json:"leases"`
}

type V2Metadata struct {
	ProjectName string `json:"project_name"`
	RandomSeed  int64  `json:"random_seed"`
}

type V2WindowSpec struct {
	BaseTick    int64 `json:"base_tick"`
	LengthTicks int64 `json:"length_ticks"`
}

type V2Force struct {
	Armed bool `json:"armed"`
}

type V2Policy struct {
	MaxParallelMigrations int    `json:"max_parallel_migrations"`
	MoveTimeoutTicks      int64  `json:"move_timeout_ticks"`
	Threads               int    `json:"threads"`
	QueuePerNode          int    `json:"queue_per_node"`
	CheckpointVersion     int    `json:"checkpoint_version"`
	RecallRule            string `json:"recall_rule"`
}

type V2NodeWindow struct {
	StartOffset int64 `json:"start_offset"`
	LengthTicks int64 `json:"length_ticks"`
}

type V2Node struct {
	Name                  string        `json:"name"`
	CapacityMillicores    int64         `json:"capacity_millicores"`
	WatermarkBasisPoints  int           `json:"watermark_basis_points"`
	MaxBorrowedMillicores int64         `json:"max_borrowed_millicores"`
	UsedMillicores        int64         `json:"used_millicores"`
	Lifecycle             string        `json:"lifecycle"`
	Span                  *V2NodeWindow `json:"span,omitempty"`
}

type V2Instance struct {
	ID                string `json:"id"`
	NodeName          string `json:"node_name"`
	RequestMillicores int64  `json:"request_millicores"`
	RankPriority      int    `json:"rank_priority"`
	Sequence          int    `json:"sequence"`
	DurationTicks     int64  `json:"duration_ticks"`
	ExemptReturn      bool   `json:"exempt_return"`
	DegradeMillicores int64  `json:"degrade_millicores"`
}

type V2WavePlan struct {
	Waves []V2Wave `json:"waves"`
}

type V2Wave struct {
	WaveNo  int       `json:"wave_no"`
	Batches []V2Batch `json:"batches"`
}

type V2Batch struct {
	BatchName string   `json:"batch_name"`
	NodeNames []string `json:"node_names"`
	Priority  int      `json:"priority"`
	Order     int      `json:"order"`
	ForceFlag bool     `json:"force_flag"`
}

type V2Event struct {
	ID           string `json:"id"`
	NodeName     string `json:"node_name"`
	Type         string `json:"type"`
	AtOffset     *int64 `json:"at_offset_ticks,omitempty"`
	AbsoluteTick *int64 `json:"absolute_tick,omitempty"`
}

type V2Lease struct {
	ID             string `json:"id"`
	Lot            string `json:"lot"`
	LenderName     string `json:"lender_name"`
	BorrowerName   string `json:"borrower_name"`
	Millicores     int64  `json:"millicores"`
	StartOffset    *int64 `json:"start_offset_ticks,omitempty"`
	StartAbsolute  *int64 `json:"start_absolute_tick,omitempty"`
	ExpiryOffset   *int64 `json:"expiry_offset_ticks,omitempty"`
	ExpiryAbsolute *int64 `json:"expiry_absolute_tick,omitempty"`
	Priority       int    `json:"priority"`
	Order          int    `json:"order"`
	RecallRule     string `json:"recall_rule"`
}

func loadV2(data []byte) (*domain.Project, error) {
	var doc V2Document
	if err := decodeVersion(data, &doc); err != nil {
		return nil, err
	}
	base := domain.Tick(doc.WindowSpec.BaseTick)
	p := &domain.Project{
		SchemaVersion: domain.SchemaV2,
		ProjectID:     doc.Metadata.ProjectName,
		Seed:          doc.Metadata.RandomSeed,
		TimeBase:      domain.CanonicalTimeBase,
		ForceArmed:    doc.Force.Armed,
		Window:        domain.Window{Start: base, End: base + domain.Tick(doc.WindowSpec.LengthTicks)},
		Policies: domain.Policies{
			MaxConcurrentMigrations: doc.RuntimePolicy.MaxParallelMigrations,
			MaxMigrationTicks:       domain.Tick(doc.RuntimePolicy.MoveTimeoutTicks),
			ThreadBudget:            doc.RuntimePolicy.Threads,
			PerNodeQueueLimit:       doc.RuntimePolicy.QueuePerNode,
			CheckpointVersion:       doc.RuntimePolicy.CheckpointVersion,
			RecallRule:              doc.RuntimePolicy.RecallRule,
		},
	}
	for _, off := range doc.ProbeOffsets {
		p.ProbeTicks = append(p.ProbeTicks, base+domain.Tick(off))
	}
	for _, n := range doc.Nodes {
		node := domain.Node{
			ID: n.Name, CapacityMilli: n.CapacityMillicores,
			InitialUsedMilli: n.UsedMillicores, WatermarkBasisPoints: n.WatermarkBasisPoints,
			BorrowCapMilli: n.MaxBorrowedMillicores, State: n.Lifecycle,
		}
		if n.Span != nil {
			start := base + domain.Tick(n.Span.StartOffset)
			node.Window = domain.Window{Start: start, End: start + domain.Tick(n.Span.LengthTicks)}
		}
		if node.State == "" {
			node.State = domain.StateOnline
		}
		p.Nodes = append(p.Nodes, node)
	}
	for _, in := range doc.Instances {
		p.Instances = append(p.Instances, domain.Instance{
			ID: in.ID, NodeID: in.NodeName, MilliCores: in.RequestMillicores,
			Priority: in.RankPriority, DeclaredOrder: in.Sequence,
			MigrationTicks: domain.Tick(in.DurationTicks), ReturnExempt: in.ExemptReturn,
			DegradeMilliCores: in.DegradeMillicores,
		})
	}
	for _, w := range doc.WavePlan.Waves {
		wave := domain.Wave{Seq: w.WaveNo}
		for _, b := range w.Batches {
			p.Batches = append(p.Batches, domain.Batch{
				ID: b.BatchName, Wave: w.WaveNo, NodeIDs: append([]string(nil), b.NodeNames...),
				Priority: b.Priority, DeclaredOrder: b.Order,
				ForceDeclared: b.ForceFlag, Forced: b.ForceFlag && doc.Force.Armed,
			})
			wave.BatchIDs = append(wave.BatchIDs, b.BatchName)
		}
		p.Waves = append(p.Waves, wave)
	}
	sortWaves(p.Waves)
	for i, e := range doc.Events {
		tick, err := resolveTick(e.AbsoluteTick, e.AtOffset, base, "$.events["+itoaSchema(i)+"]")
		if err != nil {
			return nil, err
		}
		p.Events = append(p.Events, domain.MaintenanceEvent{ID: e.ID, NodeID: e.NodeName, Kind: e.Type, Tick: tick})
	}
	for i, l := range doc.Leases {
		path := "$.leases[" + itoaSchema(i) + "]"
		start, err := resolveTick(l.StartAbsolute, l.StartOffset, base, domain.JoinPath(path, "start"))
		if err != nil {
			return nil, err
		}
		expiry, err := resolveTick(l.ExpiryAbsolute, l.ExpiryOffset, base, domain.JoinPath(path, "expiry"))
		if err != nil {
			return nil, err
		}
		rule := l.RecallRule
		if rule == "" {
			rule = p.Policies.RecallRule
		}
		p.Loans = append(p.Loans, domain.LoanSpec{
			ID: l.ID, LotID: l.Lot, LenderNodeID: l.LenderName, BorrowerNodeID: l.BorrowerName,
			AmountMilli: l.Millicores, StartTick: start, ExpiryTick: expiry,
			Priority: l.Priority, DeclaredOrder: l.Order, RecallRule: rule,
		})
	}
	return p, nil
}

func EncodeV2(p domain.Project) ([]byte, error) {
	base := int64(p.Window.Start)
	doc := V2Document{
		Schema:        domain.SchemaV2,
		Metadata:      V2Metadata{ProjectName: p.ProjectID, RandomSeed: p.Seed},
		TimelineBasis: "window_relative_offset",
		WindowSpec:    V2WindowSpec{BaseTick: base, LengthTicks: int64(p.Window.End - p.Window.Start)},
		Force:         V2Force{Armed: p.ForceArmed},
		RuntimePolicy: V2Policy{
			MaxParallelMigrations: p.Policies.MaxConcurrentMigrations,
			MoveTimeoutTicks:      int64(p.Policies.MaxMigrationTicks),
			Threads:               p.Policies.ThreadBudget, QueuePerNode: p.Policies.PerNodeQueueLimit,
			CheckpointVersion: p.Policies.CheckpointVersion, RecallRule: p.Policies.RecallRule,
		},
	}
	for _, t := range p.ProbeTicks {
		doc.ProbeOffsets = append(doc.ProbeOffsets, int64(t-p.Window.Start))
	}
	for _, n := range p.Nodes {
		vn := V2Node{Name: n.ID, CapacityMillicores: n.CapacityMilli,
			WatermarkBasisPoints: n.WatermarkBasisPoints, MaxBorrowedMillicores: n.BorrowCapMilli,
			UsedMillicores: n.InitialUsedMilli, Lifecycle: n.State}
		if n.Window.Start != 0 || n.Window.End != 0 {
			vn.Span = &V2NodeWindow{StartOffset: int64(n.Window.Start - p.Window.Start), LengthTicks: int64(n.Window.End - n.Window.Start)}
		}
		doc.Nodes = append(doc.Nodes, vn)
	}
	for _, in := range p.Instances {
		doc.Instances = append(doc.Instances, V2Instance{
			ID: in.ID, NodeName: in.NodeID, RequestMillicores: in.MilliCores,
			RankPriority: in.Priority, Sequence: in.DeclaredOrder,
			DurationTicks: int64(in.MigrationTicks), ExemptReturn: in.ReturnExempt,
			DegradeMillicores: in.DegradeMilliCores,
		})
	}
	for _, w := range p.Waves {
		vw := V2Wave{WaveNo: w.Seq}
		for _, bid := range w.BatchIDs {
			b, ok := p.Batch(bid)
			if !ok {
				continue
			}
			vw.Batches = append(vw.Batches, V2Batch{
				BatchName: b.ID, NodeNames: b.NodeIDs, Priority: b.Priority,
				Order: b.DeclaredOrder, ForceFlag: b.ForceDeclared,
			})
		}
		doc.WavePlan.Waves = append(doc.WavePlan.Waves, vw)
	}
	for _, e := range p.Events {
		off := int64(e.Tick - p.Window.Start)
		doc.Events = append(doc.Events, V2Event{ID: e.ID, NodeName: e.NodeID, Type: e.Kind, AtOffset: &off})
	}
	for _, l := range p.Loans {
		so := int64(l.StartTick - p.Window.Start)
		eo := int64(l.ExpiryTick - p.Window.Start)
		doc.Leases = append(doc.Leases, V2Lease{
			ID: l.ID, Lot: l.LotID, LenderName: l.LenderNodeID, BorrowerName: l.BorrowerNodeID,
			Millicores: l.AmountMilli, StartOffset: &so, ExpiryOffset: &eo,
			Priority: l.Priority, Order: l.DeclaredOrder, RecallRule: l.RecallRule,
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

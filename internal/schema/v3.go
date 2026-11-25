package schema

import (
	"encoding/json"

	"maintenanceorchestrator/internal/domain"
)

type V3Document struct {
	Schema          string       `json:"schema"`
	Header          V3Header     `json:"header"`
	Clock           string       `json:"clock"`
	WindowSpec      V3Window     `json:"window_spec"`
	ProbeMoments    []int64      `json:"probe_moments"`
	ForceGate       V3ForceGate  `json:"force_gate"`
	Execution       V3Execution  `json:"execution"`
	Servers         []V3Server   `json:"servers"`
	Pods            []V3Pod      `json:"pods"`
	MaintenanceTree []V3TreeWave `json:"maintenance_tree"`
	Incidents       []V3Incident `json:"incidents"`
	Loans           []V3Loan     `json:"loans"`
}

type V3Header struct {
	ID   string `json:"id"`
	Seed int64  `json:"seed"`
}

type V3Window struct {
	StartsAtTick  int64 `json:"starts_at_tick"`
	DurationTicks int64 `json:"duration_ticks"`
}

type V3ForceGate struct {
	Enabled bool `json:"enabled"`
}

type V3Execution struct {
	ConcurrencySlots int    `json:"concurrency_slots"`
	MoveDeadline     int64  `json:"move_deadline_ticks"`
	WorkerThreads    int    `json:"worker_threads"`
	QueueDepth       int    `json:"queue_depth_per_server"`
	StateRevision    int    `json:"state_revision"`
	RecallPolicy     string `json:"recall_policy"`
}

type V3Watermark struct {
	PerTenThousand int `json:"per_ten_thousand"`
}

type V3Server struct {
	ServerRef       string      `json:"server_ref"`
	CapacityMcpu    int64       `json:"capacity_mcpu"`
	SafeFill        V3Watermark `json:"safe_fill"`
	BorrowLimitMcpu int64       `json:"borrow_limit_mcpu"`
	OccupiedMcpu    int64       `json:"occupied_mcpu"`
	Status          string      `json:"status"`
}

type V3Pod struct {
	PodRef        string `json:"pod_ref"`
	ServerRef     string `json:"server_ref"`
	Mcpu          int64  `json:"mcpu"`
	PriorityClass int    `json:"priority_class"`
	TieBreakOrder int    `json:"tie_break_order"`
	MoveTicks     int64  `json:"move_ticks"`
	ReturnWaiver  bool   `json:"return_waiver"`
	DegradeMcpu   int64  `json:"degrade_mcpu"`
}

type V3TreeWave struct {
	WaveNo     int           `json:"wave_no"`
	WorkOrders []V3WorkOrder `json:"work_orders"`
}

type V3WorkOrder struct {
	WoRef            string   `json:"wo_ref"`
	ServerRefs       []string `json:"server_refs"`
	Priority         int      `json:"priority"`
	ListOrder        int      `json:"list_order"`
	MandatoryIfArmed bool     `json:"mandatory_if_armed"`
}

type V3Incident struct {
	Ref       string `json:"ref"`
	ServerRef string `json:"server_ref"`
	Incident  string `json:"incident"`
	AtTick    *int64 `json:"at_tick,omitempty"`
	AtOffset  *int64 `json:"at_offset,omitempty"`
}

type V3Loan struct {
	Ref          string `json:"ref"`
	CapacityLot  string `json:"capacity_lot"`
	LenderRef    string `json:"lender_ref"`
	BorrowerRef  string `json:"borrower_ref"`
	AmountMcpu   int64  `json:"amount_mcpu"`
	StartsAt     *int64 `json:"starts_at_tick,omitempty"`
	StartOffset  *int64 `json:"start_offset,omitempty"`
	ExpiresAt    *int64 `json:"expires_at_tick,omitempty"`
	ExpiryOffset *int64 `json:"expiry_offset,omitempty"`
	Priority     int    `json:"priority"`
	Order        int    `json:"order"`
	RecallPolicy string `json:"recall_policy"`
}

func loadV3(data []byte) (*domain.Project, error) {
	var doc V3Document
	if err := decodeVersion(data, &doc); err != nil {
		return nil, err
	}
	base := domain.Tick(doc.WindowSpec.StartsAtTick)
	p := &domain.Project{
		SchemaVersion: domain.SchemaV3,
		ProjectID:     doc.Header.ID,
		Seed:          doc.Header.Seed,
		TimeBase:      domain.CanonicalTimeBase,
		ForceArmed:    doc.ForceGate.Enabled,
		Window:        domain.Window{Start: base, End: base + domain.Tick(doc.WindowSpec.DurationTicks)},
		Policies: domain.Policies{
			MaxConcurrentMigrations: doc.Execution.ConcurrencySlots,
			MaxMigrationTicks:       domain.Tick(doc.Execution.MoveDeadline),
			ThreadBudget:            doc.Execution.WorkerThreads,
			PerNodeQueueLimit:       doc.Execution.QueueDepth,
			CheckpointVersion:       doc.Execution.StateRevision,
			RecallRule:              doc.Execution.RecallPolicy,
		},
	}
	for _, m := range doc.ProbeMoments {
		p.ProbeTicks = append(p.ProbeTicks, domain.Tick(m))
	}
	for _, s := range doc.Servers {
		state := s.Status
		if state == "" {
			state = domain.StateOnline
		}
		p.Nodes = append(p.Nodes, domain.Node{
			ID: s.ServerRef, CapacityMilli: s.CapacityMcpu,
			InitialUsedMilli: s.OccupiedMcpu, WatermarkBasisPoints: s.SafeFill.PerTenThousand,
			BorrowCapMilli: s.BorrowLimitMcpu, State: state,
		})
	}
	for _, pod := range doc.Pods {
		p.Instances = append(p.Instances, domain.Instance{
			ID: pod.PodRef, NodeID: pod.ServerRef, MilliCores: pod.Mcpu,
			Priority: pod.PriorityClass, DeclaredOrder: pod.TieBreakOrder,
			MigrationTicks: domain.Tick(pod.MoveTicks), ReturnExempt: pod.ReturnWaiver,
			DegradeMilliCores: pod.DegradeMcpu,
		})
	}
	for _, w := range doc.MaintenanceTree {
		wave := domain.Wave{Seq: w.WaveNo}
		for _, wo := range w.WorkOrders {
			p.Batches = append(p.Batches, domain.Batch{
				ID: wo.WoRef, Wave: w.WaveNo, NodeIDs: append([]string(nil), wo.ServerRefs...),
				Priority: wo.Priority, DeclaredOrder: wo.ListOrder,
				ForceDeclared: wo.MandatoryIfArmed, Forced: wo.MandatoryIfArmed && doc.ForceGate.Enabled,
			})
			wave.BatchIDs = append(wave.BatchIDs, wo.WoRef)
		}
		p.Waves = append(p.Waves, wave)
	}
	sortWaves(p.Waves)
	for i, inc := range doc.Incidents {
		tick, err := resolveTick(inc.AtTick, inc.AtOffset, base, "$.incidents["+itoaSchema(i)+"]")
		if err != nil {
			return nil, err
		}
		p.Events = append(p.Events, domain.MaintenanceEvent{ID: inc.Ref, NodeID: inc.ServerRef, Kind: inc.Incident, Tick: tick})
	}
	for i, l := range doc.Loans {
		path := "$.loans[" + itoaSchema(i) + "]"
		start, err := resolveTick(l.StartsAt, l.StartOffset, base, domain.JoinPath(path, "starts"))
		if err != nil {
			return nil, err
		}
		expiry, err := resolveTick(l.ExpiresAt, l.ExpiryOffset, base, domain.JoinPath(path, "expires"))
		if err != nil {
			return nil, err
		}
		p.Loans = append(p.Loans, domain.LoanSpec{
			ID: l.Ref, LotID: l.CapacityLot, LenderNodeID: l.LenderRef, BorrowerNodeID: l.BorrowerRef,
			AmountMilli: l.AmountMcpu, StartTick: start, ExpiryTick: expiry,
			Priority: l.Priority, DeclaredOrder: l.Order,
			RecallRule: firstNonEmpty(l.RecallPolicy, p.Policies.RecallRule),
		})
	}
	return p, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func EncodeV3(p domain.Project) ([]byte, error) {
	doc := V3Document{
		Schema:     domain.SchemaV3,
		Header:     V3Header{ID: p.ProjectID, Seed: p.Seed},
		Clock:      "global_tick_v3",
		WindowSpec: V3Window{StartsAtTick: int64(p.Window.Start), DurationTicks: int64(p.Window.End - p.Window.Start)},
		ForceGate:  V3ForceGate{Enabled: p.ForceArmed},
		Execution: V3Execution{
			ConcurrencySlots: p.Policies.MaxConcurrentMigrations,
			MoveDeadline:     int64(p.Policies.MaxMigrationTicks),
			WorkerThreads:    p.Policies.ThreadBudget, QueueDepth: p.Policies.PerNodeQueueLimit,
			StateRevision: p.Policies.CheckpointVersion, RecallPolicy: p.Policies.RecallRule,
		},
	}
	for _, t := range p.ProbeTicks {
		doc.ProbeMoments = append(doc.ProbeMoments, int64(t))
	}
	for _, n := range p.Nodes {
		doc.Servers = append(doc.Servers, V3Server{
			ServerRef: n.ID, CapacityMcpu: n.CapacityMilli,
			SafeFill:        V3Watermark{PerTenThousand: n.WatermarkBasisPoints},
			BorrowLimitMcpu: n.BorrowCapMilli, OccupiedMcpu: n.InitialUsedMilli, Status: n.State,
		})
	}
	for _, in := range p.Instances {
		doc.Pods = append(doc.Pods, V3Pod{
			PodRef: in.ID, ServerRef: in.NodeID, Mcpu: in.MilliCores,
			PriorityClass: in.Priority, TieBreakOrder: in.DeclaredOrder,
			MoveTicks: int64(in.MigrationTicks), ReturnWaiver: in.ReturnExempt, DegradeMcpu: in.DegradeMilliCores,
		})
	}
	for _, w := range p.Waves {
		tw := V3TreeWave{WaveNo: w.Seq}
		for _, bid := range w.BatchIDs {
			b, ok := p.Batch(bid)
			if !ok {
				continue
			}
			tw.WorkOrders = append(tw.WorkOrders, V3WorkOrder{
				WoRef: b.ID, ServerRefs: b.NodeIDs, Priority: b.Priority,
				ListOrder: b.DeclaredOrder, MandatoryIfArmed: b.ForceDeclared,
			})
		}
		doc.MaintenanceTree = append(doc.MaintenanceTree, tw)
	}
	for _, e := range p.Events {
		tick := int64(e.Tick)
		doc.Incidents = append(doc.Incidents, V3Incident{
			Ref: e.ID, ServerRef: e.NodeID, Incident: e.Kind, AtTick: &tick,
		})
	}
	for _, l := range p.Loans {
		st := int64(l.StartTick)
		ex := int64(l.ExpiryTick)
		doc.Loans = append(doc.Loans, V3Loan{
			Ref: l.ID, CapacityLot: l.LotID, LenderRef: l.LenderNodeID, BorrowerRef: l.BorrowerNodeID,
			AmountMcpu: l.AmountMilli, StartsAt: &st, ExpiresAt: &ex,
			Priority: l.Priority, Order: l.DeclaredOrder, RecallPolicy: l.RecallRule,
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

package gate

import (
	"sort"

	"maintenanceorchestrator/internal/domain"
	"maintenanceorchestrator/internal/lease"
	"maintenanceorchestrator/internal/solver"
)

const (
	StepMigrationComplete = "outgoing_complete"
	StepLeaseReturn       = "lease_return"
	StepReturnStart       = "return_start"
	StepReturnComplete    = "return_complete"

	ReasonOutstandingLeases    = "R_OUTSTANDING_LEASES"
	ReasonPendingReturns       = "R_PENDING_RETURNS"
	ReasonReturnCapacityBudget = "R_RETURN_CAPACITY_WAIT"
	ReasonReturnWatermark      = "R_RETURN_WATERMARK"
	ReasonEvacuationIncomplete = "R_EVACUATION_INCOMPLETE"
)

type Step struct {
	Seq        int         `json:"seq"`
	Tick       domain.Tick `json:"tick"`
	Kind       string      `json:"kind"`
	NodeID     string      `json:"node_id"`
	InstanceID string      `json:"instance_id,omitempty"`
	LeaseID    string      `json:"lease_id,omitempty"`
	Detail     string      `json:"detail,omitempty"`
}

type NodeGate struct {
	NodeID                     string      `json:"node_id"`
	Tick                       domain.Tick `json:"tick"`
	Schedulable                bool        `json:"schedulable"`
	OutstandingLeaseIDs        []string    `json:"outstanding_lease_ids"`
	PendingReturnInstanceIDs   []string    `json:"pending_return_instance_ids"`
	CompletedReturnInstanceIDs []string    `json:"completed_return_instance_ids"`
	ExemptInstanceIDs          []string    `json:"exempt_instance_ids"`
	NotEvacuatedInstanceIDs    []string    `json:"not_evacuated_instance_ids"`
	ReasonCodes                []string    `json:"reason_codes"`
}

type Snapshot struct {
	Tick  domain.Tick `json:"tick"`
	Gates []NodeGate  `json:"gates"`
}

type Result struct {
	Steps     []Step     `json:"steps"`
	Gates     []NodeGate `json:"gates"`
	Snapshots []Snapshot `json:"snapshots"`
}

type returnJob struct {
	inst      domain.Instance
	earliest  domain.Tick
	start     domain.Tick
	end       domain.Tick
	scheduled bool
	completed bool
	evacuated bool
}

func Plan(p domain.Project, plan solver.WavePlan, tl *lease.Timeline) Result {
	freed := FreedCapacity(p, plan)
	loanReturnTick := map[string]domain.Tick{}
	loansByLender := map[string][]domain.LoanSpec{}
	for _, l := range p.Loans {
		loansByLender[l.LenderNodeID] = append(loansByLender[l.LenderNodeID], l)
	}
	if tl != nil {
		for _, st := range tl.Leases {
			if st.ReturnedTick > 0 {
				loanReturnTick[st.ID] = st.ReturnedTick
			} else {
				loanReturnTick[st.ID] = st.ExpiryTick
			}
		}
	} else {
		for _, l := range p.Loans {
			loanReturnTick[l.ID] = l.ExpiryTick
		}
	}

	jobsByNode := map[string][]*returnJob{}
	migrated := map[string]solver.Migration{}
	for _, m := range plan.Migrations {
		migrated[m.InstanceID] = m
	}
	maintenance := domain.MaintenanceNodeSet(p)
	for nodeID := range maintenance {
		for _, in := range p.InstancesForNode(nodeID) {
			if in.ReturnExempt {
				continue
			}
			m, ok := migrated[in.ID]
			if !ok {
				jobsByNode[nodeID] = append(jobsByNode[nodeID], &returnJob{inst: in, evacuated: false})
				continue
			}
			jobsByNode[nodeID] = append(jobsByNode[nodeID], &returnJob{
				inst: in, earliest: m.EndTick, evacuated: true,
			})
		}
	}

	var result Result
	seq := 0
	nextSeq := func() int { seq++; return seq }

	endTick := p.Window.End
	for t := p.Window.Start; t <= endTick; t++ {
		for _, nodeID := range sortedNodeIDs(jobsByNode) {
			for _, job := range jobsByNode[nodeID] {
				if job.scheduled && !job.completed && job.end == t {
					job.completed = true
					result.Steps = append(result.Steps, Step{
						Seq: nextSeq(), Tick: t, Kind: StepReturnComplete,
						NodeID: nodeID, InstanceID: job.inst.ID,
					})
				}
			}
		}

		for _, l := range p.Loans {
			if loanReturnTick[l.ID] == t {
				result.Steps = append(result.Steps, Step{
					Seq: nextSeq(), Tick: t, Kind: StepLeaseReturn,
					NodeID: l.LenderNodeID, LeaseID: l.ID,
				})
			}
		}

		for _, nodeID := range sortedNodeIDs(jobsByNode) {
			jobs := jobsByNode[nodeID]
			sortJobs(jobs)
			outstanding := int64(0)
			for _, l := range loansByLender[nodeID] {
				if rt := loanReturnTick[l.ID]; rt > t {
					outstanding += l.AmountMilli
				}
			}
			reserved := int64(0)
			completed := int64(0)
			for _, job := range jobs {
				if job.completed {
					completed += job.inst.MilliCores
				} else if job.scheduled {
					reserved += job.inst.MilliCores
				}
			}
			node, _ := p.Node(nodeID)
			homeUsed := node.InitialUsedMilli
			for _, m := range plan.Migrations {
				if m.SourceNodeID == nodeID {
					homeUsed -= migrationSize(p, m.InstanceID)
				}
			}

			for _, job := range jobs {
				if !job.evacuated || job.scheduled || job.completed || job.earliest > t {
					continue
				}
				size := job.inst.MilliCores
				if outstanding+reserved+completed+size > freed[nodeID] {
					continue
				}
				if homeUsed+reserved+size > node.SafeLimitMilli() {
					continue
				}
				end := t + job.inst.MigrationTicks
				if node.Window.End > 0 && end > node.Window.End {
					continue
				}
				job.scheduled = true
				job.start = t
				job.end = end
				reserved += size
				result.Steps = append(result.Steps, Step{
					Seq: nextSeq(), Tick: t, Kind: StepReturnStart,
					NodeID: nodeID, InstanceID: job.inst.ID,
					Detail: "original node capacity slot is available",
				})
			}
		}
	}

	for _, tick := range p.ProbeTicks {
		result.Snapshots = append(result.Snapshots, Snapshot{Tick: tick, Gates: gatesAt(p, plan, jobsByNode, loansByLender, loanReturnTick, tick)})
	}
	result.Gates = gatesAt(p, plan, jobsByNode, loansByLender, loanReturnTick, p.Window.End)
	return result
}

func gatesAt(p domain.Project, plan solver.WavePlan, jobs map[string][]*returnJob, loans map[string][]domain.LoanSpec, loanReturn map[string]domain.Tick, tick domain.Tick) []NodeGate {
	var out []NodeGate
	for _, nodeID := range domain.SortedNodeIDs(p.Nodes) {
		maintenance := false
		for _, b := range p.Batches {
			for _, id := range b.NodeIDs {
				if id == nodeID {
					maintenance = true
				}
			}
		}
		if !maintenance {
			continue
		}
		g := NodeGate{NodeID: nodeID, Tick: tick, Schedulable: true, OutstandingLeaseIDs: []string{}, PendingReturnInstanceIDs: []string{}, CompletedReturnInstanceIDs: []string{}, ExemptInstanceIDs: []string{}, NotEvacuatedInstanceIDs: []string{}}
		for _, l := range loans[nodeID] {
			if rt := loanReturn[l.ID]; rt > tick {
				g.OutstandingLeaseIDs = append(g.OutstandingLeaseIDs, l.ID)
			}
		}
		sort.Strings(g.OutstandingLeaseIDs)
		for _, in := range p.InstancesForNode(nodeID) {
			if in.ReturnExempt {
				g.ExemptInstanceIDs = append(g.ExemptInstanceIDs, in.ID)
				continue
			}
			job := findJob(jobs[nodeID], in.ID)
			if job == nil || !job.evacuated {
				g.NotEvacuatedInstanceIDs = append(g.NotEvacuatedInstanceIDs, in.ID)
				continue
			}
			if job.completed && job.end <= tick {
				g.CompletedReturnInstanceIDs = append(g.CompletedReturnInstanceIDs, in.ID)
			} else if job.scheduled && job.end <= tick {
				g.CompletedReturnInstanceIDs = append(g.CompletedReturnInstanceIDs, in.ID)
				job.completed = true
			} else {
				g.PendingReturnInstanceIDs = append(g.PendingReturnInstanceIDs, in.ID)
			}
		}
		sort.Strings(g.CompletedReturnInstanceIDs)
		sort.Strings(g.ExemptInstanceIDs)
		sort.Strings(g.NotEvacuatedInstanceIDs)
		if len(g.OutstandingLeaseIDs) > 0 {
			g.Schedulable = false
			g.ReasonCodes = append(g.ReasonCodes, ReasonOutstandingLeases)
		}
		if len(g.PendingReturnInstanceIDs) > 0 {
			g.Schedulable = false
			g.ReasonCodes = append(g.ReasonCodes, ReasonPendingReturns)
		}
		if len(g.NotEvacuatedInstanceIDs) > 0 {
			g.Schedulable = false
			g.ReasonCodes = append(g.ReasonCodes, ReasonEvacuationIncomplete)
		}
		out = append(out, g)
	}
	return out
}

func FreedCapacity(p domain.Project, plan solver.WavePlan) map[string]int64 {
	out := map[string]int64{}
	for _, m := range plan.Migrations {
		out[m.SourceNodeID] += migrationSize(p, m.InstanceID)
	}
	return out
}

func migrationSize(p domain.Project, id string) int64 {
	for _, in := range p.Instances {
		if in.ID == id {
			return in.MilliCores
		}
	}
	return 0
}

func findJob(jobs []*returnJob, id string) *returnJob {
	for _, j := range jobs {
		if j.inst.ID == id {
			return j
		}
	}
	return nil
}

func sortedNodeIDs(in map[string][]*returnJob) []string {
	out := make([]string, 0, len(in))
	for id := range in {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sortJobs(jobs []*returnJob) {
	sort.SliceStable(jobs, func(i, j int) bool {
		a, b := jobs[i].inst, jobs[j].inst
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.DeclaredOrder != b.DeclaredOrder {
			return a.DeclaredOrder < b.DeclaredOrder
		}
		return a.ID < b.ID
	})
}

package solver

import (
	"fmt"
	"sort"

	"maintenanceorchestrator/internal/domain"
)

const (
	ReasonTargetCapacity  = "R_TARGET_CAPACITY_INSUFFICIENT"
	ReasonTargetWatermark = "R_TARGET_WATERMARK_FULL"
	ReasonNoOnlineTarget  = "R_NO_ONLINE_TARGET"
	ReasonWaveConcurrency = "R_WAVE_CONCURRENCY_FULL"
	ReasonMoveDuration    = "R_MIGRATION_DURATION_EXCEEDS_LIMIT"
)

type TargetFit struct {
	NodeID                 string `json:"node_id"`
	HardFreeMilli          int64  `json:"hard_free_milli"`
	WatermarkHeadroomMilli int64  `json:"watermark_headroom_milli"`
	Fits                   bool   `json:"fits"`
	ReasonCode             string `json:"reason_code,omitempty"`
	ConstraintID           string `json:"constraint_id,omitempty"`
	Message                string `json:"message,omitempty"`
}

type InstanceFeasibility struct {
	InstanceID         string      `json:"instance_id"`
	NodeID             string      `json:"node_id"`
	RequestMilli       int64       `json:"request_milli"`
	Movable            bool        `json:"movable"`
	SelectedTargetID   string      `json:"selected_target_id,omitempty"`
	ReasonCode         string      `json:"reason_code,omitempty"`
	ConstraintID       string      `json:"constraint_id,omitempty"`
	Message            string      `json:"message,omitempty"`
	CandidateTargetIDs []string    `json:"candidate_target_ids"`
	TargetFits         []TargetFit `json:"target_fits"`
}

type NodeFeasibility struct {
	NodeID             string                `json:"node_id"`
	Tick               domain.Tick           `json:"tick"`
	Pending            bool                  `json:"pending"`
	TotalInstances     int                   `json:"total_instances"`
	EvacuableCount     int                   `json:"evacuable_count"`
	AcceptingTargetIDs []string              `json:"accepting_target_ids"`
	Decisions          []InstanceFeasibility `json:"decisions"`
}

type TickReport struct {
	Tick         domain.Tick       `json:"tick"`
	PendingNodes []string          `json:"pending_nodes"`
	Nodes        []NodeFeasibility `json:"nodes"`
}

type FeasibilityOptions struct {
	Tick        domain.Tick
	Project     domain.Project
	Used        map[string]int64
	Reserved    map[string]int64
	Pending     map[string]bool
	CurrentNode map[string]string
}

func EvaluateTick(opts FeasibilityOptions) TickReport {
	p := opts.Project
	used := opts.Used
	if used == nil {
		used = UsageFromPlacements(p, nil)
	}
	reserved := opts.Reserved
	if reserved == nil {
		reserved = map[string]int64{}
	}
	pending := opts.Pending
	if pending == nil {
		pending = domain.MaintenanceNodeSet(p)
	}

	report := TickReport{Tick: opts.Tick}
	for id := range pending {
		if pending[id] {
			report.PendingNodes = append(report.PendingNodes, id)
		}
	}
	sort.Strings(report.PendingNodes)

	for _, nodeID := range report.PendingNodes {
		nf := evaluateNode(p, nodeID, opts.Tick, used, reserved, pending, opts.CurrentNode)
		report.Nodes = append(report.Nodes, nf)
	}
	return report
}

func EvaluateNode(p domain.Project, nodeID string, tick domain.Tick, used, reserved map[string]int64, pending map[string]bool, currentNode map[string]string) NodeFeasibility {
	return evaluateNode(p, nodeID, tick, used, reserved, pending, currentNode)
}

func UsageFromPlacements(p domain.Project, placement map[string]string) map[string]int64 {
	out := map[string]int64{}
	for _, n := range p.Nodes {
		out[n.ID] = 0
	}
	for _, in := range p.Instances {
		nodeID := in.NodeID
		if placement != nil {
			if movedTo, ok := placement[in.ID]; ok {
				nodeID = movedTo
			}
		}
		out[nodeID] += in.MilliCores
	}
	return out
}

func evaluateNode(p domain.Project, nodeID string, tick domain.Tick, used, reserved map[string]int64, pending map[string]bool, currentNode map[string]string) NodeFeasibility {
	nf := NodeFeasibility{NodeID: nodeID, Tick: tick, Pending: true}
	var instances []domain.Instance
	for _, in := range p.InstancesForNode(nodeID) {
		if currentNode != nil {
			if current, ok := currentNode[in.ID]; ok && current != nodeID {
				continue
			}
		}
		instances = append(instances, in)
	}
	nf.TotalInstances = len(instances)
	consumed := map[string]int64{}
	accepting := map[string]bool{}

	for _, in := range instances {
		decision := InstanceFeasibility{InstanceID: in.ID, NodeID: nodeID, RequestMilli: in.MilliCores, CandidateTargetIDs: []string{}}
		fits := targetFits(p, in.MilliCores, used, reserved, consumed, pending)
		decision.TargetFits = fits
		var candidates []TargetFit
		for _, f := range fits {
			if f.Fits {
				candidates = append(candidates, f)
				decision.CandidateTargetIDs = append(decision.CandidateTargetIDs, f.NodeID)
				accepting[f.NodeID] = true
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			hi := candidates[i].WatermarkHeadroomMilli
			hj := candidates[j].WatermarkHeadroomMilli
			if hi != hj {
				return hi < hj
			}
			return candidates[i].NodeID < candidates[j].NodeID
		})
		if len(candidates) > 0 {
			best := candidates[0]
			decision.Movable = true
			decision.SelectedTargetID = best.NodeID
			consumed[best.NodeID] += in.MilliCores
			nf.EvacuableCount++
		} else {
			applyNoFitReason(&decision, fits)
		}
		nf.Decisions = append(nf.Decisions, decision)
	}
	nf.AcceptingTargetIDs = sortedKeys(accepting)
	return nf
}

func targetFits(p domain.Project, request int64, used, reserved, consumed map[string]int64, pending map[string]bool) []TargetFit {
	var out []TargetFit
	for _, n := range p.Nodes {
		if pending[n.ID] || n.State == domain.StateMaintenance {
			continue
		}
		effectiveUsed := used[n.ID] + reserved[n.ID] + consumed[n.ID]
		hardFree := n.CapacityMilli - effectiveUsed
		wmHeadroom := n.SafeLimitMilli() - effectiveUsed
		fit := TargetFit{NodeID: n.ID, HardFreeMilli: hardFree, WatermarkHeadroomMilli: wmHeadroom}
		switch {
		case hardFree < request:
			fit.ReasonCode = ReasonTargetCapacity
			fit.ConstraintID = domain.EConstraintCapacity
		case wmHeadroom < request:
			fit.ReasonCode = ReasonTargetWatermark
			fit.ConstraintID = domain.EConstraintWatermark
		default:
			fit.Fits = true
		}
		out = append(out, fit)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

func applyNoFitReason(decision *InstanceFeasibility, fits []TargetFit) {
	if len(fits) == 0 {
		decision.ReasonCode = ReasonNoOnlineTarget
		decision.ConstraintID = domain.EConstraintCapacity
		decision.Message = "no remaining online target node"
		return
	}
	watermarkBlocked := false
	capacityBlocked := false
	for _, f := range fits {
		if f.ReasonCode == ReasonTargetWatermark {
			watermarkBlocked = true
		}
		if f.ReasonCode == ReasonTargetCapacity {
			capacityBlocked = true
		}
	}
	if watermarkBlocked {
		decision.ReasonCode = ReasonTargetWatermark
		decision.ConstraintID = domain.EConstraintWatermark
		decision.Message = fmt.Sprintf("request %d does not fit nondisruption watermark on any hard-capable target", decision.RequestMilli)
		return
	}
	if capacityBlocked {
		decision.ReasonCode = ReasonTargetCapacity
		decision.ConstraintID = domain.EConstraintCapacity
		decision.Message = fmt.Sprintf("request %d exceeds remaining hard capacity on all targets", decision.RequestMilli)
	}
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

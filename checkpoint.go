package orchestrator

import "sort"

type CheckpointNode struct {
	NodeState
	PendingRecoveryTick *int64 `json:"pending_recovery_tick,omitempty"`
	Recovered           bool   `json:"recovered"`
}

type CheckpointDomain struct {
	DomainView
}

type CheckpointWorkload struct {
	WorkloadID   string   `json:"workload_id"`
	NodeID       string   `json:"node_id"`
	BudgetBytes  int64    `json:"budget_bytes"`
	UsedBytes    int64    `json:"used_bytes"`
	Frozen       bool     `json:"frozen"`
	FreezeSource []string `json:"freeze_sources,omitempty"`
}

type Checkpoint struct {
	Tick       int64                `json:"tick"`
	SnapshotID string               `json:"snapshot_id"`
	Revision   string               `json:"revision"`
	Nodes      []CheckpointNode     `json:"nodes"`
	Domains    []CheckpointDomain   `json:"domains"`
	Workloads  []CheckpointWorkload `json:"workloads"`
	Waiting    []WaitEntry          `json:"waiting_queue"`
}

func (s *simulation) checkpoint(tick int64, result TickResult) Checkpoint {
	checkpoint := Checkpoint{
		Tick:       tick,
		SnapshotID: s.project.SnapshotID,
		Revision:   s.project.Revision,
		Waiting:    s.waitingSnapshot(),
	}
	stateByNode := make(map[string]NodeState, len(result.NodeStates))
	for _, state := range result.NodeStates {
		stateByNode[state.NodeID] = state
	}
	for _, node := range s.project.Nodes {
		state := stateByNode[node.ID]
		checkpointNode := CheckpointNode{NodeState: state}
		if requested, ok := s.state.Pending[node.ID]; ok {
			requestedCopy := requested
			checkpointNode.PendingRecoveryTick = &requestedCopy
		}
		_, checkpointNode.Recovered = s.state.Recovered[node.ID]
		checkpoint.Nodes = append(checkpoint.Nodes, checkpointNode)
	}
	domainIDs := make([]int64, 0, len(s.state.Domains))
	for id := range s.state.Domains {
		domainIDs = append(domainIDs, id)
	}
	sort.Slice(domainIDs, func(i, j int) bool { return domainIDs[i] < domainIDs[j] })
	for _, id := range domainIDs {
		domain := s.state.Domains[id]
		checkpoint.Domains = append(checkpoint.Domains, CheckpointDomain{DomainView: s.domainView(domain, "active", nil)})
	}
	for _, node := range s.project.Nodes {
		for _, workload := range node.Workloads {
			item := CheckpointWorkload{
				WorkloadID:  workload.ID,
				NodeID:      node.ID,
				BudgetBytes: workload.BudgetBytes,
				UsedBytes:   s.state.Used[workload.ID],
			}
			if _, frozen := s.state.Frozen[workload.ID]; frozen {
				item.Frozen = true
				item.FreezeSource = sortedSet(s.state.FrozenSources[workload.ID])
			}
			checkpoint.Workloads = append(checkpoint.Workloads, item)
		}
	}
	return checkpoint
}

func stateFromCheckpoint(checkpoint Checkpoint, project *Project) (*simState, error) {
	state := newInitialState(project)
	state.LastTick = checkpoint.Tick
	seenWorkloads := make(map[string]struct{}, len(checkpoint.Workloads))
	for _, workload := range checkpoint.Workloads {
		if _, ok := project.Workload(workload.WorkloadID); !ok {
			return nil, newError(CodeUnknownWorkload, "$.checkpoint.workloads", "unknown workload %q", workload.WorkloadID)
		}
		state.Used[workload.WorkloadID] = workload.UsedBytes
		seenWorkloads[workload.WorkloadID] = struct{}{}
		if workload.Frozen {
			state.Frozen[workload.WorkloadID] = struct{}{}
			state.FrozenSources[workload.WorkloadID] = stringSet(workload.FreezeSource)
		}
	}
	for _, node := range project.Nodes {
		for _, workload := range node.Workloads {
			if _, ok := seenWorkloads[workload.ID]; !ok {
				return nil, newError(CodeCheckpoint, "$.checkpoint.workloads", "missing workload %q", workload.ID)
			}
		}
	}
	for _, domain := range checkpoint.Domains {
		roots := make(map[string]struct{}, len(domain.RootNodes))
		for _, root := range domain.RootNodes {
			if _, ok := project.Node(root); !ok {
				return nil, newError(CodeUnknownNode, "$.checkpoint.domains", "unknown root node %q", root)
			}
			roots[root] = struct{}{}
		}
		domainState := &domainState{
			ID:              domain.ID,
			Roots:           roots,
			StartedTick:     domain.StartedTick,
			Timeout:         Timeout{Set: true, Ticks: domain.TimeoutTicks},
			DeadlineTick:    domain.DeadlineTick,
			Forced:          domain.Forced,
			UpgradedTick:    copyInt64(domain.UpgradedTick),
			UpgradeEvidence: domain.UpgradeEvidence,
			CommittedTick:   nil,
		}
		state.Domains[domain.ID] = domainState
	}
	for _, node := range checkpoint.Nodes {
		if _, ok := project.Node(node.NodeID); !ok {
			return nil, newError(CodeUnknownNode, "$.checkpoint.nodes", "unknown node %q", node.NodeID)
		}
		if node.Faulty {
			state.Faulty[node.NodeID] = struct{}{}
		}
		if node.Recovered {
			state.Recovered[node.NodeID] = struct{}{}
		}
		if node.PendingRecoveryTick != nil {
			state.Pending[node.NodeID] = *node.PendingRecoveryTick
		}
	}
	state.Waiting = make([]waitRequest, 0, len(checkpoint.Waiting))
	for _, wait := range checkpoint.Waiting {
		if _, ok := project.Workload(wait.WorkloadID); !ok {
			return nil, newError(CodeUnknownWorkload, "$.checkpoint.waiting_queue", "unknown workload %q", wait.WorkloadID)
		}
		state.Waiting = append(state.Waiting, waitRequest{
			ID:          wait.ID,
			WorkloadID:  wait.WorkloadID,
			NodeID:      wait.NodeID,
			AmountBytes: wait.AmountBytes,
			ArrivedTick: wait.ArrivedTick,
			Seq:         wait.Seq,
			Sources:     append([]string(nil), wait.Sources...),
		})
	}
	return state, nil
}

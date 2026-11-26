package loader

import (
	"schedlab/internal/domain"
	"schedlab/internal/faults"
	"schedlab/internal/gen"
	"sort"
)

func fromV2(dto *gen.V2Project, opts Options) (*domain.Project, error) {
	refCPU, err := parseCores(dto.ReferenceCPU.CPU, "reference-cpu")
	if err != nil {
		return nil, err
	}
	refMem, err := parseMemory(dto.ReferenceCPU.Mem, "reference-mem")
	if err != nil {
		return nil, err
	}
	if err := checkNegative("reference", refCPU, refMem, dto.ProjectID); err != nil {
		return nil, err
	}
	seenSem := map[string]string{}
	for _, ts := range dto.TopologySemantics {
		seenSem[ts.TopologyKey] = ts.Semantic
	}
	if err := validateSemantics(seenSem, opts.TopologySemantics); err != nil {
		return nil, err
	}

	p := &domain.Project{
		ID:             dto.ProjectID,
		SchemaVersions: []string{V2},
		ReferenceCPU:   domain.Resources{MilliCPU: refCPU, Memory: refMem},
		TopologyKeys:   semList(seenSem),
	}
	nodeIDs := map[string]bool{}
	// Flatten zone -> rack -> node in declared tree order.
	for _, zone := range dto.ZoneTree {
		for _, rack := range zone.Racks {
			for _, n := range rack.Nodes {
				// Defer order assignment until after canonical re-sort below.
				if nodeIDs[n.NodeID] {
					return nil, faults.New(faults.E_DUPLICATE_ID, "duplicate node", "node", n.NodeID)
				}
				nodeIDs[n.NodeID] = true
				cpu, err := parseCores(n.Allocatable.CPU, n.NodeID+".cpu")
				if err != nil {
					return nil, err
				}
				mem, err := parseMemory(n.Allocatable.Mem, n.NodeID+".mem")
				if err != nil {
					return nil, err
				}
				if err := checkNegative("allocatable", cpu, mem, n.NodeID); err != nil {
					return nil, err
				}
				dn := &domain.Node{
					ID:          n.NodeID,
					Allocatable: domain.Resources{MilliCPU: cpu, Memory: mem},
					Labels:      cloneLabels(n.Labels),
					Topology:    cloneLabels(n.Topology),
				}
				for _, t := range n.Taints {
					dn.Taints = append(dn.Taints, domain.Taint{Key: t.Key, Value: t.Value, Effect: domain.TaintEffect(t.Effect)})
				}
				p.Nodes = append(p.Nodes, dn)
			}
		}
	}
	// Re-impose canonical traversal order (node id order) so the nested schema
	// yields the exact same ordering as the flat v1/v3 manifests.
	sort.SliceStable(p.Nodes, func(i, j int) bool { return p.Nodes[i].ID < p.Nodes[j].ID })
	for i, dn := range p.Nodes {
		dn.Order = i
	}

	podIDs := map[string]bool{}
	for i, pd := range dto.Pods {
		if podIDs[pd.PodID] {
			return nil, faults.New(faults.E_DUPLICATE_ID, "duplicate pod", "pod", pd.PodID)
		}
		podIDs[pd.PodID] = true
		cpu, err := parseCores(pd.Request.CPU, pd.PodID+".cpu")
		if err != nil {
			return nil, err
		}
		mem, err := parseMemory(pd.Request.Mem, pd.PodID+".mem")
		if err != nil {
			return nil, err
		}
		if err := checkNegative("request", cpu, mem, pd.PodID); err != nil {
			return nil, err
		}
		dp := &domain.Pod{
			Order: i, ID: pd.PodID,
			Request:            domain.Resources{MilliCPU: cpu, Memory: mem},
			NodeSelector:       domain.LabelSelector{MatchLabels: cloneLabels(pd.NodeSelector)},
			SoftTimeoutMs:      0,
			SoftTimeoutPresent: false,
		}
		for _, t := range pd.Tolerations {
			dp.Tolerations = append(dp.Tolerations, domain.Toleration{
				Key: t.Key, Operator: t.Operator, Value: t.Value, Effect: domain.TaintEffect(t.Effect),
			})
		}
		for _, r := range pd.MustRules {
			dr, err := v2Rule(r, domain.Must)
			if err != nil {
				return nil, err
			}
			dp.MustRules = append(dp.MustRules, dr)
		}
		for _, r := range pd.PreferRules {
			dr, err := v2Rule(r, domain.Prefer)
			if err != nil {
				return nil, err
			}
			dp.PreferRules = append(dp.PreferRules, dr)
		}
		p.Pods = append(p.Pods, dp)
	}
	return p, nil
}

func v2Rule(r gen.V2Rule, phase domain.RulePhase) (domain.AffinityRule, error) {
	if phase == domain.Prefer {
		if err := checkWeight(string(phase), r.WeightBps, r.RuleID); err != nil {
			return domain.AffinityRule{}, err
		}
	}
	return domain.AffinityRule{
		ID:           r.RuleID,
		Phase:        phase,
		Mode:         domain.AffinityMode(r.Mode),
		Selector:     domain.LabelSelector{MatchLabels: cloneLabels(r.MatchLabels)},
		TopologyKey:  r.TopologyKey,
		WeightBps:    r.WeightBps,
		MaxCoLocated: r.MaxColocated,
	}, nil
}

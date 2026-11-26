package loader

import (
	"schedlab/internal/domain"
	"schedlab/internal/faults"
	"schedlab/internal/gen"
	"schedlab/internal/scale"
)

func fromV1(dto *gen.V1Project, opts Options) (*domain.Project, error) {
	refCPU := dto.ReferenceCPU.CPUCores * 1000
	refMem := dto.ReferenceCPU.MemBytes
	if err := checkNegative("reference", refCPU, refMem, dto.ProjectID); err != nil {
		return nil, err
	}
	seenSem := map[string]string{}
	for _, ts := range dto.TopologySemantics {
		seenSem[ts.Key] = ts.Semantic
	}
	if err := validateSemantics(seenSem, opts.TopologySemantics); err != nil {
		return nil, err
	}

	p := &domain.Project{
		ID:             dto.ProjectID,
		SchemaVersions: []string{V1},
		ReferenceCPU:   domain.Resources{MilliCPU: refCPU, Memory: refMem},
		TopologyKeys:   semList(seenSem),
	}
	nodeIDs := map[string]bool{}
	for i, n := range dto.Nodes {
		if nodeIDs[n.NodeID] {
			return nil, faults.New(faults.E_DUPLICATE_ID, "duplicate node", "node", n.NodeID)
		}
		nodeIDs[n.NodeID] = true
		cpu := n.Allocatable.CPUCores * 1000
		mem := n.Allocatable.MemBytes
		if err := checkNegative("allocatable", cpu, mem, n.NodeID); err != nil {
			return nil, err
		}
		dn := &domain.Node{
			Order: i, ID: n.NodeID,
			Allocatable: domain.Resources{MilliCPU: cpu, Memory: mem},
			Labels:      cloneLabels(n.Labels),
			Topology:    cloneLabels(n.Topology),
		}
		for _, t := range n.Taints {
			dn.Taints = append(dn.Taints, domain.Taint{Key: t.Key, Value: t.Value, Effect: domain.TaintEffect(t.Effect)})
		}
		p.Nodes = append(p.Nodes, dn)
	}

	podIDs := map[string]bool{}
	for i, pd := range dto.Pods {
		if podIDs[pd.PodID] {
			return nil, faults.New(faults.E_DUPLICATE_ID, "duplicate pod", "pod", pd.PodID)
		}
		podIDs[pd.PodID] = true
		cpu := pd.Request.CPUCores * 1000
		mem := pd.Request.MemBytes
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
			dr, err := v1Rule(r, domain.Must)
			if err != nil {
				return nil, err
			}
			dp.MustRules = append(dp.MustRules, dr)
		}
		for _, r := range pd.PreferRules {
			dr, err := v1Rule(r, domain.Prefer)
			if err != nil {
				return nil, err
			}
			dp.PreferRules = append(dp.PreferRules, dr)
		}
		p.Pods = append(p.Pods, dp)
	}
	return p, nil
}

func v1Rule(r gen.V1Rule, phase domain.RulePhase) (domain.AffinityRule, error) {
	bps, _, err := scale.PermilleToBPS(r.WeightPermille)
	if err != nil {
		return domain.AffinityRule{}, faults.Wrap(faults.E_OVERFLOW, "weight scale overflow", err, "rule", r.RuleID)
	}
	if phase == domain.Prefer {
		if err := checkWeight(string(phase), bps, r.RuleID); err != nil {
			return domain.AffinityRule{}, err
		}
	}
	return domain.AffinityRule{
		ID:           r.RuleID,
		Phase:        phase,
		Mode:         domain.AffinityMode(r.Mode),
		Selector:     domain.LabelSelector{MatchLabels: cloneLabels(r.MatchLabels)},
		TopologyKey:  r.TopologyKey,
		WeightBps:    bps,
		MaxCoLocated: r.MaxColocated,
	}, nil
}

package loader

import (
	"strings"

	"schedlab/internal/domain"
	"schedlab/internal/faults"
	"schedlab/internal/gen"
	"schedlab/internal/scale"
)

func fromV3(dto *gen.V3Project, opts Options) (*domain.Project, error) {
	refCPU, err := parseCPUMilli(dto.ReferenceCPU.CPU, 0, "reference-cpu")
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
		SchemaVersions: []string{V3},
		ReferenceCPU:   domain.Resources{MilliCPU: refCPU, Memory: refMem},
		TopologyKeys:   semList(seenSem),
	}
	nodeIDs := map[string]bool{}
	for i, n := range dto.Nodes {
		if nodeIDs[n.NodeID] {
			return nil, faults.New(faults.E_DUPLICATE_ID, "duplicate node", "node", n.NodeID)
		}
		nodeIDs[n.NodeID] = true
		cpu, err := parseCPUMilli(n.Allocatable.CPU, refCPU, n.NodeID+".cpu")
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
		cpu, err := parseCPUMilli(pd.Request.CPU, refCPU, pd.PodID+".cpu")
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
			Request:      domain.Resources{MilliCPU: cpu, Memory: mem},
			NodeSelector: domain.LabelSelector{MatchLabels: cloneLabels(pd.NodeSelector)},
			// v3 never carries a soft timeout.
			SoftTimeoutPresent: false,
		}
		for _, t := range pd.Tolerations {
			dp.Tolerations = append(dp.Tolerations, domain.Toleration{
				Key: t.Key, Operator: t.Operator, Value: t.Value, Effect: domain.TaintEffect(t.Effect),
			})
		}
		for _, r := range pd.MustRules {
			dr, err := v3Rule(r, domain.Must)
			if err != nil {
				return nil, err
			}
			dp.MustRules = append(dp.MustRules, dr)
		}
		for _, r := range pd.PreferRules {
			dr, err := v3Rule(r, domain.Prefer)
			if err != nil {
				return nil, err
			}
			dp.PreferRules = append(dp.PreferRules, dr)
		}
		p.Pods = append(p.Pods, dp)
	}
	return p, nil
}

func v3Rule(r gen.V3Rule, phase domain.RulePhase) (domain.AffinityRule, error) {
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

// parseCPUMilli accepts "1500m" milli tokens, plain integer/decimal cores and
// ratio strings ("37.5%"). Ratios resolve against refMilliCPU.
func parseCPUMilli(token string, refMilliCPU int64, ctx string) (int64, error) {
	switch {
	case strings.HasSuffix(token, "m"):
		v, _, err := scale.MilliToken(token)
		if err != nil {
			return 0, faults.Wrap(faults.E_RESOURCE_PARSE, "bad milli token", err, "field", ctx, "value", token)
		}
		return v, nil
	case strings.Contains(token, "%"):
		v, _, err := scale.RatioToMilli(token, refMilliCPU)
		if err != nil {
			return mapScaleErr(err, ctx, token)
		}
		return v, nil
	default:
		v, _, err := scale.CoresToMilli(token)
		if err != nil {
			return mapScaleErr(err, ctx, token)
		}
		return v, nil
	}
}

func mapScaleErr(err error, ctx, token string) (int64, error) {
	if err == scale.ErrOverflow {
		return 0, faults.Wrap(faults.E_OVERFLOW, "resource conversion overflow", err, "field", ctx, "value", token)
	}
	return 0, faults.Wrap(faults.E_RESOURCE_PARSE, "bad resource token", err, "field", ctx, "value", token)
}

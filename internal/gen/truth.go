package gen

import (
	"schedlab/internal/domain"
)

// ToDomain renders the canonical spec directly into the internal model. This
// is the generator-side ground truth that loader output is compared against.
func (s *canonSpec) ToDomain() *domain.Project {
	p := &domain.Project{
		ID:             s.projectID,
		SchemaVersions: []string{"v1", "v2", "v3"},
		ReferenceCPU: domain.Resources{
			MilliCPU: s.refCpuMilli,
			Memory:   s.refMemGiB * giB,
		},
		TopologyKeys: cloneSem(s.topoSem),
	}
	for i, n := range s.nodes {
		dn := &domain.Node{
			Order: i,
			ID:    n.id,
			Allocatable: domain.Resources{
				MilliCPU: n.cpuCores * 1000,
				Memory:   n.memGiB * giB,
			},
			Labels:   cloneMap(n.labels),
			Topology: cloneMap(n.topology),
		}
		for _, t := range n.taints {
			dn.Taints = append(dn.Taints, domain.Taint{
				Key: t.key, Value: t.value, Effect: domain.TaintEffect(t.effect),
			})
		}
		p.Nodes = append(p.Nodes, dn)
	}
	for i, pod := range s.pods {
		dp := &domain.Pod{
			Order: i,
			ID:    pod.id,
			Request: domain.Resources{
				MilliCPU: pod.cpuMilli,
				Memory:   pod.memGiB * giB,
			},
			NodeSelector: domain.LabelSelector{MatchLabels: cloneMap(pod.nodeSelector)},
			// The soft timeout is optional and omitted outright by schema v3;
			// the normalised model does not carry it, keeping all three
			// versions structurally identical.
			SoftTimeoutMs:      0,
			SoftTimeoutPresent: false,
		}
		for _, t := range pod.tolerations {
			dp.Tolerations = append(dp.Tolerations, domain.Toleration{
				Key: t.key, Operator: t.operator, Value: t.value,
				Effect: domain.TaintEffect(t.effect),
			})
		}
		for _, r := range pod.mustRules {
			dp.MustRules = append(dp.MustRules, toDomainRule(r))
		}
		for _, r := range pod.preferRules {
			dp.PreferRules = append(dp.PreferRules, toDomainRule(r))
		}
		p.Pods = append(p.Pods, dp)
	}
	return p
}

func toDomainRule(r canonRule) domain.AffinityRule {
	return domain.AffinityRule{
		ID:           r.id,
		Phase:        domain.RulePhase(r.phase),
		Mode:         domain.AffinityMode(r.mode),
		Selector:     domain.LabelSelector{MatchLabels: cloneMap(r.matchLabels)},
		TopologyKey:  r.topologyKey,
		WeightBps:    r.weightBps,
		MaxCoLocated: r.maxColocated,
	}
}

func cloneSem(m map[string]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		out[k] = []string{v}
	}
	return out
}

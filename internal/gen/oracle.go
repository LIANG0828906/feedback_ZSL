package gen

import "schedlab/internal/domain"

// OracleNodeScore is the generator-side ground-truth score for one node.
type OracleNodeScore struct {
	NodeID        string
	ComponentRaw  []int64 // per preferred rule, declaration order
	RawTotal      int64
	NormalizedBps int64
}

// OracleScore re-derives preferred-rule scores independently from the domain
// model. It uses the same integer semantics as the engine but is written as a
// separate, deliberately simple implementation so it can act as ground truth.
func OracleScore(p *domain.Project, pod *domain.Pod, feasible []string) []OracleNodeScore {
	byID := p.NodeByID()
	type comp struct {
		raw int64
	}
	all := make([]OracleNodeScore, 0, len(feasible))
	var maxRaw int64
	for _, nodeID := range feasible {
		cand := byID[nodeID]
		ons := OracleNodeScore{NodeID: nodeID}
		var raw int64
		for _, rule := range pod.PreferRules {
			domVal := cand.Topology[rule.TopologyKey]
			var matched int64
			if domVal != "" {
				for _, n := range p.Nodes {
					if n.ID == cand.ID || n.Topology[rule.TopologyKey] != domVal {
						continue
					}
					if rule.Selector.Matches(n.Labels) {
						matched++
					}
				}
			}
			penalty := int64(0)
			if rule.MaxCoLocated > 0 && matched > rule.MaxCoLocated {
				penalty = matched - rule.MaxCoLocated
			}
			var c int64
			switch rule.Mode {
			case domain.Affinity:
				c = sat(rule.WeightBps * (matched - penalty))
			case domain.AntiAffinity:
				eff := rule.MaxCoLocated - matched
				if eff < 0 {
					eff = 0
				}
				c = sat(rule.WeightBps * eff)
			}
			ons.ComponentRaw = append(ons.ComponentRaw, c)
			raw = sat(raw + c)
		}
		ons.RawTotal = raw
		if raw > maxRaw {
			maxRaw = raw
		}
		all = append(all, ons)
	}
	for i := range all {
		if maxRaw <= 0 {
			all[i].NormalizedBps = 0
			continue
		}
		all[i].NormalizedBps = halfScale(all[i].RawTotal, 10000, maxRaw)
	}
	return all
}

// sat clamps a product/sum to the int64 range; generator inputs never reach
// the boundary but the guard keeps the oracle total.
func sat(v int64) int64 {
	const max = int64(^uint64(0) >> 1)
	const min = -max - 1
	if v > max {
		return max
	}
	if v < min {
		return min
	}
	return v
}

func halfScale(v, num, den int64) int64 {
	// Non-negative inputs only; emulate half-away-from-zero rounding.
	q := v * num
	r := q % den
	res := q / den
	if r != 0 && r >= den-r {
		res++
	}
	return res
}

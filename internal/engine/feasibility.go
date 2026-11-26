// Package engine implements feasibility filtering, hard-rule elimination,
// preferred-rule scoring and the two scoring paths over the domain model.
package engine

import (
	"fmt"

	"schedlab/internal/domain"
)

// ConstraintRef is a stable locator for a failed hard constraint.
type ConstraintRef struct {
	// Code is the constraint family: C1 resource, C2 node-selector,
	// C3 taint, C4 must-affinity, C5 must-anti-affinity.
	Code string `json:"code"`
	// Index is the zero-based declaration index within that family.
	Index int `json:"index"`
	// Detail is a deterministic human-readable context.
	Detail string `json:"detail"`
}

func (c ConstraintRef) String() string { return fmt.Sprintf("%s[%d](%s)", c.Code, c.Index, c.Detail) }

// NodeFailure records every constraint that eliminated one node, in
// declaration order rather than first-fail order.
type NodeFailure struct {
	NodeID      string          `json:"node_id"`
	Constraints []ConstraintRef `json:"constraints"`
}

// FeasibilityResult is the full filter output for one pod.
type FeasibilityResult struct {
	PodID        string        `json:"pod_id"`
	Feasible     []string      `json:"feasible_nodes"`
	Eliminations []NodeFailure `json:"eliminations"`
}

// Filter evaluates the hard constraint chain for one pod against all nodes.
// All simultaneously-failing constraints are collected in declaration order:
// C1 resource capacity, C2 node selector, C3 taints, C4 must affinity,
// C5 must anti-affinity.
func Filter(p *domain.Project, pod *domain.Pod) FeasibilityResult {
	res := FeasibilityResult{PodID: pod.ID}
	for _, node := range p.Nodes {
		var failed []ConstraintRef

		// C1: resource capacity on the unified milli-core / byte scale.
		if pod.Request.MilliCPU > node.Allocatable.MilliCPU {
			failed = append(failed, ConstraintRef{
				Code: "C1", Index: 0,
				Detail: fmt.Sprintf("cpu %dm > %dm", pod.Request.MilliCPU, node.Allocatable.MilliCPU),
			})
		}
		if pod.Request.Memory > node.Allocatable.Memory {
			failed = append(failed, ConstraintRef{
				Code: "C1", Index: 1,
				Detail: fmt.Sprintf("mem %d > %d bytes", pod.Request.Memory, node.Allocatable.Memory),
			})
		}

		// C2: node selector.
		if !pod.NodeSelector.Matches(node.Labels) {
			failed = append(failed, ConstraintRef{
				Code: "C2", Index: 0, Detail: "node selector not satisfied",
			})
		}

		// C3: untolerated taints.
		for ti, t := range node.Taints {
			if !domain.Tolerates(pod.Tolerations, t) {
				failed = append(failed, ConstraintRef{
					Code: "C3", Index: ti,
					Detail: fmt.Sprintf("untolerated taint %s=%s:%s", t.Key, t.Value, t.Effect),
				})
			}
		}

		// C4/C5: must rules evaluated in declaration order, then grouped into
		// family order (all C4 affinity before all C5 anti-affinity) so the
		// reported constraints follow the declared family sequence.
		var c4, c5 []ConstraintRef
		for ri, rule := range pod.MustRules {
			ok, detail := evalMust(p, node, rule)
			if ok {
				continue
			}
			if rule.Mode == domain.AntiAffinity {
				c5 = append(c5, ConstraintRef{Code: "C5", Index: ri, Detail: detail})
				continue
			}
			c4 = append(c4, ConstraintRef{Code: "C4", Index: ri, Detail: detail})
		}
		failed = append(failed, c4...)
		failed = append(failed, c5...)

		if len(failed) == 0 {
			res.Feasible = append(res.Feasible, node.ID)
		} else {
			res.Eliminations = append(res.Eliminations, NodeFailure{
				NodeID: node.ID, Constraints: failed,
			})
		}
	}
	return res
}

// evalMust evaluates one hard rule against the candidate node's topology
// domain. Affinity needs at least one matching node in the domain (the
// candidate included); anti-affinity needs zero other matching nodes.
func evalMust(p *domain.Project, candidate *domain.Node, rule domain.AffinityRule) (bool, string) {
	domainVal, ok := candidate.Topology[rule.TopologyKey]
	if !ok {
		return false, "candidate lacks topology key " + rule.TopologyKey
	}
	matches := matchingInDomain(p, candidate, rule, domainVal)
	switch rule.Mode {
	case domain.Affinity:
		if len(matches) == 0 {
			return false, "no matching node in domain " + domainVal
		}
	case domain.AntiAffinity:
		// matches excludes the candidate; any other matching node violates it.
		if len(matches) > 0 {
			return false, "co-located matching node(s) in domain " + domainVal
		}
	}
	return true, ""
}

// matchingInDomain returns nodes sharing the candidate's topology domain value
// that satisfy the rule selector. The candidate itself is excluded.
func matchingInDomain(p *domain.Project, candidate *domain.Node, rule domain.AffinityRule, domainVal string) []*domain.Node {
	var out []*domain.Node
	for _, n := range p.Nodes {
		if n.ID == candidate.ID {
			continue
		}
		if n.Topology[rule.TopologyKey] != domainVal {
			continue
		}
		if rule.Selector.Matches(n.Labels) {
			out = append(out, n)
		}
	}
	return out
}

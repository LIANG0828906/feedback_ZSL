package engine

import (
	"time"

	"schedlab/internal/domain"
	"schedlab/internal/fixedint"
)

// Path identifiers and the deterministic path-selection reason codes.
const (
	PathExact = "EXACT_NODEWISE"
	PathAgg   = "TOPOLOGY_PREAGGREGATED"

	ReasonUnderThreshold = "P1_under_threshold"
	ReasonOverThreshold  = "P2_over_threshold"
)

// RuleComponent is one per-rule score contribution for a candidate node.
type RuleComponent struct {
	RuleID       string `json:"rule_id"`
	Mode         string `json:"mode"`
	TopologyKey  string `json:"topology_key"`
	WeightBps    int64  `json:"weight_bps"`
	Matched      int64  `json:"matched_in_domain"`
	DomainCap    int64  `json:"domain_cap"`
	Penalty      int64  `json:"cap_penalty"`
	Contribution int64  `json:"raw_contribution"`
}

// NodeScore is the full scoring detail for one candidate node.
type NodeScore struct {
	NodeID        string          `json:"node_id"`
	Components    []RuleComponent `json:"components"`
	RawTotal      int64           `json:"raw_total_saturated"`
	NormalizedBps int64           `json:"normalized_bps"`
}

// ScoreResult bundles scoring output and path diagnostics.
type ScoreResult struct {
	PodID         string        `json:"pod_id"`
	Path          string        `json:"path"`
	ReasonCode    string        `json:"reason_code"`
	Threshold     int64         `json:"threshold"`
	Product       int64         `json:"candidate_x_rule_product"`
	Scores        []NodeScore   `json:"scores"`
	SelectedNode  string        `json:"selected_node"`
	ExactDuration time.Duration `json:"exact_duration"`
	AggDuration   time.Duration `json:"agg_duration"`
}

// Threshold is the declared product (candidate nodes x preferred rules) above
// which the pre-aggregated path is selected. The standard profile stays at or
// below 40 while the large profile reaches 52, so both paths are exercised.
var Threshold int64 = 40

// Score runs the declared path selection and returns per-node detail. Both
// paths are always executed so their timings can be observed; the selected
// path is chosen purely from the deterministic product threshold.
func Score(p *domain.Project, pod *domain.Pod, feasible []string) ScoreResult {
	product := int64(len(feasible)) * int64(len(pod.PreferRules))
	path, reason := PathExact, ReasonUnderThreshold
	if product > Threshold {
		path, reason = PathAgg, ReasonOverThreshold
	}

	startExact := time.Now()
	exact := scoreExact(p, pod, feasible)
	exactDur := time.Since(startExact)

	startAgg := time.Now()
	agg := scoreAggregated(p, pod, feasible)
	aggDur := time.Since(startAgg)

	chosen := exact
	if path == PathAgg {
		chosen = agg
	}
	selected := selectNode(chosen)

	return ScoreResult{
		PodID: pod.ID, Path: path, ReasonCode: reason, Threshold: Threshold,
		Product: product, Scores: chosen, SelectedNode: selected,
		ExactDuration: exactDur, AggDuration: aggDur,
	}
}

func selectNode(scores []NodeScore) string {
	if len(scores) == 0 {
		return ""
	}
	best := 0
	for i := 1; i < len(scores); i++ {
		if scores[i].NormalizedBps > scores[best].NormalizedBps {
			best = i
		}
		// Tie keeps the earlier (canonical) node, giving deterministic choice.
	}
	return scores[best].NodeID
}

// scoreExact recomputes every domain count directly for each candidate.
func scoreExact(p *domain.Project, pod *domain.Pod, feasible []string) []NodeScore {
	byID := p.NodeByID()
	out := make([]NodeScore, 0, len(feasible))
	var maxRaw int64
	for _, nodeID := range feasible {
		candidate := byID[nodeID]
		ns := NodeScore{NodeID: nodeID}
		var raw int64
		for _, rule := range pod.PreferRules {
			comp := scoreRuleExact(p, candidate, rule)
			ns.Components = append(ns.Components, comp)
			sum, _ := fixedint.Add(raw, comp.Contribution)
			raw = sum
		}
		ns.RawTotal = raw
		if raw > maxRaw {
			maxRaw = raw
		}
		out = append(out, ns)
	}
	normalize(out, maxRaw)
	return out
}

func scoreRuleExact(p *domain.Project, candidate *domain.Node, rule domain.AffinityRule) RuleComponent {
	domainVal, ok := candidate.Topology[rule.TopologyKey]
	if !ok {
		return RuleComponent{RuleID: rule.ID, Mode: string(rule.Mode), TopologyKey: rule.TopologyKey,
			WeightBps: rule.WeightBps, DomainCap: rule.MaxCoLocated}
	}
	matched := int64(len(matchingInDomain(p, candidate, rule, domainVal)))
	return buildComponent(rule, matched)
}

// scoreAggregated precomputes matching counts per (rule, domain) once and
// reuses them for every candidate, producing identical integer results.
func scoreAggregated(p *domain.Project, pod *domain.Pod, feasible []string) []NodeScore {
	byID := p.NodeByID()
	// cache[ruleIndex][domainValue] = count of matching nodes excluding the
	// queried candidate; counts are total matching and are adjusted per node.
	type domCount map[string]int64
	cache := make([]domCount, len(pod.PreferRules))
	for ri, rule := range pod.PreferRules {
		cache[ri] = domCount{}
		for _, n := range p.Nodes {
			domVal := n.Topology[rule.TopologyKey]
			if domVal == "" {
				continue
			}
			if rule.Selector.Matches(n.Labels) {
				cache[ri][domVal]++
			}
		}
	}

	out := make([]NodeScore, 0, len(feasible))
	var maxRaw int64
	for _, nodeID := range feasible {
		candidate := byID[nodeID]
		ns := NodeScore{NodeID: nodeID}
		var raw int64
		for ri, rule := range pod.PreferRules {
			domVal := candidate.Topology[rule.TopologyKey]
			var matched int64
			if domVal != "" {
				total := cache[ri][domVal]
				matched = total
				if rule.Selector.Matches(candidate.Labels) {
					matched-- // exclude the candidate itself
				}
			}
			comp := buildComponent(rule, matched)
			ns.Components = append(ns.Components, comp)
			sum, _ := fixedint.Add(raw, comp.Contribution)
			raw = sum
		}
		ns.RawTotal = raw
		if raw > maxRaw {
			maxRaw = raw
		}
		out = append(out, ns)
	}
	normalize(out, maxRaw)
	return out
}

// buildComponent applies the rule polarity, bps weight and same-domain cap
// using only saturated fixed-point integer arithmetic.
func buildComponent(rule domain.AffinityRule, matched int64) RuleComponent {
	c := RuleComponent{
		RuleID: rule.ID, Mode: string(rule.Mode), TopologyKey: rule.TopologyKey,
		WeightBps: rule.WeightBps, Matched: matched, DomainCap: rule.MaxCoLocated,
	}
	var penalty int64
	if rule.MaxCoLocated > 0 && matched > rule.MaxCoLocated {
		penalty = matched - rule.MaxCoLocated
	}
	c.Penalty = penalty

	switch rule.Mode {
	case domain.Affinity:
		// Reward effective (cap-bounded) co-located matches.
		c.Contribution = satMul(rule.WeightBps, matched-penalty)
	case domain.AntiAffinity:
		// Reward spreading: weight * (cap - matched), floored at zero; the cap
		// excess is therefore the quantity that erodes the score.
		eff := rule.MaxCoLocated - matched
		if eff < 0 {
			eff = 0
		}
		c.Contribution = satMul(rule.WeightBps, eff)
	}
	return c
}

// satMul multiplies two int64 values, keeping the saturated boundary value the
// fixed-point helper returns on overflow.
func satMul(a, b int64) int64 {
	v, _ := fixedint.Mul(a, b)
	return v
}

// normalize maps the largest raw total to 10000 bps and scales every node with
// half-away-from-zero rounding. When all totals are zero every node gets 0.
func normalize(out []NodeScore, maxRaw int64) {
	if maxRaw <= 0 {
		for i := range out {
			out[i].NormalizedBps = 0
		}
		return
	}
	for i := range out {
		v, _ := fixedint.Scale(out[i].RawTotal, fixedint.BPS, maxRaw)
		out[i].NormalizedBps = v
	}
}

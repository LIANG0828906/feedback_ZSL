package tail

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"samplingkernel/internal/model"
)

type Input struct {
	TraceID        string
	Rules          []model.TailRule
	Spans          []model.Span
	HeadKeep       bool
	HeadDecisionID string
	Forced         bool
	ForcedEdgeIDs  []string
}

func Evaluate(input Input) model.TailDecision {
	rules := append([]model.TailRule{}, input.Rules...)
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority < rules[j].Priority
		}
		if rules[i].Order != rules[j].Order {
			return rules[i].Order < rules[j].Order
		}
		return rules[i].ID < rules[j].ID
	})

	decision := model.TailDecision{
		TraceID:        input.TraceID,
		Forced:         input.Forced,
		ForcedEdgeIDs:  compactSorted(append([]string{}, input.ForcedEdgeIDs...)),
		MatchedRuleIDs: []string{},
		Path:           []string{fmt.Sprintf("trace=%s start_root_settle", input.TraceID)},
	}

	matchedEvidence := map[string][]string{}
	for _, rule := range rules {
		decision.EvaluatedRuleIDs = append(decision.EvaluatedRuleIDs, rule.ID)
		matched, evidence, explanation := matchRule(rule, input)
		if matched {
			decision.MatchedRuleIDs = append(decision.MatchedRuleIDs, rule.ID)
			matchedEvidence[rule.ID] = evidence
			decision.Path = append(decision.Path, fmt.Sprintf("rule=%s priority=%d order=%d HIT %s evidence=[%s]",
				rule.ID, rule.Priority, rule.Order, explanation, strings.Join(evidence, ",")))
		} else {
			decision.Path = append(decision.Path, fmt.Sprintf("rule=%s priority=%d order=%d MISS %s",
				rule.ID, rule.Priority, rule.Order, explanation))
		}
	}

	if len(decision.MatchedRuleIDs) > 0 {
		decision.SelectedRuleID = decision.MatchedRuleIDs[0]
		decision.BasisSpanIDs = append(decision.BasisSpanIDs, matchedEvidence[decision.SelectedRuleID]...)
		decision.BasisSpanIDs = compactSorted(decision.BasisSpanIDs)
	}

	if input.Forced {
		decision.Keep = true
		decision.DecisionType = "FORCED_RETAIN"
		if decision.SelectedRuleID == "" {
			decision.SelectedRuleID = "FORCED_RETENTION"
		}
		decision.Path = append(decision.Path,
			"forced_marker edges=["+strings.Join(decision.ForcedEdgeIDs, ",")+"] overrides service quota",
			"joint_decision=FORCED_RETAIN")
		if len(decision.ForcedEdgeIDs) > 0 {
			decision.BasisSpanIDs = compactSorted(append(decision.BasisSpanIDs, "edge:"+decision.ForcedEdgeIDs[0]))
		}
		return decision
	}

	if decision.SelectedRuleID != "" {
		decision.Keep = true
		decision.DecisionType = "TAIL_RETAIN"
		decision.Path = append(decision.Path, "select highest_declared_match="+decision.SelectedRuleID, "joint_decision=TAIL_RETAIN")
		return decision
	}

	decision.Keep = false
	decision.DecisionType = "TAIL_DROP"
	decision.Path = append(decision.Path, "no_tail_rule_matched", "joint_decision=TAIL_DROP")
	return decision
}

func matchRule(rule model.TailRule, input Input) (bool, []string, string) {
	condition := rule.Condition
	switch condition.Kind {
	case "span_status", "status":
		wanted := condition.Value
		var evidence []string
		for _, sp := range input.Spans {
			if strings.EqualFold(sp.Status, wanted) {
				evidence = append(evidence, sp.SpanID)
			}
		}
		return len(evidence) > 0, compactSorted(evidence), "status==" + wanted

	case "latency_ms", "latency_gt":
		threshold, _ := strconv.ParseUint(condition.Value, 10, 64)
		var evidence []string
		for _, sp := range input.Spans {
			if sp.LatencyMS > threshold {
				evidence = append(evidence, sp.SpanID)
			}
		}
		return len(evidence) > 0, compactSorted(evidence), fmt.Sprintf("latency_ms>%d", threshold)

	case "tag_eq":
		key := condition.Field
		wanted := condition.Value
		var evidence []string
		for _, sp := range input.Spans {
			if actual, ok := sp.Tags[key]; ok && actual == wanted {
				evidence = append(evidence, sp.SpanID)
			}
		}
		return len(evidence) > 0, compactSorted(evidence), "tag[" + key + "]==" + wanted

	case "head_sampled":
		wanted, _ := strconv.ParseBool(condition.Value)
		if input.HeadKeep == wanted {
			basis := []string{"head:" + input.HeadDecisionID}
			return true, basis, "head_sampled==" + condition.Value
		}
		return false, nil, "head_sampled!=" + condition.Value

	case "span_count":
		threshold, _ := strconv.Atoi(condition.Value)
		count := len(input.Spans)
		matched := compareInt(count, condition.Op, threshold)
		explanation := fmt.Sprintf("span_count(%d)%s%s", count, opOrGTE(condition.Op), condition.Value)
		if matched {
			evidence := make([]string, 0, len(input.Spans))
			for _, sp := range input.Spans {
				evidence = append(evidence, sp.SpanID)
			}
			return true, compactSorted(evidence), explanation
		}
		return false, nil, explanation

	case "ref":
		for _, other := range input.Rules {
			if other.ID == condition.Ref {
				matched, evidence, _ := matchRule(other, input)
				return matched, evidence, "ref->" + condition.Ref
			}
		}
		return false, nil, "missing_ref->" + condition.Ref

	default:
		return false, nil, "unsupported_condition_kind=" + condition.Kind
	}
}

func compareInt(value int, op string, threshold int) bool {
	switch op {
	case "eq", "==":
		return value == threshold
	case "lt", "<":
		return value < threshold
	case "le", "<=":
		return value <= threshold
	case "gt", ">":
		return value > threshold
	default:
		return value >= threshold
	}
}

func opOrGTE(op string) string {
	if op == "" {
		return ">="
	}
	return op
}

func compactSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	sort.Strings(in)
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

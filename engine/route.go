package engine

import (
	"sort"

	"dnslab/spec"
)

// RouteDecision contains the winner and fully ordered adjudication candidates.
type RouteDecision struct {
	Matched    bool
	Rule       spec.Rule
	Basis      string
	Candidates []spec.Rule
}

func (d RouteDecision) CandidateNumbers() []string {
	out := make([]string, 0, len(d.Candidates))
	for _, r := range d.Candidates {
		out = append(out, r.Number)
	}
	return out
}

type scoredRule struct {
	rule   spec.Rule
	labels int
}

func routeWithProject(p *spec.Project, qname string) RouteDecision {
	var matching []scoredRule
	for _, r := range p.Rules {
		if spec.MatchesSuffix(qname, r.Suffix) {
			matching = append(matching, scoredRule{rule: r, labels: spec.LabelCount(r.Suffix)})
		}
	}
	if len(matching) == 0 {
		return RouteDecision{Matched: false, Basis: "no_suffix_match"}
	}

	sort.SliceStable(matching, func(i, j int) bool {
		a, b := matching[i], matching[j]
		if a.labels != b.labels {
			return a.labels > b.labels
		}
		if a.rule.Priority != b.rule.Priority {
			return a.rule.Priority > b.rule.Priority
		}
		return a.rule.Ordinal < b.rule.Ordinal
	})

	candidates := make([]spec.Rule, 0, len(matching))
	for _, m := range matching {
		candidates = append(candidates, m.rule)
	}
	winner := matching[0].rule
	basis := "declared_order_asc"

	maxLabels := matching[0].labels
	longest := make([]scoredRule, 0)
	for _, m := range matching {
		if m.labels == maxLabels {
			longest = append(longest, m)
		}
	}
	if len(longest) == 1 {
		basis = "longest_suffix"
	} else {
		topPriority := longest[0].rule.Priority
		priorityTied := make([]scoredRule, 0)
		for _, m := range longest {
			if m.rule.Priority == topPriority {
				priorityTied = append(priorityTied, m)
			}
		}
		if len(priorityTied) == 1 {
			basis = "priority_desc"
		}
	}
	return RouteDecision{Matched: true, Rule: winner, Basis: basis, Candidates: candidates}
}

package workbench

import (
	"reflect"
	"strconv"

	"meshpolicy/internal/engine"
	"meshpolicy/internal/model"
	"meshpolicy/internal/schema"
	"meshpolicy/internal/store"
)

type EquivalenceReport struct {
	Consistent              bool                `json:"consistent"`
	LoadedVersions          []string            `json:"loaded_versions"`
	CanonicalVersions       map[string]string   `json:"canonical_versions"`
	PolicyOrder             []string            `json:"policy_order"`
	AttemptOrder            []string            `json:"attempt_order"`
	MatchSetsEqual          bool                `json:"match_sets_equal"`
	DecisionSignaturesEqual bool                `json:"decision_signatures_equal"`
	PropagationSetsEqual    bool                `json:"propagation_sets_equal"`
	DecisionCounts          map[string]int      `json:"decision_counts"`
	ArrivalSets             map[string][]string `json:"arrival_sets_first_version,omitempty"`
}

func (s *Service) Equivalence() (*EquivalenceReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &EquivalenceReport{
		LoadedVersions:    []string{"v1", "v2", "v3"},
		CanonicalVersions: map[string]string{},
		DecisionCounts:    map[string]int{},
		ArrivalSets:       map[string][]string{},
		Consistent:        true,
	}
	projects := map[string]*model.Project{}
	reports := map[string]*engine.RunReport{}
	for _, version := range out.LoadedVersions {
		p, err := schema.Load(s.sample.Documents[version])
		if err != nil {
			return nil, err
		}
		r, err := engine.Run(p, engine.RunOptions{Workers: 4, BufferPerNS: 64})
		if err != nil {
			return nil, err
		}
		projects[version] = p
		reports[version] = r
		out.CanonicalVersions[version] = store.CanonicalVersion(p)
		out.DecisionCounts[version] = len(r.Decisions)
	}

	first := projects["v1"]
	out.PolicyOrder = policyIDs(first)
	out.AttemptOrder = attemptIDs(first)
	firstMatches := selectorMatches(first)
	firstDecisions := decisionSig(reports["v1"])
	firstProp := propagationSig(reports["v1"])
	out.MatchSetsEqual = true
	out.DecisionSignaturesEqual = true
	out.PropagationSetsEqual = true
	for _, version := range []string{"v2", "v3"} {
		if !reflect.DeepEqual(policyIDs(first), policyIDs(projects[version])) ||
			!reflect.DeepEqual(attemptIDs(first), attemptIDs(projects[version])) ||
			!reflect.DeepEqual(firstMatches, selectorMatches(projects[version])) {
			out.MatchSetsEqual = false
			out.Consistent = false
		}
		if !reflect.DeepEqual(firstDecisions, decisionSig(reports[version])) {
			out.DecisionSignaturesEqual = false
			out.Consistent = false
		}
		if !reflect.DeepEqual(firstProp, propagationSig(reports[version])) {
			out.PropagationSetsEqual = false
			out.Consistent = false
		}
	}
	for key, value := range firstProp {
		out.ArrivalSets[key] = value
	}
	return out, nil
}

func policyIDs(p *model.Project) []string {
	policies := append([]model.Policy(nil), p.Policies...)
	// Reuse engine candidate ordering indirectly by sorting with the same priority/order key.
	for i := 0; i < len(policies); i++ {
		for j := i + 1; j < len(policies); j++ {
			if higherPolicy(policies[j], policies[i]) {
				policies[i], policies[j] = policies[j], policies[i]
			}
		}
	}
	ids := make([]string, 0, len(policies))
	for _, p := range policies {
		ids = append(ids, p.ID)
	}
	return ids
}

func higherPolicy(a, b model.Policy) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if a.Order != b.Order {
		return a.Order < b.Order
	}
	return a.ID < b.ID
}

func attemptIDs(p *model.Project) []string {
	var out []string
	for _, a := range p.Attempts() {
		out = append(out, a.ID)
	}
	return out
}

func selectorMatches(p *model.Project) map[string][]string {
	workloads := map[string]model.Workload{}
	for _, w := range p.Workloads {
		workloads[w.ID] = w
	}
	out := map[string][]string{}
	for _, pol := range p.Policies {
		var ids []string
		for id, w := range workloads {
			if engine.SelectorMatches(pol.Source, pol.Namespace, w) {
				ids = append(ids, id)
			}
		}
		out[pol.ID] = model.SortedStrings(ids)
	}
	return out
}

func decisionSig(r *engine.RunReport) []string {
	out := make([]string, 0, len(r.Decisions))
	for _, d := range r.Decisions {
		out = append(out, d.AttemptID+":"+boolWord(d.Allow)+":"+d.HitPolicyID+":"+d.Gate.State+":"+d.Gate.MarkerID)
	}
	return out
}

func propagationSig(r *engine.RunReport) map[string][]string {
	out := map[string][]string{}
	if r.Propagation == nil {
		return out
	}
	for _, l := range r.Propagation.Layers {
		if l.Generation == 0 {
			continue
		}
		out[l.ChangeID+"/G"+strconv.Itoa(l.Generation)] = append([]string(nil), l.ArrivalSet...)
	}
	return out
}

func boolWord(v bool) string {
	if v {
		return "allow"
	}
	return "deny"
}

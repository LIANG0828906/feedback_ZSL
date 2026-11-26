package gen

import (
	"fmt"
	"math/rand"

	"meshpolicy/internal/model"
)

const DefaultSeed int64 = 42042

type GateAnchor struct {
	AttemptID     string `json:"attempt_id"`
	Allow         bool   `json:"allow"`
	HitPolicyID   string `json:"hit_policy_id"`
	Gate          string `json:"gate,omitempty"`
	MarkerGen     int    `json:"marker_generation,omitempty"`
	AppliedPolicy string `json:"applied_policy_id,omitempty"`
}

type ReplacementScenario struct {
	Namespace string       `json:"namespace"`
	TargetID  string       `json:"target_id"`
	RuleID    string       `json:"rule_id"`
	AtSeq     int64        `json:"at_seq"`
	NewPolicy model.Policy `json:"new_policy"`
}

type Truth struct {
	PolicyPriorityOrder []string              `json:"policy_priority_order"`
	AttemptOrder        []string              `json:"attempt_order"`
	SelectorMatches     map[string][]string   `json:"selector_matches"`
	Anchors             []GateAnchor          `json:"anchors"`
	Replacements        []ReplacementScenario `json:"replacements"`
}

type Sample struct {
	Seed      int64             `json:"seed"`
	Project   *model.Project    `json:"project"`
	Documents map[string][]byte `json:"documents"`
	Truth     Truth             `json:"truth"`
}

func Generate(seed int64) *Sample {
	if seed == 0 {
		seed = DefaultSeed
	}
	rng := rand.New(rand.NewSource(seed))
	p := buildProject(seed)
	truth := buildTruth(p)

	docs := map[string][]byte{}
	docs["v1"] = mustEncodeV1(p, rng)
	docs["v2"] = mustEncodeV2(p, rng)
	docs["v3"] = mustEncodeV3(p, rng)

	return &Sample{Seed: seed, Project: p, Documents: docs, Truth: truth}
}

func buildProject(seed int64) *model.Project {
	p := &model.Project{
		ID:          fmt.Sprintf("mesh-policy-seed-%d", seed),
		Seed:        seed,
		TickMS:      100,
		DefaultDeny: true,
		PolicyLimits: map[string]int{
			"edge":  4,
			"order": 5,
			"pay":   7,
		},
		Workloads: []model.Workload{
			{
				ID: "gateway", Namespace: "edge", ServiceAccount: "gateway-sa",
				Labels:    map[string]string{"app": "gateway", "tier": "edge"},
				DependsOn: []string{"order"},
			},
			{
				ID: "static", Namespace: "edge", ServiceAccount: "static-sa",
				Labels: map[string]string{"app": "static", "tier": "edge"},
			},
			{
				ID: "order", Namespace: "order", ServiceAccount: "order-sa",
				Labels:    map[string]string{"app": "order-api", "tier": "api"},
				DependsOn: []string{"pay"},
			},
			{
				ID: "pay", Namespace: "pay", ServiceAccount: "pay-sa",
				Labels:    map[string]string{"app": "pay-api", "tier": "api", "pci": "true"},
				DependsOn: []string{"ledger", "audit"},
			},
			{
				ID: "ledger", Namespace: "pay", ServiceAccount: "ledger-sa",
				Labels: map[string]string{"app": "ledger", "tier": "data"},
			},
			{
				ID: "audit", Namespace: "pay", ServiceAccount: "audit-sa",
				Labels: map[string]string{"app": "audit", "tier": "compliance"},
			},
		},
		Policies: []model.Policy{
			{
				ID: "pol-edge-gateway-allow", Namespace: "order",
				Source:   model.Selector{Namespace: "edge", MatchLabels: map[string]string{"app": "gateway"}, ServiceAccounts: []string{"gateway-sa"}, WorkloadRefs: []string{"gateway"}},
				PortFrom: 8080, PortTo: 8080, Action: model.ActionAllow, Priority: 100, Order: 1,
				StartSeq: 1, EndSeq: 99, State: model.StateActive, Revision: 1,
			},
			{
				ID: "pol-edge-tier-deny-shadow", Namespace: "order",
				Source:   model.Selector{Namespace: "edge", MatchLabels: map[string]string{"tier": "edge"}},
				PortFrom: 8080, PortTo: 8080, Action: model.ActionDeny, Priority: 50, Order: 2,
				StartSeq: 1, EndSeq: 99, State: model.StateActive, Revision: 1,
			},
			{
				ID: "pol-pay-order-allow", Namespace: "pay",
				Source:   model.Selector{Namespace: "order", MatchLabels: map[string]string{"app": "order-api"}, ServiceAccounts: []string{"order-sa"}, WorkloadRefs: []string{"order"}},
				PortFrom: 8080, PortTo: 8080, Action: model.ActionAllow, Priority: 100, Order: 1,
				StartSeq: 1, EndSeq: 99, State: model.StateActive, Revision: 1,
			},
			{
				ID: "pol-pay-api-tier-deny", Namespace: "pay",
				Source:   model.Selector{Namespace: "order", MatchLabels: map[string]string{"tier": "api"}},
				PortFrom: 8080, PortTo: 8080, Action: model.ActionDeny, Priority: 90, Order: 2,
				StartSeq: 1, EndSeq: 99, State: model.StateActive, Revision: 1,
			},
			{
				ID: "pol-pay-ledger-range-allow", Namespace: "pay",
				Source:   model.Selector{Namespace: "pay", MatchLabels: map[string]string{"app": "pay-api"}, ServiceAccounts: []string{"pay-sa"}, WorkloadRefs: []string{"pay"}},
				PortFrom: 7000, PortTo: 7010, Action: model.ActionAllow, Priority: 100, Order: 3,
				StartSeq: 1, EndSeq: 99, State: model.StateActive, Revision: 1,
			},
			{
				ID: "pol-pay-audit-deny", Namespace: "pay",
				Source:   model.Selector{Namespace: "pay", MatchLabels: map[string]string{"app": "pay-api"}},
				PortFrom: 9000, PortTo: 9000, Action: model.ActionDeny, Priority: 100, Order: 4,
				StartSeq: 1, EndSeq: 99, State: model.StateActive, Revision: 1,
			},
			{
				ID: "pol-pay-expired-legacy", Namespace: "pay",
				Source:   model.Selector{Namespace: "pay", MatchLabels: map[string]string{"app": "legacy"}},
				PortFrom: 9090, PortTo: 9090, Action: model.ActionDeny, Priority: 10, Order: 5,
				StartSeq: 1, EndSeq: 2, State: model.StateExpired, Revision: 1,
			},
			{
				ID: "pol-order-revoked-legacy", Namespace: "order",
				Source:   model.Selector{Namespace: "order", MatchLabels: map[string]string{"app": "legacy"}},
				PortFrom: 9091, PortTo: 9091, Action: model.ActionDeny, Priority: 10, Order: 3,
				StartSeq: 1, EndSeq: 99, State: model.StateRevoked, Revision: 1,
			},
		},
		Timeline: []model.Event{},
	}

	attempt := func(id string, seq int64, order int, src, dst string, port int) model.Event {
		return model.Event{Seq: seq, Order: order, Attempt: &model.ConnectionAttempt{
			ID: id, Seq: seq, Order: order, SourceWorkloadID: src, TargetWorkloadID: dst,
			Port: port, Protocol: "TCP",
		}}
	}

	p.Timeline = []model.Event{
		attempt("A-01", 1, 10, "gateway", "order", 8080),
		attempt("A-02", 1, 20, "order", "pay", 8080),
		attempt("A-03", 1, 30, "pay", "ledger", 7005),
		attempt("A-04", 2, 10, "gateway", "order", 8080),
		attempt("A-05", 2, 20, "order", "pay", 8080),
		attempt("A-06", 2, 30, "pay", "ledger", 7005),
		{
			Seq: 3, Order: 5,
			Change: &model.PolicyChange{
				ID: "C-01", Seq: 3, Order: 5, Kind: model.ChangePriority, Namespace: "pay",
				PolicyID: "pol-pay-order-allow", EditedFields: []string{"priority"},
				Before: clonePolicy(mustPolicy(p, "pol-pay-order-allow")),
				After:  policyWithPriority(mustPolicy(p, "pol-pay-order-allow"), 40),
			},
		},
		attempt("A-07", 3, 10, "order", "pay", 8080),
		attempt("A-08", 3, 20, "pay", "ledger", 7005),
		attempt("A-09", 4, 10, "order", "pay", 8080),
		attempt("A-10", 4, 20, "pay", "ledger", 7005),
		attempt("A-11", 4, 30, "gateway", "order", 8080),
		attempt("A-12", 5, 10, "order", "pay", 8080),
		attempt("A-13", 5, 20, "pay", "ledger", 7005),
		attempt("A-14", 6, 10, "order", "pay", 8080),
		attempt("A-15", 6, 20, "pay", "ledger", 7005),
	}

	return p
}

func buildTruth(p *model.Project) Truth {
	matches := map[string][]string{}
	for _, pol := range p.Policies {
		var ids []string
		for _, w := range p.Workloads {
			if selectorMatchesTruth(pol.Source, w) {
				ids = append(ids, w.ID)
			}
		}
		matches[pol.ID] = model.SortedStrings(ids)
	}

	newPay := mustPolicy(p, "pol-pay-order-allow")
	newPay.Priority = 40
	newPay.Revision = 2

	return Truth{
		PolicyPriorityOrder: []string{
			"pol-pay-api-tier-deny",
			"pol-pay-order-allow",
			"pol-pay-ledger-range-allow",
			"pol-pay-audit-deny",
			"pol-pay-expired-legacy",
		},
		AttemptOrder: []string{
			"A-01", "A-02", "A-03", "A-04", "A-05", "A-06",
			"A-07", "A-08", "A-09", "A-10", "A-11",
			"A-12", "A-13", "A-14", "A-15",
		},
		SelectorMatches: matches,
		Anchors: []GateAnchor{
			{AttemptID: "A-01", Allow: true, HitPolicyID: "pol-edge-gateway-allow", AppliedPolicy: "pol-edge-gateway-allow"},
			{AttemptID: "A-02", Allow: true, HitPolicyID: "pol-pay-order-allow", AppliedPolicy: "pol-pay-order-allow"},
			{AttemptID: "A-03", Allow: true, HitPolicyID: "pol-pay-ledger-range-allow", AppliedPolicy: "pol-pay-ledger-range-allow"},
			{AttemptID: "A-07", Allow: true, HitPolicyID: "pol-pay-api-tier-deny", Gate: "fallback_committed", MarkerGen: 1, AppliedPolicy: "pol-pay-order-allow"},
			{AttemptID: "A-08", Allow: true, HitPolicyID: "pol-pay-ledger-range-allow", Gate: "fallback_committed", MarkerGen: 2, AppliedPolicy: "pol-pay-ledger-range-allow"},
			{AttemptID: "A-09", Allow: true, HitPolicyID: "pol-pay-api-tier-deny", Gate: "fallback_committed", MarkerGen: 1, AppliedPolicy: "pol-pay-order-allow"},
			{AttemptID: "A-10", Allow: true, HitPolicyID: "pol-pay-ledger-range-allow", Gate: "fallback_committed", MarkerGen: 2, AppliedPolicy: "pol-pay-ledger-range-allow"},
			{AttemptID: "A-12", Allow: false, HitPolicyID: "pol-pay-api-tier-deny", Gate: "applied_new", AppliedPolicy: "pol-pay-api-tier-deny"},
			{AttemptID: "A-13", Allow: true, HitPolicyID: "pol-pay-ledger-range-allow", Gate: "fallback_committed", MarkerGen: 2, AppliedPolicy: "pol-pay-ledger-range-allow"},
			{AttemptID: "A-14", Allow: false, HitPolicyID: "pol-pay-api-tier-deny", Gate: "applied_new", AppliedPolicy: "pol-pay-api-tier-deny"},
			{AttemptID: "A-15", Allow: true, HitPolicyID: "pol-pay-ledger-range-allow", Gate: "applied_new", AppliedPolicy: "pol-pay-ledger-range-allow"},
		},
		Replacements: []ReplacementScenario{
			{
				Namespace: "pay", TargetID: "pol-pay-expired-legacy", RuleID: "R-REPLACE-EXPIRED-WINDOW", AtSeq: 6,
				NewPolicy: model.Policy{
					ID: "pol-pay-legacy-v2", Namespace: "pay",
					Source:   model.Selector{Namespace: "pay", MatchLabels: map[string]string{"app": "legacy-api"}},
					PortFrom: 9092, PortTo: 9092, Action: model.ActionDeny, Priority: 200, Order: 6,
					StartSeq: 6, EndSeq: 99, State: model.StateActive,
				},
			},
			{
				Namespace: "order", TargetID: "pol-order-revoked-legacy", RuleID: "R-REPLACE-CONFIRMED-DEAD", AtSeq: 6,
				NewPolicy: model.Policy{
					ID: "pol-order-legacy-v2", Namespace: "order",
					Source:   model.Selector{Namespace: "order", MatchLabels: map[string]string{"app": "legacy-api"}},
					PortFrom: 9093, PortTo: 9093, Action: model.ActionAllow, Priority: 120, Order: 4,
					StartSeq: 6, EndSeq: 99, State: model.StateActive,
				},
			},
		},
	}
}

func mustPolicy(p *model.Project, id string) model.Policy {
	pol, ok := p.PolicyByID(id)
	if !ok {
		panic("missing generator policy " + id)
	}
	return pol
}

func clonePolicy(pol model.Policy) *model.Policy {
	out := pol
	out.Source.MatchLabels = cloneLabels(pol.Source.MatchLabels)
	out.Source.ServiceAccounts = append([]string(nil), pol.Source.ServiceAccounts...)
	out.Source.WorkloadRefs = append([]string(nil), pol.Source.WorkloadRefs...)
	return &out
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func policyWithPriority(pol model.Policy, priority int) *model.Policy {
	out := clonePolicy(pol)
	out.Priority = priority
	out.Revision++
	return out
}

func selectorMatchesTruth(sel model.Selector, w model.Workload) bool {
	if sel.Namespace != "" && sel.Namespace != "*" && sel.Namespace != w.Namespace {
		return false
	}
	for k, v := range sel.MatchLabels {
		if w.Labels[k] != v {
			return false
		}
	}
	if len(sel.ServiceAccounts) > 0 {
		ok := false
		for _, sa := range sel.ServiceAccounts {
			if sa == w.ServiceAccount {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

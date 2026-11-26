package engine

import (
	"fmt"
	"sort"

	"meshpolicy/internal/model"
)

const (
	ReasonPriorityDecision = "R-POLICY-PRIORITY-ORDER"
	ReasonDefaultDeny      = "R-DEFAULT-DENY-MARKER"
	ReasonDefaultAllow     = "R-DEFAULT-ALLOW-NO-MATCH"
	ReasonGateFallback     = "R-GATE-FALLBACK-COMMITTED"
	ReasonGateDeny         = "R-GATE-DENIED-NO-BASELINE"
)

func SelectorMatches(sel model.Selector, policyNamespace string, w model.Workload) bool {
	ns := sel.Namespace
	if ns == "" {
		ns = policyNamespace
	}
	if ns != "*" && ns != w.Namespace {
		return false
	}
	for key, value := range sel.MatchLabels {
		if w.Labels == nil || w.Labels[key] != value {
			return false
		}
	}
	if len(sel.ServiceAccounts) > 0 {
		matched := false
		for _, sa := range sel.ServiceAccounts {
			if sa == w.ServiceAccount {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func PortMatches(pol model.Policy, port int) bool {
	return port >= pol.PortFrom && port <= pol.PortTo
}

func IsLiveAt(pol model.Policy, seq int64) bool {
	if pol.State != model.StateActive {
		return false
	}
	return seq >= pol.StartSeq && seq <= pol.EndSeq
}

type Candidate struct {
	Policy model.Policy
	Basis  Basis
}

func MatchCandidates(policies []model.Policy, workloads map[string]model.Workload, a model.ConnectionAttempt, seq int64) []Candidate {
	target, ok := workloads[a.TargetWorkloadID]
	if !ok {
		return nil
	}
	source, ok := workloads[a.SourceWorkloadID]
	if !ok {
		return nil
	}
	var out []Candidate
	for _, pol := range policies {
		if pol.Namespace != target.Namespace {
			continue
		}
		if !IsLiveAt(pol, seq) {
			continue
		}
		if !SelectorMatches(pol.Source, pol.Namespace, source) {
			continue
		}
		if !PortMatches(pol, a.Port) {
			continue
		}
		out = append(out, Candidate{Policy: pol})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Policy.Priority != out[j].Policy.Priority {
			return out[i].Policy.Priority > out[j].Policy.Priority
		}
		if out[i].Policy.Order != out[j].Policy.Order {
			return out[i].Policy.Order < out[j].Policy.Order
		}
		return out[i].Policy.ID < out[j].Policy.ID
	})
	for i := range out {
		pol := out[i].Policy
		reason := "shadowed by higher priority or earlier declaration"
		if i == 0 {
			reason = ReasonPriorityDecision
		}
		out[i].Basis = Basis{
			PolicyID: pol.ID, Priority: pol.Priority, Order: pol.Order, Action: pol.Action,
			State: string(pol.State), Port: fmt.Sprintf("%d-%d", pol.PortFrom, pol.PortTo), Reason: reason,
		}
	}
	return out
}

func EvaluateCandidate(p *model.Project, policies []model.Policy, a model.ConnectionAttempt, seq int64) Decision {
	workloads := map[string]model.Workload{}
	for _, w := range p.Workloads {
		workloads[w.ID] = w
	}
	return EvaluateCandidateWithMap(workloads, p.DefaultDeny, policies, a, seq)
}

func EvaluateCandidateWithMap(workloads map[string]model.Workload, defaultDeny bool, policies []model.Policy, a model.ConnectionAttempt, seq int64) Decision {
	targetNS := ""
	if t, ok := workloads[a.TargetWorkloadID]; ok {
		targetNS = t.Namespace
	}
	candidates := MatchCandidates(policies, workloads, a, seq)
	d := Decision{
		AttemptID: a.ID, Seq: a.Seq, Order: a.Order, Namespace: targetNS, FlowKey: a.FlowKey(),
		SourceWorkloadID: a.SourceWorkloadID, TargetWorkloadID: a.TargetWorkloadID,
		Gate: GateInfo{State: "clean", Arrived: true, Acknowledged: true},
	}
	if len(candidates) > 0 {
		winner := candidates[0].Policy
		d.CandidateAllow = winner.Action == model.ActionAllow
		d.CandidateHitID = winner.ID
		d.HitPolicyID = winner.ID
		d.Allow = d.CandidateAllow
		d.AppliedPolicyID = winner.ID
		d.AppliedRevision = winner.Revision
		d.ReasonCode = ReasonPriorityDecision
		d.Basis = make([]Basis, 0, len(candidates))
		for _, c := range candidates {
			d.Basis = append(d.Basis, c.Basis)
		}
		d.Gate.AppliedPolicyID = winner.ID
		d.Gate.AppliedAllow = &d.Allow
		d.Gate.AppliedRevision = winner.Revision
		return d
	}
	d.CandidateAllow = !defaultDeny
	d.Allow = d.CandidateAllow
	d.ReasonCode = ReasonDefaultDeny
	if !defaultDeny {
		d.ReasonCode = ReasonDefaultAllow
	}
	d.Gate.AppliedAllow = &d.Allow
	return d
}

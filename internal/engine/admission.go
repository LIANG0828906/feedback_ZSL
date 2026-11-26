package engine

import (
	"fmt"

	"meshpolicy/internal/model"
	"meshpolicy/internal/xerr"
)

const (
	MinPriority        = 0
	MaxPriority        = 1000
	MaxDeclaredLimit   = 10000
	DefaultPolicyLimit = 20
	RuleExpiredWindow  = "R-REPLACE-EXPIRED-WINDOW"
	RuleConfirmedDead  = "R-REPLACE-CONFIRMED-DEAD"
)

type ReplaceRequest struct {
	Namespace string       `json:"namespace"`
	TargetID  string       `json:"target_id"`
	AtSeq     int64        `json:"at_seq"`
	Order     int          `json:"order,omitempty"`
	NewPolicy model.Policy `json:"new_policy"`
}

type ReplacementResult struct {
	Namespace   string              `json:"namespace"`
	TargetID    string              `json:"target_id"`
	NewPolicyID string              `json:"new_policy_id"`
	RuleID      string              `json:"rule_id"`
	Basis       string              `json:"basis"`
	EntryCount  int                 `json:"entry_count"`
	Limit       int                 `json:"limit"`
	Change      *model.PolicyChange `json:"change"`
}

func ReplacePolicy(p *model.Project, req ReplaceRequest) (*ReplacementResult, error) {
	limit := DefaultPolicyLimit
	if declared, ok := p.PolicyLimits[req.Namespace]; ok {
		limit = declared
	}
	if _, declared := p.PolicyLimits[req.Namespace]; declared {
		if limit < 0 || limit > MaxDeclaredLimit {
			return nil, xerr.New(xerr.EPolicyLimitInvalid, "declared namespace policy limit is outside stable bounds", map[string]any{
				"namespace": req.Namespace, "limit": limit, "min": 0, "max": MaxDeclaredLimit,
			})
		}
		if limit == 0 {
			return nil, xerr.New(xerr.EPolicyLimitZero, "namespace policy limit is zero; no write is admitted", map[string]any{
				"namespace": req.Namespace, "limit": 0, "rule": "ADMISSION-LIMIT-ZERO",
			})
		}
	}
	if req.NewPolicy.Priority < MinPriority || req.NewPolicy.Priority > MaxPriority {
		return nil, xerr.New(xerr.EPriorityRange, "declared priority is outside stable bounds", map[string]any{
			"namespace": req.Namespace, "policy_id": req.NewPolicy.ID,
			"priority": req.NewPolicy.Priority, "min": MinPriority, "max": MaxPriority,
		})
	}
	if req.NewPolicy.PortFrom < 1 || req.NewPolicy.PortFrom > 65535 ||
		req.NewPolicy.PortTo < 1 || req.NewPolicy.PortTo > 65535 ||
		req.NewPolicy.PortFrom > req.NewPolicy.PortTo {
		return nil, xerr.New(xerr.EPortRangeInvalid, "replacement policy has invalid port range", map[string]any{
			"namespace": req.Namespace, "policy_id": req.NewPolicy.ID,
			"from": req.NewPolicy.PortFrom, "to": req.NewPolicy.PortTo,
		})
	}
	if req.NewPolicy.ID == "" {
		return nil, xerr.New(xerr.EFieldConflict, "replacement policy id is required", map[string]any{"namespace": req.Namespace})
	}

	targetIdx := -1
	for i := range p.Policies {
		if p.Policies[i].ID == req.TargetID {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return nil, xerr.New(xerr.EPolicyNotFound, "replacement target policy was not found", map[string]any{
			"namespace": req.Namespace, "target_id": req.TargetID,
		})
	}
	target := p.Policies[targetIdx]
	if target.Namespace != req.Namespace {
		return nil, xerr.New(xerr.EPolicyNotFound, "replacement target is not in requested namespace", map[string]any{
			"requested_namespace": req.Namespace, "target_namespace": target.Namespace, "target_id": req.TargetID,
		})
	}

	ruleID := ""
	basis := ""
	if target.State == model.StateRevoked {
		ruleID = RuleConfirmedDead
		basis = "target state=revoked is confirmed invalid"
	} else if target.EndSeq < req.AtSeq {
		ruleID = RuleExpiredWindow
		basis = fmt.Sprintf("target window end_seq=%d is before write seq=%d", target.EndSeq, req.AtSeq)
	} else {
		return nil, xerr.New(xerr.EReplaceTargetActive, "only confirmed-dead or window-expired entries may be replaced", map[string]any{
			"namespace": req.Namespace, "target_id": target.ID, "state": target.State,
			"start_seq": target.StartSeq, "end_seq": target.EndSeq, "at_seq": req.AtSeq,
		})
	}

	currentCount := 0
	for _, pol := range p.Policies {
		if pol.Namespace == req.Namespace && pol.State != model.StateReplaced && pol.State != model.StateDeleted {
			currentCount++
		}
	}
	if currentCount > limit {
		return nil, xerr.New(xerr.ENamespacePolicyLimit, "active policy count already reaches namespace limit", map[string]any{
			"namespace": req.Namespace, "current_count": currentCount, "limit": limit,
		})
	}

	before := target
	p.Policies[targetIdx].State = model.StateReplaced
	p.Policies[targetIdx].ReplacedBy = req.NewPolicy.ID

	newPol := req.NewPolicy
	newPol.Namespace = req.Namespace
	newPol.State = model.StateActive
	newPol.Replaces = target.ID
	if newPol.Revision == 0 {
		newPol.Revision = target.Revision + 1
	}
	p.Policies = append(p.Policies, newPol)

	order := req.Order
	if order == 0 {
		order = 1000 + len(p.Timeline)
	}
	change := &model.PolicyChange{
		ID: fmt.Sprintf("C-REPLACE-%s", req.NewPolicy.ID), Seq: req.AtSeq, Order: order,
		Kind: model.ChangeAdmissionReplace, Namespace: req.Namespace, PolicyID: newPol.ID,
		EditedFields: []string{"replacement"}, Before: &before, After: &newPol,
		ReplacementRuleID: ruleID,
	}
	p.Timeline = append(p.Timeline, model.Event{Seq: req.AtSeq, Order: order, Change: change})

	return &ReplacementResult{
		Namespace: req.Namespace, TargetID: target.ID, NewPolicyID: newPol.ID,
		RuleID: ruleID, Basis: basis, EntryCount: currentCount, Limit: limit, Change: change,
	}, nil
}

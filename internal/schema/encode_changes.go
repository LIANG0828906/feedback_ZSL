package schema

import "meshpolicy/internal/model"

func v1PolicyDTO(pol *model.Policy) *v1Policy {
	if pol == nil {
		return nil
	}
	actionCode := v1ActionCode(pol.Action)
	dto := &v1Policy{
		PolicyID: pol.ID, Namespace: pol.Namespace,
		SourceSelector: &v1Selector{
			Namespace: pol.Source.Namespace, MatchLabels: pol.Source.MatchLabels,
			ServiceAccount: pol.Source.ServiceAccounts, WorkloadRefs: pol.Source.WorkloadRefs,
		},
		Action: string(pol.Action), ActionCode: &actionCode,
		Priority: pol.Priority, DeclaredOrder: pol.Order,
		Window: v1Window{StartTick: pol.StartSeq, EndTick: pol.EndSeq},
		State:  string(pol.State), Revision: pol.Revision,
		Replaces: pol.Replaces, ReplacedBy: pol.ReplacedBy,
	}
	if pol.PortFrom == pol.PortTo {
		port := pol.PortFrom
		dto.TargetPort = &port
	} else {
		dto.PortRange = []int{pol.PortFrom, pol.PortTo}
	}
	return dto
}

func encodeV1Change(c *model.PolicyChange) *v1Change {
	if c == nil {
		return nil
	}
	return &v1Change{
		ChangeID: c.ID, Kind: string(c.Kind), Namespace: c.Namespace, PolicyID: c.PolicyID,
		WorkloadID: c.WorkloadID, EditedFields: c.EditedFields, ReplacementRuleID: c.ReplacementRuleID,
		Before: v1PolicyDTO(c.Before), After: v1PolicyDTO(c.After),
	}
}

func v2PolicyDTO(pol *model.Policy, tickMS int64) *v2Policy {
	if pol == nil {
		return nil
	}
	code := v2ActionCode(pol.Action)
	return &v2Policy{
		PolicyID: pol.ID, Namespace: pol.Namespace,
		SourceSelector: &v2Selector{Kind: "namespace_tree", NamespaceTree: map[string]v2SelectorNode{
			selectorNamespace(pol.Source, pol.Namespace): {
				MatchLabels: pol.Source.MatchLabels, ServiceAccounts: pol.Source.ServiceAccounts,
				WorkloadRefs: pol.Source.WorkloadRefs,
			},
		}},
		PortRange:       &v2PortRange{From: pol.PortFrom, To: pol.PortTo},
		ActionName:      v2ActionName(pol.Action),
		ActionCode:      &code,
		Priority:        pol.Priority,
		DeclaredOrder:   pol.Order,
		EffectiveWindow: &v2Window{StartMS: pol.StartSeq * tickMS, EndMS: pol.EndSeq * tickMS},
		State:           string(pol.State), Revision: pol.Revision,
		Replaces: pol.Replaces, ReplacedBy: pol.ReplacedBy,
	}
}

func encodeV2Change(c *model.PolicyChange, tickMS int64) *v2Change {
	if c == nil {
		return nil
	}
	return &v2Change{
		ChangeID: c.ID, Kind: string(c.Kind), Namespace: c.Namespace, PolicyID: c.PolicyID,
		WorkloadID: c.WorkloadID, EditedFields: c.EditedFields, ReplacementRuleID: c.ReplacementRuleID,
		Before: v2PolicyDTO(c.Before, tickMS), After: v2PolicyDTO(c.After, tickMS),
	}
}

func v3PolicyDTO(pol *model.Policy) *v3Policy {
	if pol == nil {
		return nil
	}
	return &v3Policy{
		EntryID: pol.ID, NamespaceName: pol.Namespace,
		SelectorRef:      "sel-" + pol.ID,
		PortExpression:   encodePortExpression(pol.PortFrom, pol.PortTo),
		Effect:           string(pol.Action),
		PriorityValue:    pol.Priority,
		DeclarationIndex: pol.Order,
		TickWindow:       []int64{pol.StartSeq, pol.EndSeq},
		LifecycleState:   string(pol.State), Revision: pol.Revision,
		Replaces: pol.Replaces, ReplacedBy: pol.ReplacedBy,
	}
}

func encodeV3Change(c *model.PolicyChange) *v3Edit {
	if c == nil {
		return nil
	}
	return &v3Edit{
		EditID: c.ID, Kind: string(c.Kind), NamespaceName: c.Namespace, EntryRef: c.PolicyID,
		WorkloadRef: c.WorkloadID, ChangedFields: c.EditedFields, ReplacementRuleID: c.ReplacementRuleID,
		Before: v3PolicyDTO(c.Before), After: v3PolicyDTO(c.After),
	}
}

package schema

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"meshpolicy/internal/model"
	"meshpolicy/internal/xerr"
)

type envelope struct {
	Schema     string `json:"schema"`
	APIVersion string `json:"apiVersion"`
}

func Load(data []byte) (*model.Project, error) {
	var head envelope
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, xerr.Wrap(xerr.EDocumentInvalid, "schema document is not valid JSON", err, map[string]any{"path": "$"})
	}
	version := head.Schema
	if version == "" {
		version = head.APIVersion
	}
	switch version {
	case V1:
		var doc v1Doc
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, xerr.Wrap(xerr.EDocumentInvalid, "v1 document parse failed", err, map[string]any{"schema": V1})
		}
		p, err := decodeV1(&doc)
		if err != nil {
			return nil, err
		}
		p.SchemaSource = V1
		return p, ValidateProject(p)
	case V2:
		var doc v2Doc
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, xerr.Wrap(xerr.EDocumentInvalid, "v2 document parse failed", err, map[string]any{"schema": V2})
		}
		p, err := decodeV2(&doc)
		if err != nil {
			return nil, err
		}
		p.SchemaSource = V2
		return p, ValidateProject(p)
	case V3:
		var doc v3Doc
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, xerr.Wrap(xerr.EDocumentInvalid, "v3 document parse failed", err, map[string]any{"schema": V3})
		}
		p, err := decodeV3(&doc)
		if err != nil {
			return nil, err
		}
		p.SchemaSource = V3
		return p, ValidateProject(p)
	default:
		return nil, xerr.New(xerr.ESchemaUnsupported, "unsupported schema version", map[string]any{
			"path":    "$.schema/apiVersion",
			"value":   version,
			"allowed": []string{V1, V2, V3},
		})
	}
}

func decodeV1(doc *v1Doc) (*model.Project, error) {
	tickMS := doc.TickMS
	if tickMS <= 0 {
		tickMS = 100
	}
	p := &model.Project{
		ID: doc.ProjectID, Seed: doc.Seed, TickMS: tickMS,
		DefaultDeny: true, PolicyLimits: doc.PolicyLimits,
	}
	if doc.DefaultDeny != nil {
		p.DefaultDeny = *doc.DefaultDeny
	}
	for i, w := range doc.Workloads {
		p.Workloads = append(p.Workloads, model.Workload{
			ID: w.WorkloadID, Namespace: w.Namespace, ServiceAccount: w.ServiceAccount,
			Labels: w.Labels, DependsOn: w.DependsOn, Exited: w.Exited,
		})
		_ = i
	}
	for i := range doc.Policies {
		pol, err := v1PolicyFromDTO(&doc.Policies[i], fmt.Sprintf("$.policies[%d]", i))
		if err != nil {
			return nil, err
		}
		p.Policies = append(p.Policies, *pol)
	}
	for i := range doc.Events {
		ev, err := v1EventFromDTO(&doc.Events[i], fmt.Sprintf("$.events[%d]", i))
		if err != nil {
			return nil, err
		}
		p.Timeline = append(p.Timeline, ev)
	}
	return p, nil
}

func v1PolicyFromDTO(d *v1Policy, path string) (*model.Policy, error) {
	action, err := resolveActionV1(d.Action, d.ActionCode, path+".action_code")
	if err != nil {
		return nil, err
	}
	from, to, err := portFromV1(d, path)
	if err != nil {
		return nil, err
	}
	sel := model.Selector{}
	if d.SourceSelector != nil {
		sel = model.Selector{
			Namespace: d.SourceSelector.Namespace, MatchLabels: d.SourceSelector.MatchLabels,
			ServiceAccounts: d.SourceSelector.ServiceAccount, WorkloadRefs: d.SourceSelector.WorkloadRefs,
		}
	}
	state := model.PolicyState(d.State)
	if state == "" {
		state = model.StateActive
	}
	return &model.Policy{
		ID: d.PolicyID, Namespace: d.Namespace, Source: sel,
		PortFrom: from, PortTo: to, Action: action, Priority: d.Priority, Order: d.DeclaredOrder,
		StartSeq: d.Window.StartTick, EndSeq: d.Window.EndTick, State: state,
		Revision: d.Revision, Replaces: d.Replaces, ReplacedBy: d.ReplacedBy,
	}, nil
}

func portFromV1(d *v1Policy, path string) (int, int, error) {
	if d.TargetPort != nil {
		return validatePortRange(*d.TargetPort, *d.TargetPort, path+".target_port")
	}
	if len(d.PortRange) == 2 {
		return validatePortRange(d.PortRange[0], d.PortRange[1], path+".port_range")
	}
	return 0, 0, xerr.New(xerr.EPortRangeInvalid, "policy must declare target_port or [from,to] port_range", map[string]any{
		"path": path, "policy_id": d.PolicyID,
	})
}

func resolveActionV1(text string, code *int, path string) (model.Action, error) {
	var byText model.Action
	hasText := strings.TrimSpace(text) != ""
	if hasText {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "allow", "allowed":
			byText = model.ActionAllow
		case "deny", "denied":
			byText = model.ActionDeny
		default:
			return "", xerr.New(xerr.EActionConflict, "unknown action text", map[string]any{"path": path, "value": text})
		}
	}
	var byCode model.Action
	hasCode := code != nil
	if hasCode {
		switch *code {
		case 0:
			byCode = model.ActionAllow
		case 1:
			byCode = model.ActionDeny
		default:
			return "", xerr.New(xerr.EActionConflict, "v1 action_code must be 0(allow) or 1(deny)", map[string]any{
				"path": path, "value": *code, "allowed": []int{0, 1},
			})
		}
	}
	if hasText && hasCode && byText != byCode {
		return "", xerr.New(xerr.EActionConflict, "action text and v1 integer encoding disagree", map[string]any{
			"path": path, "text": text, "code": *code, "text_semantic": byText, "code_semantic": byCode,
		})
	}
	if hasText {
		return byText, nil
	}
	if hasCode {
		return byCode, nil
	}
	return "", xerr.New(xerr.EActionConflict, "action semantic cannot be determined from action or action_code", map[string]any{"path": path})
}

func v1EventFromDTO(d *v1Event, path string) (model.Event, error) {
	ev := model.Event{Seq: d.AtSeq, Order: d.Order}
	if d.Connection != nil {
		a := model.ConnectionAttempt{
			ID: d.Connection.AttemptID, Seq: d.Connection.AtSeq, Order: d.Connection.Order,
			SourceWorkloadID: d.Connection.SourceWorkloadID, TargetWorkloadID: d.Connection.TargetWorkloadID,
			Port: d.Connection.TargetPort, Protocol: defaultProtocol(d.Connection.Protocol),
		}
		if a.Seq == 0 {
			a.Seq = d.AtSeq
		}
		if a.Order == 0 {
			a.Order = d.Order
		}
		ev.Seq, ev.Order = a.Seq, a.Order
		ev.Attempt = &a
	}
	if d.Change != nil {
		c, err := v1ChangeFromDTO(d.Change, path+".policy_change")
		if err != nil {
			return ev, err
		}
		c.Seq = d.AtSeq
		c.Order = d.Order
		ev.Change = c
	}
	return ev, nil
}

func v1ChangeFromDTO(d *v1Change, path string) (*model.PolicyChange, error) {
	c := &model.PolicyChange{
		ID: d.ChangeID, Kind: model.ChangeKind(d.Kind), Namespace: d.Namespace, PolicyID: d.PolicyID,
		WorkloadID: d.WorkloadID, EditedFields: d.EditedFields, ReplacementRuleID: d.ReplacementRuleID,
	}
	if d.Before != nil {
		pol, err := v1PolicyFromDTO(d.Before, path+".before")
		if err != nil {
			return nil, err
		}
		c.Before = pol
	}
	if d.After != nil {
		pol, err := v1PolicyFromDTO(d.After, path+".after")
		if err != nil {
			return nil, err
		}
		c.After = pol
	}
	return c, nil
}

func decodeV2(doc *v2Doc) (*model.Project, error) {
	if doc.TickMS <= 0 {
		return nil, xerr.New(xerr.ETimeBaseInvalid, "v2 tickMs must be positive", map[string]any{"path": "$.tickMs", "value": doc.TickMS})
	}
	p := &model.Project{
		ID: doc.ProjectID, Seed: doc.Seed, TickMS: doc.TickMS,
		// V2 omits the marker; its version profile is default-deny.
		DefaultDeny: true, PolicyLimits: doc.PolicyLimits,
	}
	if doc.DefaultDeny != nil {
		p.DefaultDeny = *doc.DefaultDeny
	}
	for _, w := range doc.Workloads {
		p.Workloads = append(p.Workloads, model.Workload{
			ID: w.WorkloadID, Namespace: w.Namespace, ServiceAccount: w.ServiceAccount,
			Labels: w.Labels, DependsOn: w.DependsOn, Exited: w.Exited,
		})
	}
	for i := range doc.Policies {
		pol, err := v2PolicyFromDTO(&doc.Policies[i], doc.TickMS, fmt.Sprintf("$.policies[%d]", i))
		if err != nil {
			return nil, err
		}
		p.Policies = append(p.Policies, *pol)
	}
	for i := range doc.Timeline {
		ev, err := v2EventFromDTO(&doc.Timeline[i], doc.TickMS, fmt.Sprintf("$.timeline[%d]", i))
		if err != nil {
			return nil, err
		}
		p.Timeline = append(p.Timeline, ev)
	}
	return p, nil
}

func v2PolicyFromDTO(d *v2Policy, tickMS int64, path string) (*model.Policy, error) {
	action, err := resolveActionV2(d.ActionName, d.ActionCode, path+".actionCode")
	if err != nil {
		return nil, err
	}
	if d.PortRange == nil {
		return nil, xerr.New(xerr.EPortRangeInvalid, "v2 policy requires portRange", map[string]any{"path": path + ".portRange", "policy_id": d.PolicyID})
	}
	from, to, err := validatePortRange(d.PortRange.From, d.PortRange.To, path+".portRange")
	if err != nil {
		return nil, err
	}
	sel, err := selectorFromV2(d.SourceSelector, path+".sourceSelector")
	if err != nil {
		return nil, err
	}
	start, end := int64(1), int64(99)
	if d.EffectiveWindow != nil {
		start, err = msToSeq(d.EffectiveWindow.StartMS, tickMS, path+".effectiveWindow.startMs")
		if err != nil {
			return nil, err
		}
		end, err = msToSeq(d.EffectiveWindow.EndMS, tickMS, path+".effectiveWindow.endMs")
		if err != nil {
			return nil, err
		}
	}
	state := model.PolicyState(d.State)
	if state == "" {
		state = model.StateActive
	}
	return &model.Policy{
		ID: d.PolicyID, Namespace: d.Namespace, Source: sel, PortFrom: from, PortTo: to,
		Action: action, Priority: d.Priority, Order: d.DeclaredOrder, StartSeq: start, EndSeq: end,
		State: state, Revision: d.Revision, Replaces: d.Replaces, ReplacedBy: d.ReplacedBy,
	}, nil
}

func selectorFromV2(s *v2Selector, path string) (model.Selector, error) {
	if s == nil {
		return model.Selector{}, nil
	}
	if s.Kind != "" && s.Kind != "namespace_tree" {
		return model.Selector{}, xerr.New(xerr.ESelectorTreeInvalid, "unknown v2 selector kind", map[string]any{"path": path + ".kind", "value": s.Kind})
	}
	if len(s.NamespaceTree) != 1 {
		return model.Selector{}, xerr.New(xerr.ESelectorTreeInvalid, "namespaceTree must contain exactly one namespaced selector node", map[string]any{"path": path + ".namespaceTree", "nodes": len(s.NamespaceTree)})
	}
	for ns, node := range s.NamespaceTree {
		return model.Selector{
			Namespace: ns, MatchLabels: node.MatchLabels, ServiceAccounts: node.ServiceAccounts,
			WorkloadRefs: node.WorkloadRefs,
		}, nil
	}
	return model.Selector{}, nil
}

func resolveActionV2(text string, code *int, path string) (model.Action, error) {
	var byText model.Action
	hasText := strings.TrimSpace(text) != ""
	if hasText {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "allow", "allowed":
			byText = model.ActionAllow
		case "deny", "denied":
			byText = model.ActionDeny
		default:
			return "", xerr.New(xerr.EActionConflict, "unknown actionName", map[string]any{"path": path, "value": text})
		}
	}
	var byCode model.Action
	hasCode := code != nil
	if hasCode {
		switch *code {
		case 1:
			byCode = model.ActionAllow
		case 0:
			byCode = model.ActionDeny
		default:
			return "", xerr.New(xerr.EActionConflict, "v2 actionCode must be 1(allow) or 0(deny)", map[string]any{"path": path, "value": *code, "allowed": []int{0, 1}})
		}
	}
	if hasText && hasCode && byText != byCode {
		return "", xerr.New(xerr.EActionConflict, "actionName and swapped v2 actionCode disagree", map[string]any{
			"path": path, "text": text, "code": *code, "text_semantic": byText, "code_semantic": byCode,
		})
	}
	if hasText {
		return byText, nil
	}
	if hasCode {
		return byCode, nil
	}
	return "", xerr.New(xerr.EActionConflict, "v2 action semantic is missing", map[string]any{"path": path})
}

func v2EventFromDTO(d *v2Event, tickMS int64, path string) (model.Event, error) {
	seq, err := msToSeq(d.AtMS, tickMS, path+".atMs")
	if err != nil {
		return model.Event{}, err
	}
	ev := model.Event{Seq: seq, Order: d.Order}
	if d.Connection != nil {
		aSeq, err := msToSeq(d.Connection.AtMS, tickMS, path+".connection.atMs")
		if err != nil {
			return model.Event{}, err
		}
		a := model.ConnectionAttempt{
			ID: d.Connection.AttemptID, Seq: aSeq, Order: d.Connection.Order,
			SourceWorkloadID: d.Connection.SourceWorkloadID, TargetWorkloadID: d.Connection.TargetWorkloadID,
			Port: d.Connection.TargetPort, Protocol: defaultProtocol(d.Connection.Protocol),
		}
		if a.Order == 0 {
			a.Order = d.Order
		}
		ev.Seq, ev.Order = a.Seq, a.Order
		ev.Attempt = &a
	}
	if d.Change != nil {
		c := &model.PolicyChange{
			ID: d.Change.ChangeID, Kind: model.ChangeKind(d.Change.Kind), Namespace: d.Change.Namespace,
			PolicyID: d.Change.PolicyID, WorkloadID: d.Change.WorkloadID, EditedFields: d.Change.EditedFields,
			ReplacementRuleID: d.Change.ReplacementRuleID,
		}
		if d.Change.Before != nil {
			pol, err := v2PolicyFromDTO(d.Change.Before, tickMS, path+".change.before")
			if err != nil {
				return ev, err
			}
			c.Before = pol
		}
		if d.Change.After != nil {
			pol, err := v2PolicyFromDTO(d.Change.After, tickMS, path+".change.after")
			if err != nil {
				return ev, err
			}
			c.After = pol
		}
		c.Seq, c.Order = seq, d.Order
		ev.Change = c
	}
	return ev, nil
}

func decodeV3(doc *v3Doc) (*model.Project, error) {
	if doc.TickDurationMS <= 0 {
		doc.TickDurationMS = 100
	}
	p := &model.Project{
		ID: doc.ProjectName, Seed: doc.FixedSeed, TickMS: doc.TickDurationMS,
		DefaultDeny: true, PolicyLimits: doc.Quotas,
	}
	if doc.DefaultDeny != nil {
		p.DefaultDeny = *doc.DefaultDeny
	}
	selectors := map[string]v3SelectorEntry{}
	for _, s := range doc.SelectorTable {
		selectors[s.SelectorID] = s
	}
	for _, w := range doc.WorkloadSet {
		p.Workloads = append(p.Workloads, model.Workload{
			ID: w.Name, Namespace: w.Namespace, ServiceAccount: w.ServiceAccountName,
			Labels: w.LabelSet, DependsOn: w.DependencyRefs, Exited: w.Removed,
		})
	}
	for i := range doc.PolicyEntries {
		d := &doc.PolicyEntries[i]
		path := fmt.Sprintf("$.policyEntries[%d]", i)
		selEntry, ok := selectors[d.SelectorRef]
		if !ok {
			return nil, xerr.New(xerr.ESelectorTreeInvalid, "policy references missing selector table entry", map[string]any{
				"path": path + ".selectorRef", "policy_id": d.EntryID, "selector_ref": d.SelectorRef,
			})
		}
		from, to, err := parsePortExpression(d.PortExpression, path+".portExpression", d.EntryID)
		if err != nil {
			return nil, err
		}
		action, err := resolveActionV3(d.Effect, d.EffectCode, path+".effect")
		if err != nil {
			return nil, err
		}
		start, end := int64(1), int64(99)
		if len(d.TickWindow) == 2 {
			start, end = d.TickWindow[0], d.TickWindow[1]
		}
		state := model.PolicyState(d.LifecycleState)
		if state == "" {
			state = model.StateActive
		}
		p.Policies = append(p.Policies, model.Policy{
			ID: d.EntryID, Namespace: d.NamespaceName,
			Source: model.Selector{
				Namespace: selEntry.Namespace, MatchLabels: selEntry.MatchLabels,
				ServiceAccounts: selEntry.ServiceAccounts, WorkloadRefs: selEntry.WorkloadRefs,
			},
			PortFrom: from, PortTo: to, Action: action, Priority: d.PriorityValue, Order: d.DeclarationIndex,
			StartSeq: start, EndSeq: end, State: state, Revision: d.Revision,
			Replaces: d.Replaces, ReplacedBy: d.ReplacedBy,
		})
	}
	for i := range doc.Moments {
		m := &doc.Moments[i]
		ev := model.Event{Seq: m.AtTick, Order: m.SequenceOrder}
		if m.Connection != nil {
			a := model.ConnectionAttempt{
				ID: m.Connection.ConnectionID, Seq: m.Connection.AtTick, Order: m.Connection.SequenceOrder,
				SourceWorkloadID: m.Connection.SourceRef, TargetWorkloadID: m.Connection.TargetRef,
				Port: m.Connection.ServicePort, Protocol: defaultProtocol(m.Connection.ProtocolName),
			}
			if a.Seq == 0 {
				a.Seq = m.AtTick
			}
			ev.Seq, ev.Order = a.Seq, a.Order
			ev.Attempt = &a
		}
		if m.Edit != nil {
			c := &model.PolicyChange{
				ID: m.Edit.EditID, Seq: m.AtTick, Order: m.SequenceOrder,
				Kind: model.ChangeKind(m.Edit.Kind), Namespace: m.Edit.NamespaceName,
				PolicyID: m.Edit.EntryRef, WorkloadID: m.Edit.WorkloadRef,
				EditedFields: m.Edit.ChangedFields, ReplacementRuleID: m.Edit.ReplacementRuleID,
			}
			if m.Edit.Before != nil {
				pol, err := v3PolicyFromDTO(m.Edit.Before, selectors, fmt.Sprintf("$.moments[%d].edit.before", i))
				if err != nil {
					return nil, err
				}
				c.Before = pol
			}
			if m.Edit.After != nil {
				pol, err := v3PolicyFromDTO(m.Edit.After, selectors, fmt.Sprintf("$.moments[%d].edit.after", i))
				if err != nil {
					return nil, err
				}
				c.After = pol
			}
			ev.Change = c
		}
		p.Timeline = append(p.Timeline, ev)
	}
	return p, nil
}

func v3PolicyFromDTO(d *v3Policy, selectors map[string]v3SelectorEntry, path string) (*model.Policy, error) {
	selEntry, ok := selectors[d.SelectorRef]
	if !ok {
		return nil, xerr.New(xerr.ESelectorTreeInvalid, "embedded v3 policy references missing selector", map[string]any{"path": path + ".selectorRef", "ref": d.SelectorRef})
	}
	from, to, err := parsePortExpression(d.PortExpression, path+".portExpression", d.EntryID)
	if err != nil {
		return nil, err
	}
	action, err := resolveActionV3(d.Effect, d.EffectCode, path+".effect")
	if err != nil {
		return nil, err
	}
	start, end := int64(1), int64(99)
	if len(d.TickWindow) == 2 {
		start, end = d.TickWindow[0], d.TickWindow[1]
	}
	return &model.Policy{
		ID: d.EntryID, Namespace: d.NamespaceName,
		Source:   model.Selector{Namespace: selEntry.Namespace, MatchLabels: selEntry.MatchLabels, ServiceAccounts: selEntry.ServiceAccounts, WorkloadRefs: selEntry.WorkloadRefs},
		PortFrom: from, PortTo: to, Action: action, Priority: d.PriorityValue, Order: d.DeclarationIndex,
		StartSeq: start, EndSeq: end, State: model.PolicyState(d.LifecycleState), Revision: d.Revision,
	}, nil
}

func resolveActionV3(text string, code *int, path string) (model.Action, error) {
	if code != nil {
		switch *code {
		case 1:
			return model.ActionAllow, nil
		case 2:
			return model.ActionDeny, nil
		default:
			return "", xerr.New(xerr.EActionConflict, "v3 effectCode must be 1(allow) or 2(deny)", map[string]any{"path": path + ".effectCode", "value": *code})
		}
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "allow", "allowed":
		return model.ActionAllow, nil
	case "deny", "denied":
		return model.ActionDeny, nil
	case "":
		return "", xerr.New(xerr.EActionConflict, "v3 effect is missing", map[string]any{"path": path})
	default:
		return "", xerr.New(xerr.EActionConflict, "unknown v3 effect", map[string]any{"path": path, "value": text})
	}
}

func parsePortExpression(expr, path, policyID string) (int, int, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return 0, 0, xerr.New(xerr.EPortRangeInvalid, "port expression is empty", map[string]any{"path": path, "policy_id": policyID})
	}
	parts := strings.Split(expr, "-")
	if len(parts) == 1 {
		port, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, 0, xerr.Wrap(xerr.EPortRangeInvalid, "port expression is not an integer", err, map[string]any{"path": path, "value": expr})
		}
		return validatePortRange(port, port, path)
	}
	if len(parts) == 2 {
		from, err1 := strconv.Atoi(parts[0])
		to, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return 0, 0, xerr.New(xerr.EPortRangeInvalid, "port range endpoints must be integers", map[string]any{"path": path, "value": expr})
		}
		return validatePortRange(from, to, path)
	}
	return 0, 0, xerr.New(xerr.EPortRangeInvalid, "port expression must be PORT or FROM-TO", map[string]any{"path": path, "value": expr})
}

func validatePortRange(from, to int, path string) (int, int, error) {
	if from < 1 || from > 65535 || to < 1 || to > 65535 || from > to {
		return 0, 0, xerr.New(xerr.EPortRangeInvalid, "port range must satisfy 1 <= from <= to <= 65535", map[string]any{
			"path": path, "from": from, "to": to,
		})
	}
	return from, to, nil
}

func msToSeq(ms, tickMS int64, path string) (int64, error) {
	if tickMS <= 0 {
		return 0, xerr.New(xerr.ETimeBaseInvalid, "tickMs must be positive", map[string]any{"path": path})
	}
	if ms < 0 || ms%tickMS != 0 {
		return 0, xerr.New(xerr.ETimeBaseInvalid, "millisecond timestamp is not aligned to logical tick base", map[string]any{
			"path": path, "ms": ms, "tick_ms": tickMS,
		})
	}
	return ms / tickMS, nil
}

func defaultProtocol(p string) string {
	if strings.TrimSpace(p) == "" {
		return "TCP"
	}
	return strings.ToUpper(p)
}

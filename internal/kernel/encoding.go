package kernel

import "sort"

func EncodeSchema(p *Project, version SchemaVersion) (map[string]any, error) {
	cp := p.Clone()
	switch version {
	case SchemaV1:
		return encodeV1(cp), nil
	case SchemaV2:
		return encodeV2(cp), nil
	case SchemaV3:
		return encodeV3(cp), nil
	default:
		return nil, NewBizError(CodeSchemaVersion, "unsupported schema %q", version)
	}
}

func kiBOrPanic(bytes int64) int64 {
	if bytes%1024 != 0 {
		panic(NewBizError(CodeOverflow, "value %d bytes is not integral KiB", bytes))
	}
	return bytes / 1024
}

func sortedRacks(nodes []Node) []string {
	set := map[string]bool{}
	for _, n := range nodes {
		set[n.Rack] = true
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func encodeV1(p *Project) map[string]any {
	nodes := make([]any, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		nodes = append(nodes, map[string]any{
			"node_id":         string(n.ID),
			"rack":            n.Rack,
			"capacity_bytes":  n.CapacityBytes,
			"allowed_tenants": idsToStrings(n.AllowedTenants),
		})
	}
	tenants := make([]any, 0, len(p.Tenants))
	for _, t := range p.Tenants {
		tenants = append(tenants, map[string]any{
			"tenant_id":         string(t.ID),
			"priority":          t.Priority,
			"weight_permille":   t.WeightPermyriad / 10,
			"quota_bytes":       t.QuotaBytes,
			"guarantee_bytes":   t.GuaranteeBytes,
			"burst_limit_bytes": t.BurstLimitBytes,
			"guarantee_node_id": string(t.GuaranteeNodeID),
		})
	}
	events := make([]any, 0, len(p.Events))
	for _, e := range p.Events {
		m := map[string]any{
			"event_id":  e.ID,
			"tick":      e.Tick,
			"op":        e.Type,
			"budget_ms": e.BudgetMS,
		}
		if e.TenantID != "" {
			m["tenant_id"] = string(e.TenantID)
		}
		if e.NodeID != "" {
			m["node_id"] = string(e.NodeID)
		}
		if e.AmountBytes != 0 {
			m["amount_bytes"] = e.AmountBytes
		}
		if e.Type == EventTypeWeight {
			m["weight_permille"] = e.WeightPermyriad / 10
		}
		if len(e.Refs) > 0 {
			m["causal_refs"] = e.Refs
		}
		events = append(events, m)
	}
	return map[string]any{
		"schema":       "quotakernel.project/v1",
		"project_id":   p.ProjectID,
		"seed":         p.Seed,
		"tick_ms":      p.TickMS,
		"fixed_policy": policyV1(p.Policy),
		"nodes":        nodes,
		"tenants":      tenants,
		"events":       events,
	}
}

func policyV1(p FixedPolicy) map[string]any {
	return map[string]any{
		"bit_width":       p.BitWidth,
		"fraction_bits":   p.FractionBits,
		"tolerance_bytes": p.ToleranceBytes,
		"tolerance_ppm":   p.TolerancePPM,
	}
}

func encodeV2(p *Project) map[string]any {
	total := int64(0)
	for _, n := range p.Nodes {
		total += n.CapacityBytes
	}
	racks := make([]any, 0)
	for _, rack := range sortedRacks(p.Nodes) {
		rackNodes := make([]any, 0)
		for _, n := range p.Nodes {
			if n.Rack != rack {
				continue
			}
			rackNodes = append(rackNodes, map[string]any{
				"nodeId":      string(n.ID),
				"capacityKiB": kiBOrPanic(n.CapacityBytes),
				"tenantIds":   idsToStrings(n.AllowedTenants),
			})
		}
		racks = append(racks, map[string]any{"rackId": rack, "nodes": rackNodes})
	}
	tenants := make([]any, 0, len(p.Tenants))
	for _, t := range p.Tenants {
		quotaPermille := t.QuotaBytes * 1000 / total
		guaranteePermille := t.GuaranteeBytes * 1000 / total
		tenants = append(tenants, map[string]any{
			"tenantId":          string(t.ID),
			"priority":          t.Priority,
			"weightPermille":    t.WeightPermyriad / 10,
			"quotaPermille":     quotaPermille,
			"guaranteePermille": guaranteePermille,
			"guaranteeNodeId":   string(t.GuaranteeNodeID),
		})
	}
	events := make([]any, 0, len(p.Events))
	for _, e := range p.Events {
		op := e.Type
		switch e.Type {
		case EventTypeApply:
			op = "reserve"
		case EventTypeRelease:
			op = "free"
		case EventTypeWeight:
			op = "reweight"
		}
		m := map[string]any{
			"eventId":          e.ID,
			"atMs":             e.Tick * p.TickMS,
			"op":               op,
			"decisionBudgetMs": e.BudgetMS,
		}
		if e.TenantID != "" {
			m["tenantId"] = string(e.TenantID)
		}
		if e.NodeID != "" {
			m["nodeId"] = string(e.NodeID)
		}
		if e.AmountBytes != 0 {
			m["amountKiB"] = kiBOrPanic(e.AmountBytes)
		}
		if e.Type == EventTypeWeight {
			m["weightPermille"] = e.WeightPermyriad / 10
		}
		if len(e.Refs) > 0 {
			m["causalRefs"] = e.Refs
		}
		events = append(events, m)
	}
	return map[string]any{
		"schema":                    "quotakernel.project/v2",
		"projectId":                 p.ProjectID,
		"seed":                      p.Seed,
		"tickDurationMs":            p.TickMS,
		"defaultBurstRatioPermille": 250,
		"fixedPolicy": map[string]any{
			"bitWidth":       p.Policy.BitWidth,
			"fractionBits":   p.Policy.FractionBits,
			"toleranceBytes": p.Policy.ToleranceBytes,
			"tolerancePPM":   p.Policy.TolerancePPM,
		},
		"topology":   map[string]any{"racks": racks},
		"tenantPool": tenants,
		"eventLog":   events,
	}
}

func encodeV3(p *Project) map[string]any {
	total := int64(0)
	for _, n := range p.Nodes {
		total += n.CapacityBytes
	}
	racks := make([]any, 0)
	for _, rack := range sortedRacks(p.Nodes) {
		nodes := make([]any, 0)
		for _, n := range p.Nodes {
			if n.Rack != rack {
				continue
			}
			nodes = append(nodes, map[string]any{
				"node-id":      string(n.ID),
				"capacity-kib": kiBOrPanic(n.CapacityBytes),
				"host-tenants": idsToStrings(n.AllowedTenants),
			})
		}
		racks = append(racks, map[string]any{"rack-name": rack, "storage-nodes": nodes})
	}
	tenants := make([]any, 0, len(p.Tenants))
	for _, t := range p.Tenants {
		quotaBP := t.QuotaBytes * 10000 / total
		guaranteeBP := t.GuaranteeBytes * 10000 / total
		burstBP := t.BurstLimitBytes * 10000 / total
		tenants = append(tenants, map[string]any{
			"tenant-id":         string(t.ID),
			"sla-priority":      t.Priority,
			"weight-bp":         t.WeightPermyriad,
			"quota-bp":          quotaBP,
			"guarantee-bp":      guaranteeBP,
			"burst-cap-bp":      burstBP,
			"guarantee-node-id": string(t.GuaranteeNodeID),
		})
	}
	events := make([]any, 0, len(p.Events))
	for _, e := range p.Events {
		kind := e.Type
		switch e.Type {
		case EventTypeApply:
			kind = "capacity.apply"
		case EventTypeRelease:
			kind = "capacity.release"
		case EventTypeWeight:
			kind = "weight.set"
		}
		m := map[string]any{
			"event-id":     e.ID,
			"timestamp-ms": e.Tick * p.TickMS,
			"kind":         kind,
			"budget-ms":    e.BudgetMS,
		}
		if e.TenantID != "" {
			m["tenant-id"] = string(e.TenantID)
		}
		if e.NodeID != "" {
			m["node-id"] = string(e.NodeID)
		}
		if e.AmountBytes != 0 {
			m["amount-kib"] = kiBOrPanic(e.AmountBytes)
		}
		if e.Type == EventTypeWeight {
			m["weight-bp"] = e.WeightPermyriad
		}
		if len(e.Refs) > 0 {
			m["causal-refs"] = e.Refs
		}
		events = append(events, m)
	}
	return map[string]any{
		"schema":            "quotakernel.project/v3",
		"project-id":        p.ProjectID,
		"seed":              p.Seed,
		"tick-milliseconds": p.TickMS,
		"fixed-policy": map[string]any{
			"bit-width":       p.Policy.BitWidth,
			"fraction-bits":   p.Policy.FractionBits,
			"tolerance-bytes": p.Policy.ToleranceBytes,
			"tolerance-ppm":   p.Policy.TolerancePPM,
		},
		"topology": map[string]any{"rack-tree": racks},
		"tenants":  tenants,
		"events":   events,
	}
}

func idsToStrings(ids []ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

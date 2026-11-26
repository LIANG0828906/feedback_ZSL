package kernel

import (
	"encoding/json"
	"fmt"
)

func loadV2(top rawObject) (*Project, error) {
	const path = "$"
	allowed := []string{"schema", "projectId", "seed", "tickDurationMs", "fixedPolicy", "defaultBurstRatioPermille", "topology", "tenantPool", "eventLog"}
	if err := top.checkAllowed(path, allowed...); err != nil {
		return nil, err
	}
	pid, err := reqString(top, "projectId", path)
	if err != nil {
		return nil, err
	}
	seed, err := optInt(top, "seed", path, 42)
	if err != nil {
		return nil, err
	}
	tickMS, err := reqInt(top, "tickDurationMs", path)
	if err != nil {
		return nil, err
	}
	defaultBurstRatio, err := optInt(top, "defaultBurstRatioPermille", path, 0)
	if err != nil {
		return nil, err
	}
	if defaultBurstRatio < 0 || defaultBurstRatio > 1000 {
		return nil, NewBizError(CodeBurstPercentRange, "default burst ratio permille must be [0,1000], got %d", defaultBurstRatio).
			WithPath("$.defaultBurstRatioPermille")
	}
	policy, err := parsePolicy(top["fixedPolicy"], "$.fixedPolicy", true, false)
	if err != nil {
		return nil, err
	}
	p := &Project{SchemaVersion: SchemaCanonical, ProjectID: pid, Seed: seed, TickMS: tickMS, Policy: policy}

	topoRaw, ok := top["topology"]
	if !ok {
		return nil, NewBizError(CodeUnknownField, "missing topology").WithPath("$.topology")
	}
	topo, err := asObject(topoRaw, "$.topology")
	if err != nil {
		return nil, err
	}
	if err := topo.checkAllowed("$.topology", "racks"); err != nil {
		return nil, err
	}
	racks, err := reqList(topo, "racks", "$.topology")
	if err != nil {
		return nil, err
	}
	for ri, rackRaw := range racks {
		rp := fmt.Sprintf("$.topology.racks[%d]", ri)
		rack, err := asObject(rackRaw, rp)
		if err != nil {
			return nil, err
		}
		if err := rack.checkAllowed(rp, "rackId", "nodes"); err != nil {
			return nil, err
		}
		rackID, err := reqString(rack, "rackId", rp)
		if err != nil {
			return nil, err
		}
		nodeRaws, err := reqList(rack, "nodes", rp)
		if err != nil {
			return nil, err
		}
		for ni, raw := range nodeRaws {
			np := fmt.Sprintf("%s.nodes[%d]", rp, ni)
			obj, err := asObject(raw, np)
			if err != nil {
				return nil, err
			}
			if err := obj.checkAllowed(np, "nodeId", "capacityKiB", "tenantIds"); err != nil {
				return nil, err
			}
			id, err := reqString(obj, "nodeId", np)
			if err != nil {
				return nil, err
			}
			kib, err := reqInt(obj, "capacityKiB", np)
			if err != nil {
				return nil, err
			}
			capacity, err := KiBIntegerToBytes(kib)
			if err != nil {
				return nil, err.(*BizError).WithPath(np + ".capacityKiB").WithNode(ID(id))
			}
			hosts, err := optStringList(obj, "tenantIds", np)
			if err != nil {
				return nil, err
			}
			p.Nodes = append(p.Nodes, Node{ID: ID(id), Rack: rackID, CapacityBytes: capacity, AllowedTenants: idsFromStrings(hosts)})
		}
	}

	totalCapacity := int64(0)
	for _, n := range p.Nodes {
		totalCapacity += n.CapacityBytes
	}

	tenantRaws, err := reqList(top, "tenantPool", path)
	if err != nil {
		return nil, err
	}
	for i, raw := range tenantRaws {
		tp := fmt.Sprintf("$.tenantPool[%d]", i)
		obj, err := asObject(raw, tp)
		if err != nil {
			return nil, err
		}
		allowedFields := []string{"tenantId", "priority", "weightPermille", "quotaPermille", "guaranteePermille", "guaranteeNodeId", "burstLimitKiB", "burstPermille"}
		if err := obj.checkAllowed(tp, allowedFields...); err != nil {
			return nil, err
		}
		id, err := reqString(obj, "tenantId", tp)
		if err != nil {
			return nil, err
		}
		priority, err := optInt(obj, "priority", tp, 0)
		if err != nil {
			return nil, err
		}
		weightPermille, err := optInt(obj, "weightPermille", tp, 0)
		if err != nil {
			return nil, err
		}
		weightBP, err := PermilleToPermyriad(weightPermille)
		if err != nil {
			return nil, err.(*BizError).WithPath(tp + ".weightPermille").WithTenant(ID(id))
		}
		quotaPermille, err := reqInt(obj, "quotaPermille", tp)
		if err != nil {
			return nil, err
		}
		if err := checkScaleRange(quotaPermille, 1000, tp+".quotaPermille", ID(id), "quota"); err != nil {
			return nil, err
		}
		guaranteePermille, err := reqInt(obj, "guaranteePermille", tp)
		if err != nil {
			return nil, err
		}
		if err := checkScaleRange(guaranteePermille, 1000, tp+".guaranteePermille", ID(id), "guarantee"); err != nil {
			return nil, err
		}
		quota, err := PermilleOf(totalCapacity, quotaPermille)
		if err != nil {
			return nil, err.(*BizError).WithPath(tp + ".quotaPermille").WithTenant(ID(id))
		}
		guarantee, err := PermilleOf(totalCapacity, guaranteePermille)
		if err != nil {
			return nil, err.(*BizError).WithPath(tp + ".guaranteePermille").WithTenant(ID(id))
		}
		burst := int64(0)
		_, hasKiB := obj["burstLimitKiB"]
		_, hasPermille := obj["burstPermille"]
		if hasKiB && hasPermille {
			return nil, NewBizError(CodeFieldConflict, "burstLimitKiB and burstPermille are mutually exclusive").
				WithPath(tp).WithTenant(ID(id)).
				WithDetail("fields", []string{"burstLimitKiB", "burstPermille"})
		}
		if rawBurstKiB, hasKiB := obj["burstLimitKiB"]; hasKiB {
			kib, err := decodeInt(rawBurstKiB, tp+".burstLimitKiB")
			if err != nil {
				return nil, err
			}
			burst, err = KiBIntegerToBytes(kib)
			if err != nil {
				return nil, err.(*BizError).WithPath(tp + ".burstLimitKiB").WithTenant(ID(id))
			}
		} else if rawBurstPermille, hasPermille := obj["burstPermille"]; hasPermille {
			permille, err := decodeInt(rawBurstPermille, tp+".burstPermille")
			if err != nil {
				return nil, err
			}
			if err := checkScaleRange(permille, 1000, tp+".burstPermille", ID(id), "burst"); err != nil {
				return nil, err
			}
			burst, err = PermilleOf(totalCapacity, permille)
			if err != nil {
				return nil, err.(*BizError).WithPath(tp + ".burstPermille").WithTenant(ID(id))
			}
		} else {
			burst = quota * defaultBurstRatio / 1000
		}
		gnode, err := optString(obj, "guaranteeNodeId", tp, "")
		if err != nil {
			return nil, err
		}
		p.Tenants = append(p.Tenants, Tenant{ID: ID(id), Priority: int(priority), WeightPermyriad: weightBP, QuotaBytes: quota, GuaranteeBytes: guarantee, BurstLimitBytes: burst, GuaranteeNodeID: ID(gnode)})
	}

	eventRaws, err := reqList(top, "eventLog", path)
	if err != nil {
		return nil, err
	}
	for i, raw := range eventRaws {
		ep := fmt.Sprintf("$.eventLog[%d]", i)
		e, err := loadV2Event(raw, ep, tickMS)
		if err != nil {
			return nil, err
		}
		p.Events = append(p.Events, e)
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2Event(raw json.RawMessage, ep string, tickMS int64) (Event, error) {
	obj, err := asObject(raw, ep)
	if err != nil {
		return Event{}, err
	}
	allowed := []string{"eventId", "atMs", "op", "tenantId", "nodeId", "amountKiB", "decisionBudgetMs", "weightPermille", "causalRefs"}
	if err := obj.checkAllowed(ep, allowed...); err != nil {
		return Event{}, err
	}
	id, err := reqString(obj, "eventId", ep)
	if err != nil {
		return Event{}, err
	}
	atMS, err := reqInt(obj, "atMs", ep)
	if err != nil {
		return Event{}, err
	}
	tick, aligned, err := MillisecondsToTick(atMS, tickMS)
	if err != nil {
		return Event{}, annotateEvent(err, ep+".atMs", id)
	}
	if !aligned {
		return Event{}, NewBizError(CodeTimeBasis, "timestamp %dms is not aligned to %dms ticks", atMS, tickMS).
			WithPath(ep + ".atMs").WithEvent(id)
	}
	op, err := reqString(obj, "op", ep)
	if err != nil {
		return Event{}, err
	}
	tenant, err := optString(obj, "tenantId", ep, "")
	if err != nil {
		return Event{}, err
	}
	node, err := optString(obj, "nodeId", ep, "")
	if err != nil {
		return Event{}, err
	}
	budget, err := optInt(obj, "decisionBudgetMs", ep, 0)
	if err != nil {
		return Event{}, err
	}
	kib, err := optInt(obj, "amountKiB", ep, 0)
	if err != nil {
		return Event{}, err
	}
	amount, err := KiBIntegerToBytes(kib)
	if err != nil {
		return Event{}, err.(*BizError).WithPath(ep + ".amountKiB").WithEvent(id)
	}
	weightPermille, err := optInt(obj, "weightPermille", ep, 0)
	if err != nil {
		return Event{}, err
	}
	weightBP, err := PermilleToPermyriad(weightPermille)
	if err != nil {
		return Event{}, err.(*BizError).WithPath(ep + ".weightPermille").WithEvent(id)
	}
	refs, err := optStringList(obj, "causalRefs", ep)
	if err != nil {
		return Event{}, err
	}
	return Event{ID: id, Tick: tick, Type: normalizeOp(op), TenantID: ID(tenant), NodeID: ID(node), AmountBytes: amount, BudgetMS: budget, WeightPermyriad: weightBP, Refs: refs}, nil
}

func checkScaleRange(value, max int64, path string, tenant ID, field string) error {
	if value < 0 || value > max {
		return NewBizError(CodeBurstPercentRange, "%s scale must be [0,%d], got %d", field, max, value).
			WithPath(path).WithTenant(tenant).
			WithDetail("field", field).WithDetail("max_scale", max).WithDetail("value", value)
	}
	return nil
}

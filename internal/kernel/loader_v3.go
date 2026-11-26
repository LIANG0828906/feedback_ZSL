package kernel

import (
	"encoding/json"
	"fmt"
)

func loadV3(top rawObject) (*Project, error) {
	const path = "$"
	allowed := []string{"schema", "project-id", "seed", "tick-milliseconds", "fixed-policy", "topology", "tenants", "events"}
	if err := top.checkAllowed(path, allowed...); err != nil {
		return nil, err
	}
	pid, err := reqString(top, "project-id", path)
	if err != nil {
		return nil, err
	}
	seed, err := optInt(top, "seed", path, 42)
	if err != nil {
		return nil, err
	}
	tickMS, err := reqInt(top, "tick-milliseconds", path)
	if err != nil {
		return nil, err
	}
	policy, err := parsePolicy(top["fixed-policy"], "$.fixed-policy", false, true)
	if err != nil {
		return nil, err
	}
	p := &Project{SchemaVersion: SchemaCanonical, ProjectID: pid, Seed: seed, TickMS: tickMS, Policy: policy}

	topo, err := asObject(top["topology"], "$.topology")
	if err != nil {
		return nil, err
	}
	if err := topo.checkAllowed("$.topology", "rack-tree"); err != nil {
		return nil, err
	}
	racks, err := reqList(topo, "rack-tree", "$.topology")
	if err != nil {
		return nil, err
	}
	for ri, rackRaw := range racks {
		rp := fmt.Sprintf("$.topology.rack-tree[%d]", ri)
		rack, err := asObject(rackRaw, rp)
		if err != nil {
			return nil, err
		}
		if err := rack.checkAllowed(rp, "rack-name", "storage-nodes"); err != nil {
			return nil, err
		}
		rackName, err := reqString(rack, "rack-name", rp)
		if err != nil {
			return nil, err
		}
		nodeRaws, err := reqList(rack, "storage-nodes", rp)
		if err != nil {
			return nil, err
		}
		for ni, raw := range nodeRaws {
			np := fmt.Sprintf("%s.storage-nodes[%d]", rp, ni)
			obj, err := asObject(raw, np)
			if err != nil {
				return nil, err
			}
			if err := obj.checkAllowed(np, "node-id", "capacity-kib", "host-tenants"); err != nil {
				return nil, err
			}
			id, err := reqString(obj, "node-id", np)
			if err != nil {
				return nil, err
			}
			kib, err := reqInt(obj, "capacity-kib", np)
			if err != nil {
				return nil, err
			}
			capacity, err := KiBIntegerToBytes(kib)
			if err != nil {
				return nil, err.(*BizError).WithPath(np + ".capacity-kib").WithNode(ID(id))
			}
			hosts, err := optStringList(obj, "host-tenants", np)
			if err != nil {
				return nil, err
			}
			p.Nodes = append(p.Nodes, Node{ID: ID(id), Rack: rackName, CapacityBytes: capacity, AllowedTenants: idsFromStrings(hosts)})
		}
	}

	total := int64(0)
	for _, n := range p.Nodes {
		total += n.CapacityBytes
	}

	tenantRaws, err := reqList(top, "tenants", path)
	if err != nil {
		return nil, err
	}
	for i, raw := range tenantRaws {
		tp := fmt.Sprintf("$.tenants[%d]", i)
		obj, err := asObject(raw, tp)
		if err != nil {
			return nil, err
		}
		fields := []string{"tenant-id", "sla-priority", "priority", "weight-bp", "quota-bp", "guarantee-bp", "burst-cap-bp", "guarantee-node-id"}
		if err := obj.checkAllowed(tp, fields...); err != nil {
			return nil, err
		}
		id, err := reqString(obj, "tenant-id", tp)
		if err != nil {
			return nil, err
		}
		priority, err := optInt(obj, "sla-priority", tp, 0)
		if err != nil {
			return nil, err
		}
		_, hasSLAPriority := obj["sla-priority"]
		_, hasPriority := obj["priority"]
		if hasSLAPriority && hasPriority {
			return nil, NewBizError(CodeFieldConflict, "sla-priority and priority are mutually exclusive").
				WithPath(tp).WithTenant(ID(id)).
				WithDetail("fields", []string{"sla-priority", "priority"})
		}
		if hasPriority {
			alt, err := reqInt(obj, "priority", tp)
			if err != nil {
				return nil, err
			}
			priority = alt
		}
		weightBP, err := optInt(obj, "weight-bp", tp, 0)
		if err != nil {
			return nil, err
		}
		if err := checkScaleRange(weightBP, 10000, tp+".weight-bp", ID(id), "weight"); err != nil {
			return nil, err
		}
		quotaBP, err := reqInt(obj, "quota-bp", tp)
		if err != nil {
			return nil, err
		}
		if err := checkScaleRange(quotaBP, 10000, tp+".quota-bp", ID(id), "quota"); err != nil {
			return nil, err
		}
		guaranteeBP, err := reqInt(obj, "guarantee-bp", tp)
		if err != nil {
			return nil, err
		}
		if err := checkScaleRange(guaranteeBP, 10000, tp+".guarantee-bp", ID(id), "guarantee"); err != nil {
			return nil, err
		}
		burstBP, err := optInt(obj, "burst-cap-bp", tp, 0)
		if err != nil {
			return nil, err
		}
		if err := checkScaleRange(burstBP, 10000, tp+".burst-cap-bp", ID(id), "burst"); err != nil {
			return nil, err
		}
		quota, err := PermyriadOf(total, quotaBP)
		if err != nil {
			return nil, annotateTenant(err, tp+".quota-bp", ID(id))
		}
		guarantee, err := PermyriadOf(total, guaranteeBP)
		if err != nil {
			return nil, annotateTenant(err, tp+".guarantee-bp", ID(id))
		}
		burst, err := PermyriadOf(total, burstBP)
		if err != nil {
			return nil, annotateTenant(err, tp+".burst-cap-bp", ID(id))
		}
		gnode, err := optString(obj, "guarantee-node-id", tp, "")
		if err != nil {
			return nil, err
		}
		p.Tenants = append(p.Tenants, Tenant{ID: ID(id), Priority: int(priority), WeightPermyriad: weightBP, QuotaBytes: quota, GuaranteeBytes: guarantee, BurstLimitBytes: burst, GuaranteeNodeID: ID(gnode)})
	}

	eventRaws, err := reqList(top, "events", path)
	if err != nil {
		return nil, err
	}
	for i, raw := range eventRaws {
		ep := fmt.Sprintf("$.events[%d]", i)
		e, err := loadV3Event(raw, ep, tickMS)
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

func loadV3Event(raw json.RawMessage, ep string, tickMS int64) (Event, error) {
	obj, err := asObject(raw, ep)
	if err != nil {
		return Event{}, err
	}
	allowed := []string{"event-id", "timestamp-ms", "kind", "tenant-id", "node-id", "amount-kib", "budget-ms", "weight-bp", "causal-refs"}
	if err := obj.checkAllowed(ep, allowed...); err != nil {
		return Event{}, err
	}
	id, err := reqString(obj, "event-id", ep)
	if err != nil {
		return Event{}, err
	}
	atMS, err := reqInt(obj, "timestamp-ms", ep)
	if err != nil {
		return Event{}, err
	}
	tick, aligned, err := MillisecondsToTick(atMS, tickMS)
	if err != nil {
		return Event{}, annotateEvent(err, ep+".timestamp-ms", id)
	}
	if !aligned {
		return Event{}, NewBizError(CodeTimeBasis, "timestamp %dms is not aligned to %dms ticks", atMS, tickMS).
			WithPath(ep + ".timestamp-ms").WithEvent(id)
	}
	kind, err := reqString(obj, "kind", ep)
	if err != nil {
		return Event{}, err
	}
	tenant, err := optString(obj, "tenant-id", ep, "")
	if err != nil {
		return Event{}, err
	}
	node, err := optString(obj, "node-id", ep, "")
	if err != nil {
		return Event{}, err
	}
	budget, err := optInt(obj, "budget-ms", ep, 0)
	if err != nil {
		return Event{}, err
	}
	kib, err := optInt(obj, "amount-kib", ep, 0)
	if err != nil {
		return Event{}, err
	}
	amount, err := KiBIntegerToBytes(kib)
	if err != nil {
		return Event{}, err.(*BizError).WithPath(ep + ".amount-kib").WithEvent(id)
	}
	weightBP, err := optInt(obj, "weight-bp", ep, 0)
	if err != nil {
		return Event{}, err
	}
	refs, err := optStringList(obj, "causal-refs", ep)
	if err != nil {
		return Event{}, err
	}
	return Event{ID: id, Tick: tick, Type: normalizeOp(kind), TenantID: ID(tenant), NodeID: ID(node), AmountBytes: amount, BudgetMS: budget, WeightPermyriad: weightBP, Refs: refs}, nil
}

func annotateTenant(err error, path string, tenant ID) error {
	if be, ok := AsBizError(err); ok {
		return be.WithPath(path).WithTenant(tenant)
	}
	return err
}

func annotateEvent(err error, path, event string) error {
	if be, ok := AsBizError(err); ok {
		return be.WithPath(path).WithEvent(event)
	}
	return err
}

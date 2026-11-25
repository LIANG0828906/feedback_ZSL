package thermalcache

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Fixture contains one canonical project and the same project encoded as the
// three declared schemas. Every byte is derived from the numeric seed.
type Fixture struct {
	Seed      int64             `json:"seed"`
	Canonical *Project          `json:"canonical"`
	Documents map[string][]byte `json:"documents"`
}

func GenerateFixture(seed int64) (*Fixture, error) {
	p, err := scenarioProject(seed)
	if err != nil {
		return nil, err
	}
	canonical := p.Clone()
	canonical.SchemaVersion = SchemaV2
	if err := canonical.Validate(); err != nil {
		return nil, err
	}
	canonicalize(canonical)
	docs := make(map[string][]byte, 3)
	for version, encode := range map[string]func(*Project) (any, error){
		SchemaV1: encodeV1Document,
		SchemaV2: encodeV2Document,
		SchemaV3: encodeV3Document,
	} {
		doc, err := encode(canonical)
		if err != nil {
			return nil, err
		}
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return nil, biz(CodeInternal, "cannot encode generated fixture", map[string]any{"version": version, "error": err.Error()})
		}
		docs[version] = append(b, '\n')
	}
	return &Fixture{Seed: seed, Canonical: canonical, Documents: docs}, nil
}

func MustGenerateFixture(seed int64) *Fixture {
	f, err := GenerateFixture(seed)
	if err != nil {
		panic(err)
	}
	return f
}

func scenarioProject(seed int64) (*Project, error) {
	priorityJitter := int(seed % 3)
	p := &Project{
		ID:            fmt.Sprintf("edge-thermal-%d", seed),
		SchemaVersion: SchemaV2,
		TickMs:        100,
		Score: ScoreConfig{
			Bits: 32, Scale: 1024, CountWeight: 1,
			PromoteThreshold: 3000, DemoteThreshold: 1000,
			Boundary: PromoteAtBoundary,
		},
		Limits: Limits{
			MaxBatchObjects: 2, MaxBatchBytes: 4096,
			WorkerBudget: 2, PerLayerBuffer: 16, CommitDelayMs: 100, MaxWorkers: 8,
		},
		Nodes: []Node{
			{ID: "hot-a", Tier: TierHot, Rack: "rack-a", CapacityBytes: 12288, EvictionTick: 8,
				AllowedObjects: []string{"o01", "o02", "o05", "o08", "o10"}},
			{ID: "hot-b", Tier: TierHot, Rack: "rack-b", CapacityBytes: 12288, EvictionTick: 8,
				AllowedObjects: []string{"o03", "o04", "o06", "o07"}},
			{ID: "cold-a", Tier: TierCold, Rack: "rack-a", CapacityBytes: 32768, EvictionTick: 16,
				AllowedObjects: []string{"o01", "o03", "o05", "o07", "o09"}},
			{ID: "cold-b", Tier: TierCold, Rack: "rack-b", CapacityBytes: 32768, EvictionTick: 16,
				AllowedObjects: []string{"o02", "o04", "o06", "o08", "o10"}},
		},
		Objects: []Object{
			{ID: "o01", SizeBytes: 2048, InitialTier: TierHot, AccessCounts: []int64{4, 0, 0, 0, 0, 0, 0},
				LastAccessMs: 0, TTLMs: 5000, Priority: 7 + priorityJitter},
			{ID: "o02", SizeBytes: 3072, InitialTier: TierHot, AccessCounts: []int64{6, 0, 0, 0, 1, 0, 0},
				LastAccessMs: 0, TTLMs: 5000, Priority: 4 + priorityJitter},
			{ID: "o03", SizeBytes: 1024, InitialTier: TierHot, AccessCounts: []int64{2, 0, 0, 0, 0, 0, 0},
				LastAccessMs: 0, TTLMs: 5000, Priority: 8 + priorityJitter},
			{ID: "o04", SizeBytes: 4096, InitialTier: TierHot, AccessCounts: []int64{5, 0, 0, 0, 0, 0, 0},
				LastAccessMs: 0, TTLMs: 5000, Priority: 2 + priorityJitter},
			{ID: "o05", SizeBytes: 2048, InitialTier: TierCold, AccessCounts: []int64{0, 4, 0, 0, 0, 0, 0},
				LastAccessMs: 0, TTLMs: 5000, Priority: 10 + priorityJitter,
				Watermark: int64Ptr(3500)},
			{ID: "o06", SizeBytes: 4096, InitialTier: TierCold, AccessCounts: []int64{0, 0, 3, 0, 0, 0, 0},
				LastAccessMs: 0, TTLMs: 5000, Priority: 5 + priorityJitter},
			{ID: "o07", SizeBytes: 3072, InitialTier: TierCold, AccessCounts: []int64{1, 1, 1, 1, 1, 1, 1},
				LastAccessMs: 0, TTLMs: 5000, Priority: 5 + priorityJitter},
			{ID: "o08", SizeBytes: 2048, InitialTier: TierCold, AccessCounts: []int64{0, 0, 0, 0, 0, 0, 0},
				LastAccessMs: 0, TTLMs: 5000, Priority: 8 + priorityJitter},
			{ID: "o09", SizeBytes: 1024, InitialTier: TierCold, AccessCounts: []int64{0, 0, 0, 0, 0, 0, 0},
				LastAccessMs: 0, TTLMs: 300, Priority: 1 + priorityJitter},
			{ID: "o10", SizeBytes: 4096, InitialTier: TierCold, AccessCounts: []int64{0, 0, 0, 0, 0, 5, 0},
				LastAccessMs: 0, TTLMs: 5000, Priority: 3 + priorityJitter},
		},
		AccessEvents: []AccessEvent{
			{AtMs: 300, ObjectID: "o08", Count: 6},
		},
		OpEvents: []OpEvent{
			{AtMs: 100, ObjectID: "o05", Kind: OpWrite, RequestedTier: ""},
			{AtMs: 100, ObjectID: "o05", Kind: OpRead, RequestedTier: TierHot},
			{AtMs: 100, ObjectID: "o05", Kind: OpRead, RequestedTier: ""},
			{AtMs: 200, ObjectID: "o05", Kind: OpRead, RequestedTier: TierHot},
			{AtMs: 500, ObjectID: "o10", Kind: OpWrite, RequestedTier: ""},
			{AtMs: 500, ObjectID: "o10", Kind: OpRead, RequestedTier: TierHot},
			{AtMs: 600, ObjectID: "o10", Kind: OpRead, RequestedTier: TierHot},
		},
		CapacityEvents: []CapacityEvent{
			{AtMs: 300, NodeID: "hot-b", CapacityBytes: int64Ptr(16384)},
		},
		Migrations: []MigrationCommand{
			{AtMs: 500, ObjectID: "o10", FromTier: TierCold, ToTier: TierHot, TargetNodeID: "hot-a"},
		},
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	canonicalize(p)
	return p, nil
}

func int64Ptr(v int64) *int64 { return &v }

func encodeV1Document(p *Project) (any, error) {
	nodes := make([]map[string]any, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		nodes = append(nodes, map[string]any{
			"id": n.ID, "tier": n.Tier, "rack": n.Rack,
			"capacityBytes": n.CapacityBytes, "objects": n.AllowedObjects,
			"evictionTick": n.EvictionTick,
		})
	}
	objects := make([]map[string]any, 0, len(p.Objects))
	for _, o := range p.Objects {
		item := map[string]any{
			"id": o.ID, "sizeBytes": o.SizeBytes, "tier": o.InitialTier,
			"accessCounts": o.AccessCounts, "lastAccessTick": o.LastAccessMs / p.TickMs,
			"ttlTicks": o.TTLMs / p.TickMs, "priority": o.Priority,
		}
		if o.Watermark != nil {
			item["heatWatermark"] = *o.Watermark
		}
		objects = append(objects, item)
	}
	accessEvents := make([]map[string]any, 0, len(p.AccessEvents))
	for _, e := range p.AccessEvents {
		accessEvents = append(accessEvents, map[string]any{
			"atTick": e.AtMs / p.TickMs, "objectId": e.ObjectID, "count": e.Count,
		})
	}
	opEvents := make([]map[string]any, 0, len(p.OpEvents))
	for _, e := range p.OpEvents {
		opEvents = append(opEvents, map[string]any{
			"atTick": e.AtMs / p.TickMs, "objectId": e.ObjectID,
			"kind": e.Kind, "requestedTier": e.RequestedTier,
		})
	}
	capacityEvents := make([]map[string]any, 0, len(p.CapacityEvents))
	for _, e := range p.CapacityEvents {
		item := map[string]any{"atTick": e.AtMs / p.TickMs, "nodeId": e.NodeID, "delete": e.Delete}
		if e.CapacityBytes != nil {
			item["capacityBytes"] = *e.CapacityBytes
		}
		capacityEvents = append(capacityEvents, item)
	}
	migrations := make([]map[string]any, 0, len(p.Migrations))
	for _, m := range p.Migrations {
		migrations = append(migrations, map[string]any{
			"atTick": m.AtMs / p.TickMs, "objectId": m.ObjectID,
			"from": m.FromTier, "to": m.ToTier, "targetNodeId": m.TargetNodeID,
		})
	}
	return map[string]any{
		"schema": "compact/v1", "projectId": p.ID, "tickMs": p.TickMs,
		"score": map[string]any{
			"bits": p.Score.Bits, "scale": p.Score.Scale, "countWeight": p.Score.CountWeight,
			"promoteThreshold": p.Score.PromoteThreshold, "demoteThreshold": p.Score.DemoteThreshold,
			"boundary": p.Score.Boundary,
		},
		"limits": map[string]any{
			"maxBatchObjects": p.Limits.MaxBatchObjects, "maxBatchBytes": p.Limits.MaxBatchBytes,
			"workerBudget": p.Limits.WorkerBudget, "perLayerBuffer": p.Limits.PerLayerBuffer,
			"commitDelayTicks": p.Limits.CommitDelayMs / p.TickMs, "maxWorkers": p.Limits.MaxWorkers,
		},
		"nodes": nodes, "objects": objects, "accessEvents": accessEvents,
		"opEvents": opEvents, "capacityEvents": capacityEvents, "migrations": migrations,
	}, nil
}

func encodeV2Document(p *Project) (any, error) {
	nodes := make([]map[string]any, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		if n.CapacityBytes%1024 != 0 {
			return nil, biz(CodeCapacityOverflow, "generated v2 capacity is not KiB aligned", map[string]any{"node_id": n.ID})
		}
		nodes = append(nodes, map[string]any{
			"id": n.ID, "tier": n.Tier, "rack": n.Rack,
			"capacity_kib": n.CapacityBytes / 1024, "objects": n.AllowedObjects,
			"eviction_tick": n.EvictionTick,
		})
	}
	objects := make([]map[string]any, 0, len(p.Objects))
	for _, o := range p.Objects {
		item := map[string]any{
			"id": o.ID, "size_bytes": o.SizeBytes, "tier": o.InitialTier,
			"access_counts": o.AccessCounts, "last_access_ms": o.LastAccessMs,
			"ttl_ms": o.TTLMs, "priority": o.Priority,
		}
		if o.Watermark != nil {
			item["heat_watermark"] = *o.Watermark
		}
		objects = append(objects, item)
	}
	accessEvents := make([]map[string]any, 0, len(p.AccessEvents))
	for _, e := range p.AccessEvents {
		accessEvents = append(accessEvents, map[string]any{
			"at_ms": e.AtMs, "object_id": e.ObjectID, "count": e.Count,
		})
	}
	opEvents := make([]map[string]any, 0, len(p.OpEvents))
	for _, e := range p.OpEvents {
		opEvents = append(opEvents, map[string]any{
			"at_ms": e.AtMs, "object_id": e.ObjectID, "kind": e.Kind, "requested_tier": e.RequestedTier,
		})
	}
	capacityEvents := make([]map[string]any, 0, len(p.CapacityEvents))
	for _, e := range p.CapacityEvents {
		item := map[string]any{"at_ms": e.AtMs, "node_id": e.NodeID, "delete": e.Delete}
		if e.CapacityBytes != nil {
			if *e.CapacityBytes%1024 != 0 {
				return nil, biz(CodeCapacityOverflow, "generated v2 capacity event is not KiB aligned", nil)
			}
			item["capacity_kib"] = *e.CapacityBytes / 1024
		}
		capacityEvents = append(capacityEvents, item)
	}
	migrations := make([]map[string]any, 0, len(p.Migrations))
	for _, m := range p.Migrations {
		migrations = append(migrations, map[string]any{
			"at_ms": m.AtMs, "object_id": m.ObjectID,
			"from_tier": m.FromTier, "to_tier": m.ToTier, "target_node_id": m.TargetNodeID,
		})
	}
	return map[string]any{
		"schema_version": "normalized/v2", "project_id": p.ID, "tick_ms": p.TickMs,
		"score": map[string]any{
			"bits": p.Score.Bits, "scale": p.Score.Scale, "count_weight": p.Score.CountWeight,
			"promote_threshold": p.Score.PromoteThreshold, "demote_threshold": p.Score.DemoteThreshold,
			"boundary": p.Score.Boundary,
		},
		"limits": map[string]any{
			"max_batch_objects": p.Limits.MaxBatchObjects, "max_batch_bytes": p.Limits.MaxBatchBytes,
			"worker_budget": p.Limits.WorkerBudget, "per_layer_buffer": p.Limits.PerLayerBuffer,
			"commit_delay_ms": p.Limits.CommitDelayMs, "max_workers": p.Limits.MaxWorkers,
		},
		"nodes": nodes, "objects": objects, "access_events": accessEvents,
		"op_events": opEvents, "capacity_events": capacityEvents, "migrations": migrations,
	}, nil
}

func encodeV3Document(p *Project) (any, error) {
	racks := make([]string, 0)
	seen := map[string]struct{}{}
	for _, n := range p.Nodes {
		if _, ok := seen[n.Rack]; !ok {
			seen[n.Rack] = struct{}{}
			racks = append(racks, n.Rack)
		}
	}
	sort.Strings(racks)
	groups := make([]map[string]any, 0, len(racks))
	for _, rack := range racks {
		tiers := map[string][]map[string]any{"hot": {}, "cold": {}}
		for _, n := range p.Nodes {
			if n.Rack != rack {
				continue
			}
			if n.CapacityBytes%1024 != 0 {
				return nil, biz(CodeCapacityOverflow, "generated v3 capacity is not KiB aligned", map[string]any{"node_id": n.ID})
			}
			tiers[string(n.Tier)] = append(tiers[string(n.Tier)], map[string]any{
				"id": n.ID, "capacityKiB": n.CapacityBytes / 1024,
				"objects": n.AllowedObjects, "evictionTick": n.EvictionTick,
			})
		}
		groups = append(groups, map[string]any{"rack": rack, "tiers": tiers})
	}
	objectsByTier := map[string][]map[string]any{"hot": {}, "cold": {}}
	for _, o := range p.Objects {
		item := map[string]any{
			"id": o.ID, "sizeBytes": o.SizeBytes, "accessCounts": o.AccessCounts,
			"lastAccessMs": o.LastAccessMs, "ttlMs": o.TTLMs, "priority": o.Priority,
		}
		if o.Watermark != nil {
			item["heatWatermark"] = *o.Watermark
		}
		objectsByTier[string(o.InitialTier)] = append(objectsByTier[string(o.InitialTier)], item)
	}
	accessEvents := make([]map[string]any, 0, len(p.AccessEvents))
	for _, e := range p.AccessEvents {
		accessEvents = append(accessEvents, map[string]any{"atMs": e.AtMs, "objectId": e.ObjectID, "count": e.Count})
	}
	opEvents := make([]map[string]any, 0, len(p.OpEvents))
	for _, e := range p.OpEvents {
		opEvents = append(opEvents, map[string]any{
			"atMs": e.AtMs, "objectId": e.ObjectID, "kind": e.Kind, "requestedTier": e.RequestedTier,
		})
	}
	capacityEvents := make([]map[string]any, 0, len(p.CapacityEvents))
	for _, e := range p.CapacityEvents {
		item := map[string]any{"atMs": e.AtMs, "nodeId": e.NodeID, "delete": e.Delete}
		if e.CapacityBytes != nil {
			if *e.CapacityBytes%1024 != 0 {
				return nil, biz(CodeCapacityOverflow, "generated v3 capacity event is not KiB aligned", nil)
			}
			item["capacityKiB"] = *e.CapacityBytes / 1024
		}
		capacityEvents = append(capacityEvents, item)
	}
	migrations := make([]map[string]any, 0, len(p.Migrations))
	for _, m := range p.Migrations {
		migrations = append(migrations, map[string]any{
			"atMs": m.AtMs, "objectId": m.ObjectID,
			"fromTier": m.FromTier, "toTier": m.ToTier, "targetNodeId": m.TargetNodeID,
		})
	}
	return map[string]any{
		"schemaVersion": "racked/v3", "projectId": p.ID, "tickMs": p.TickMs,
		"score": map[string]any{
			"bits": p.Score.Bits, "scale": p.Score.Scale, "countWeight": p.Score.CountWeight,
			"promoteThreshold": p.Score.PromoteThreshold, "demoteThreshold": p.Score.DemoteThreshold,
			"boundary": p.Score.Boundary,
		},
		"limits": map[string]any{
			"maxBatchObjects": p.Limits.MaxBatchObjects, "maxBatchBytes": p.Limits.MaxBatchBytes,
			"workerBudget": p.Limits.WorkerBudget, "perLayerBuffer": p.Limits.PerLayerBuffer,
			"commitDelayMs": p.Limits.CommitDelayMs, "maxWorkers": p.Limits.MaxWorkers,
		},
		"rackGroups": groups, "objectsByTier": objectsByTier, "accessEvents": accessEvents,
		"opEvents": opEvents, "capacityEvents": capacityEvents, "migrations": migrations,
	}, nil
}

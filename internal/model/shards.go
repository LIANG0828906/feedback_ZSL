package model

import (
	"fmt"
	"sort"
)

func ValidateShardRanges(shards []Shard) error {
	if len(shards) == 0 {
		return Err(EShardNotFound, "project declares no shards", map[string]any{"shards": 0})
	}
	ordered := append([]Shard(nil), shards...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Low == ordered[j].Low {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Low < ordered[j].Low
	})
	seen := make(map[string]bool, len(shards))
	for i, sh := range ordered {
		if sh.ID == "" {
			return Err(EInvalidSchema, "shard id is empty", map[string]any{"shard_index": i})
		}
		if seen[sh.ID] {
			return Err(EDuplicateID, "duplicate shard id", map[string]any{"shard_id": sh.ID})
		}
		seen[sh.ID] = true
		if sh.Low == "" || sh.High == "" || sh.Low >= sh.High {
			return Err(EEmptyShardKeyspace, "shard key interval is empty or reversed",
				map[string]any{"shard_id": sh.ID, "low": sh.Low, "high": sh.High})
		}
		if i > 0 && sh.Low < ordered[i-1].High {
			return Err(EOverlapShard, "shard key intervals overlap", map[string]any{
				"left_shard": ordered[i-1].ID, "left": ordered[i-1].Low + ".." + ordered[i-1].High,
				"right_shard": sh.ID, "right": sh.Low + ".." + sh.High,
			})
		}
	}
	return nil
}

func ShardForKey(shards []Shard, key string) (Shard, error) {
	found := 0
	var matched Shard
	for _, sh := range shards {
		if key >= sh.Low && key < sh.High {
			found++
			if found == 1 {
				matched = sh
			}
		}
	}
	if found == 0 {
		return Shard{}, Err(EKeyOutsideShard, "key is outside every declared shard interval",
			map[string]any{"key": key})
	}
	if found > 1 {
		return Shard{}, Err(EOverlapShard, "key matched more than one shard interval",
			map[string]any{"key": key, "matches": found})
	}
	return matched, nil
}

// AssignShards validates the declared shard space and fills shard ownership for
// vertices (by vertex key) and edges (by source vertex key).
func AssignShards(p *Project) error {
	if err := ValidateShardRanges(p.Shards); err != nil {
		return err
	}
	vertexIDs := make(map[string]int, len(p.Vertices))
	for i := range p.Vertices {
		v := &p.Vertices[i]
		if v.ID == "" {
			return Err(EInvalidSchema, "vertex id is empty", map[string]any{"vertex_index": i})
		}
		if v.DeclOrder < 0 {
			return Err(EInvalidSchema, "vertex declaration order is negative",
				map[string]any{"vertex_id": v.ID, "decl_order": v.DeclOrder})
		}
		if _, exists := vertexIDs[v.ID]; exists {
			return Err(EDuplicateID, "duplicate vertex id", map[string]any{"vertex_id": v.ID})
		}
		vertexIDs[v.ID] = i
		sh, err := ShardForKey(p.Shards, v.ID)
		if err != nil {
			return err
		}
		if v.ShardID != "" && v.ShardID != sh.ID {
			return Err(EAmbiguousField, "vertex declares a conflicting shard",
				map[string]any{"vertex_id": v.ID, "declared": v.ShardID, "resolved": sh.ID})
		}
		v.ShardID = sh.ID
	}
	edgeIDs := make(map[string]bool, len(p.Edges))
	for i := range p.Edges {
		e := &p.Edges[i]
		if e.ID == "" {
			return Err(EInvalidSchema, "edge id is empty", map[string]any{"edge_index": i})
		}
		if edgeIDs[e.ID] {
			return Err(EDuplicateID, "duplicate edge id", map[string]any{"edge_id": e.ID})
		}
		edgeIDs[e.ID] = true
		if _, ok := vertexIDs[e.From]; !ok {
			return Err(EMissingVertex, "edge source vertex does not exist",
				map[string]any{"edge_id": e.ID, "vertex_id": e.From, "role": "source"})
		}
		if _, ok := vertexIDs[e.To]; !ok {
			return Err(EMissingVertex, "edge target vertex does not exist",
				map[string]any{"edge_id": e.ID, "vertex_id": e.To, "role": "target"})
		}
		sh, err := ShardForKey(p.Shards, e.From)
		if err != nil {
			return err
		}
		if e.ShardID != "" && e.ShardID != sh.ID {
			return Err(EAmbiguousField, "edge declares a conflicting source shard",
				map[string]any{"edge_id": e.ID, "declared": e.ShardID, "resolved": sh.ID})
		}
		e.ShardID = sh.ID
	}
	for _, q := range p.Queries {
		switch q.Kind {
		case QueryOneHop, QueryTwoHop, QueryShortestPath:
		default:
			return Err(EInvalidQuery, "unsupported query kind",
				map[string]any{"query_id": q.ID, "kind": q.Kind})
		}
		if q.MaxDepth < 0 {
			return Err(EInvalidQuery, "query depth is negative",
				map[string]any{"query_id": q.ID, "max_depth": q.MaxDepth})
		}
	}
	for _, rule := range p.IndexRules {
		if rule.Attribute == "" {
			return Err(EInvalidSchema, "index rule has an empty attribute name",
				map[string]any{"kind": rule.Kind})
		}
		switch rule.Kind {
		case AttrInt, AttrFixed:
			if rule.Bucket <= 0 {
				return Err(EInvalidSchema, "index rule bucket must be positive",
					map[string]any{"attribute": rule.Attribute, "kind": rule.Kind, "bucket": rule.Bucket})
			}
		case AttrString:
			if rule.PrefixLen <= 0 {
				return Err(EInvalidSchema, "string index rule prefix length must be positive",
					map[string]any{"attribute": rule.Attribute, "prefix_len": rule.PrefixLen})
			}
		default:
			return Err(EUnsupportedAttributeKind, "unsupported index rule attribute kind",
				map[string]any{"attribute": rule.Attribute, "kind": rule.Kind})
		}
	}
	return nil
}

func ShardForOperation(shards []Shard, key string) (Shard, error) {
	return ShardForKey(shards, key)
}

func ValidateRuleCoverage(p *Project) error {
	for i, rule := range p.IndexRules {
		seen := false
		for _, v := range p.Vertices {
			if val, ok := v.Attrs[rule.Attribute]; ok {
				if val.Kind == rule.Kind {
					seen = true
					break
				}
			}
		}
		if !seen {
			return Err(EInvalidSchema, "index rule does not match any declared vertex attribute",
				map[string]any{"rule_index": i, "attribute": rule.Attribute, "kind": rule.Kind})
		}
	}
	return nil
}

func StringifyShard(sh Shard) string {
	return fmt.Sprintf("%s[%s,%s)", sh.ID, sh.Low, sh.High)
}

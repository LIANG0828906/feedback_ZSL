package generator

import (
	"encoding/json"
	"fmt"

	"graphlab/internal/model"
)

func renderJSON(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func renderAttrV1(value model.AttrValue) (map[string]any, error) {
	switch value.Kind {
	case model.AttrInt:
		return map[string]any{"kind": "int", "value": value.Int}, nil
	case model.AttrFixed:
		if value.FixedMicros%1000 != 0 {
			return nil, fmt.Errorf("fixed value %d cannot use v1 scale 1000", value.FixedMicros)
		}
		return map[string]any{
			"kind": "fixed", "scale": 1000, "value": value.FixedMicros / 1000,
		}, nil
	case model.AttrString:
		return map[string]any{"kind": "string", "value": value.String}, nil
	default:
		return nil, fmt.Errorf("unsupported attr kind %s", value.Kind)
	}
}

func renderAttrV2(value model.AttrValue) (map[string]any, error) {
	switch value.Kind {
	case model.AttrInt:
		return map[string]any{"type": "int", "raw": value.Int}, nil
	case model.AttrFixed:
		if value.FixedMicros%100 != 0 {
			return nil, fmt.Errorf("fixed value %d cannot use v2 scale 10000", value.FixedMicros)
		}
		return map[string]any{
			"type": "decimal", "scale": 10000, "raw": value.FixedMicros / 100,
		}, nil
	case model.AttrString:
		return map[string]any{"type": "text", "raw": value.String}, nil
	default:
		return nil, fmt.Errorf("unsupported attr kind %s", value.Kind)
	}
}

func renderAttrV3(value model.AttrValue) (map[string]any, error) {
	switch value.Kind {
	case model.AttrInt:
		return map[string]any{"t": "i", "n": value.Int}, nil
	case model.AttrFixed:
		if value.FixedMicros%1000 != 0 {
			return nil, fmt.Errorf("fixed value %d cannot use v3 scale 1000", value.FixedMicros)
		}
		return map[string]any{
			"t": "d", "scale": 1000, "n": value.FixedMicros / 1000,
		}, nil
	case model.AttrString:
		return map[string]any{"t": "s", "n": value.String}, nil
	default:
		return nil, fmt.Errorf("unsupported attr kind %s", value.Kind)
	}
}

func renderAttrs(vertex *model.Vertex, renderer func(model.AttrValue) (map[string]any, error)) (map[string]any, error) {
	out := make(map[string]any, len(vertex.Attrs))
	for name, value := range vertex.Attrs {
		rendered, err := renderer(value)
		if err != nil {
			return nil, err
		}
		out[name] = rendered
	}
	return out, nil
}

func renderOperations(ops []model.Operation) []model.Operation {
	return append([]model.Operation(nil), ops...)
}

func renderQueries(queries []model.Query) []model.Query {
	return append([]model.Query(nil), queries...)
}

func renderRulesV1(rules []model.IndexRule) []map[string]any {
	out := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		m := map[string]any{"attribute": rule.Attribute, "kind": rule.Kind}
		if rule.Bucket > 0 {
			m["bucket"] = rule.Bucket
		}
		if rule.PrefixLen > 0 {
			m["prefix_len"] = rule.PrefixLen
		}
		out = append(out, m)
	}
	return out
}

func renderRulesV2(rules []model.IndexRule) []map[string]any {
	out := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		kind := rule.Kind
		if kind == model.AttrFixed {
			kind = "fixed"
		} else if kind == model.AttrString {
			kind = "text"
		}
		m := map[string]any{"attribute": rule.Attribute, "type": kind}
		if rule.Bucket > 0 {
			m["range"] = rule.Bucket
		}
		if rule.PrefixLen > 0 {
			m["prefixLength"] = rule.PrefixLen
		}
		out = append(out, m)
	}
	return out
}

func renderRulesV3(rules []model.IndexRule) []map[string]any {
	out := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		kind := map[string]string{
			model.AttrInt:    "i",
			model.AttrFixed:  "d",
			model.AttrString: "s",
		}[rule.Kind]
		m := map[string]any{"field": rule.Attribute, "class": kind}
		if rule.Bucket > 0 {
			m["span"] = rule.Bucket
		}
		if rule.PrefixLen > 0 {
			m["prefix"] = rule.PrefixLen
		}
		out = append(out, m)
	}
	return out
}

func renderRangesV1(shards []model.Shard) []map[string]any {
	out := make([]map[string]any, 0, len(shards))
	for _, sh := range shards {
		out = append(out, map[string]any{"id": sh.ID, "start_key": sh.Low, "end_key": sh.High})
	}
	return out
}

func renderRangesV2(shards []model.Shard) []map[string]any {
	out := make([]map[string]any, 0, len(shards))
	for _, sh := range shards {
		out = append(out, map[string]any{"shardId": sh.ID, "low": sh.Low, "high": sh.High})
	}
	return out
}

func renderRangesV3(shards []model.Shard) []map[string]any {
	out := make([]map[string]any, 0, len(shards))
	for _, sh := range shards {
		out = append(out, map[string]any{"partition": sh.ID, "begin": sh.Low, "end": sh.High})
	}
	return out
}

func renderV1(p *model.Project) ([]byte, error) {
	var vertices []map[string]any
	for _, v := range p.Vertices {
		attrs, err := renderAttrs(&v, renderAttrV1)
		if err != nil {
			return nil, err
		}
		vertices = append(vertices, map[string]any{
			"vertex_id": v.ID, "decl_index": v.DeclOrder,
			"labels": v.Labels, "attrs": attrs,
		})
	}
	var edges []map[string]any
	for _, e := range p.Edges {
		edges = append(edges, map[string]any{
			"edge_id": e.ID, "source_vertex": e.From, "target_vertex": e.To,
			"directed": e.Directed, "weight": e.Weight,
			"level_labels": e.Levels, "decl_index": e.DeclOrder,
		})
	}
	doc := map[string]any{
		"schema":            "graph-schema-v1",
		"project_id":        p.ID,
		"project_version":   p.Version,
		"seed":              p.Seed,
		"hierarchy_enabled": p.HierarchyEnabled,
		"clock": map[string]any{
			"unit": "tick", "value": p.ObservedAtMillis / 2, "millis_per_tick": 2,
		},
		"capacity": map[string]any{
			"unit": "byte", "value": p.CacheCapacityByte,
		},
		"vertex_ranges": renderRangesV1(p.Shards),
		"vertices":      vertices,
		"edges":         edges,
		"index_rules":   renderRulesV1(p.IndexRules),
		"queries":       renderQueries(p.Queries),
		"operations":    renderOperations(p.Operations),
	}
	return renderJSON(doc)
}

func renderV2(p *model.Project) ([]byte, error) {
	tree := make(map[string][]map[string]any)
	for _, v := range p.Vertices {
		attrs, err := renderAttrs(&v, renderAttrV2)
		if err != nil {
			return nil, err
		}
		entry := map[string]any{
			"vertexId": v.ID, "declIndex": v.DeclOrder,
			"tags": v.Labels, "attributes": attrs,
		}
		for _, label := range v.Labels {
			tree[label] = append(tree[label], entry)
		}
	}
	for _, entries := range tree {
		sortEntriesByDecl(entries)
	}
	edgeTree := make(map[string][]map[string]any)
	for _, e := range p.Edges {
		entry := map[string]any{
			"edgeId": e.ID, "from": e.From, "to": e.To,
			"directed": e.Directed, "weight": e.Weight,
			"hierarchy": e.Levels, "declIndex": e.DeclOrder,
		}
		if len(e.Levels) == 0 {
			edgeTree["_unlabeled"] = append(edgeTree["_unlabeled"], entry)
		} else {
			for _, level := range e.Levels {
				edgeTree[level] = append(edgeTree[level], entry)
			}
		}
	}
	for _, entries := range edgeTree {
		sortEntriesByDecl(entries)
	}
	doc := map[string]any{
		"schemaVersion":    "graph-schema-v2",
		"projectId":        p.ID,
		"projectRevision":  p.Version,
		"seed":             p.Seed,
		"hierarchyEnabled": p.HierarchyEnabled,
		"clock":            map[string]any{"unit": "ms", "value": p.ObservedAtMillis},
		"capacity":         map[string]any{"unit": "KiB", "value": p.CacheCapacityByte / 1024},
		"vertexShards":     renderRangesV2(p.Shards),
		"verticesByLabel":  tree,
		"edgesByHierarchy": edgeTree,
		"indexRules":       renderRulesV2(p.IndexRules),
		"queries":          renderQueries(p.Queries),
		"operations":       renderOperations(p.Operations),
	}
	return renderJSON(doc)
}

func renderV3(p *model.Project) ([]byte, error) {
	var nodes []map[string]any
	for _, v := range p.Vertices {
		attrs, err := renderAttrs(&v, renderAttrV3)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, map[string]any{
			"node_key": v.ID, "decl_order": v.DeclOrder,
			"tags": v.Labels, "props": attrs,
		})
	}
	var links []map[string]any
	for _, e := range p.Edges {
		links = append(links, map[string]any{
			"link_id": e.ID, "from_key": e.From, "to_key": e.To,
			"directed": e.Directed, "weight": e.Weight, "order": e.DeclOrder,
		})
	}
	doc := map[string]any{
		"schema":     "graph-schema-v3",
		"dataset_id": p.ID,
		"revision":   p.Version,
		"seed":       p.Seed,
		"clock": map[string]any{
			"unit": "tick", "value": p.ObservedAtMillis, "tick_ms": 1,
		},
		"capacity": map[string]any{
			"unit": "byte", "value": p.CacheCapacityByte,
		},
		"partitions":  renderRangesV3(p.Shards),
		"nodes":       nodes,
		"links":       links,
		"range_rules": renderRulesV3(p.IndexRules),
		"queries":     renderQueries(p.Queries),
		"operations":  renderOperations(p.Operations),
	}
	return renderJSON(doc)
}

func sortEntriesByDecl(entries []map[string]any) {
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			di, _ := entries[i]["declIndex"].(int)
			dj, _ := entries[j]["declIndex"].(int)
			if di > dj {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
}

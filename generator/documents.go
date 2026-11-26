package generator

import (
	"encoding/json"
	"fmt"

	"dnslab/spec"
)

func buildDocuments(p *spec.Project) (map[string][]byte, error) {
	v1, err := json.MarshalIndent(v1Document(p), "", "  ")
	if err != nil {
		return nil, err
	}
	v2, err := json.MarshalIndent(v2Document(p), "", "  ")
	if err != nil {
		return nil, err
	}
	v3, err := json.MarshalIndent(v3Document(p), "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		spec.SchemaV1: v1,
		spec.SchemaV2: v2,
		spec.SchemaV3: v3,
	}, nil
}

func literalForIndex(p *spec.Project, idx int) string {
	if idx <= 0 {
		return ""
	}
	for _, u := range p.Upstreams {
		if u.Index == idx {
			return u.Literal
		}
	}
	return ""
}

func v1Document(p *spec.Project) map[string]any {
	profiles := map[string]any{}
	for _, u := range p.Upstreams {
		profile := map[string]any{
			"authority_id":         u.ID,
			"timeout_seconds":      u.TimeoutMS / 1000,
			"simulated_latency_ms": u.LatencyMS,
		}
		if u.ForwardTo != 0 {
			profile["next_authority"] = literalForIndex(p, u.ForwardTo)
		}
		profiles[u.Literal] = profile
	}

	zones := make([]map[string]any, 0, len(p.Regions))
	for _, z := range p.Regions {
		zones = append(zones, map[string]any{
			"zone_id":     z.ID,
			"domain_name": z.Name,
			"parent_zone": z.ParentID,
		})
	}

	rules := make([]map[string]any, 0, len(p.Rules))
	for _, r := range p.Rules {
		rules = append(rules, map[string]any{
			"rule_id":       r.Number,
			"zone_id":       r.RegionID,
			"match_suffix":  r.Suffix,
			"priority":      r.Priority,
			"authority":     literalForIndex(p, r.UpstreamIndex),
			"declared_line": r.Ordinal + 1,
		})
	}

	cache := make([]map[string]any, 0, len(p.CacheSeeds))
	for _, e := range p.CacheSeeds {
		item := map[string]any{
			"qname":   e.QName,
			"qtype":   e.QType,
			"zone_id": e.RegionID,
		}
		if e.Negative {
			item["negative_ttl_seconds"] = e.NegativeTTLMS / 1000
			item["rcode"] = "NXDOMAIN"
		} else {
			recs := make([]map[string]any, 0, len(e.Records))
			for _, r := range e.Records {
				recs = append(recs, map[string]any{
					"rr_name":     r.Name,
					"rr_type":     r.Type,
					"rdata":       r.Value,
					"ttl_seconds": r.TTLMS / 1000,
				})
			}
			item["records"] = recs
		}
		cache = append(cache, item)
	}

	answers := make([]map[string]any, 0, len(p.AnswerSets))
	for _, a := range p.AnswerSets {
		recs := make([]map[string]any, 0, len(a.Records))
		for _, r := range a.Records {
			recs = append(recs, map[string]any{
				"rr_name":     r.Name,
				"rr_type":     r.Type,
				"rdata":       r.Value,
				"ttl_seconds": r.TTLMS / 1000,
			})
		}
		answers = append(answers, map[string]any{
			"qname": a.QName, "qtype": a.QType, "zone_id": a.RegionID, "records": recs,
		})
	}

	return map[string]any{
		"schema":                  spec.SchemaV1,
		"project_id":              p.ProjectID,
		"seed":                    p.Seed,
		"version":                 p.Version,
		"reference_now_seconds":   p.ReferenceNowMS / 1000,
		"default_timeout_seconds": p.DefaultTimeoutMS / 1000,
		"worker_budget":           p.WorkerBudget,
		"max_hops":                p.MaxHops,
		"negative_cache":          map[string]any{"enabled": p.NegativeCacheEnabled, "default_ttl_seconds": p.NegativeDefaultTTLMS / 1000},
		"zones":                   zones,
		"authority_profiles":      profiles,
		"flat_rules":              rules,
		"cache_entries":           cache,
		"answer_sets":             answers,
	}
}

func v2Document(p *spec.Project) map[string]any {
	zones := make([]map[string]any, 0, len(p.Regions))
	for _, z := range p.Regions {
		zones = append(zones, map[string]any{"id": z.ID, "name": z.Name, "parentId": z.ParentID})
	}
	ups := make([]map[string]any, 0, len(p.Upstreams))
	for _, u := range p.Upstreams {
		item := map[string]any{
			"upstreamIndex": u.Index,
			"upstreamId":    u.ID,
			"address":       u.Literal,
			"timeoutMs":     u.TimeoutMS,
			"latencyMs":     u.LatencyMS,
		}
		if u.ForwardTo != 0 {
			item["forwardToIndex"] = u.ForwardTo
		}
		ups = append(ups, item)
	}
	rules := make([]map[string]any, 0, len(p.Rules))
	for _, r := range p.Rules {
		rules = append(rules, map[string]any{
			"ruleNumber": r.Number, "ordinal": r.Ordinal, "zoneId": r.RegionID,
			"matchSuffix": r.Suffix, "priority": r.Priority, "upstreamIndex": r.UpstreamIndex,
		})
	}
	cache := make([]map[string]any, 0, len(p.CacheSeeds))
	for _, e := range p.CacheSeeds {
		item := map[string]any{
			"cacheKey": e.CacheKey, "qname": e.QName, "qtype": e.QType, "regionId": e.RegionID,
			"negative": e.Negative,
		}
		if e.Negative {
			item["negativeTtlMs"] = e.NegativeTTLMS
		} else {
			recs := make([]map[string]any, 0, len(e.Records))
			for _, r := range e.Records {
				recs = append(recs, map[string]any{
					"name": r.Name, "type": r.Type, "value": r.Value, "ttlMs": r.TTLMS,
				})
			}
			item["records"] = recs
		}
		cache = append(cache, item)
	}
	answers := make([]map[string]any, 0, len(p.AnswerSets))
	for _, a := range p.AnswerSets {
		recs := make([]map[string]any, 0, len(a.Records))
		for _, r := range a.Records {
			recs = append(recs, map[string]any{"name": r.Name, "type": r.Type, "value": r.Value, "ttlMs": r.TTLMS})
		}
		answers = append(answers, map[string]any{
			"cacheKey": a.CacheKey, "qname": a.QName, "qtype": a.QType, "regionId": a.RegionID, "records": recs,
		})
	}
	return map[string]any{
		"schemaVersion":        spec.SchemaV2,
		"projectId":            p.ProjectID,
		"seed":                 p.Seed,
		"version":              p.Version,
		"referenceNowMs":       p.ReferenceNowMS,
		"defaultTimeoutMs":     p.DefaultTimeoutMS,
		"workerBudget":         p.WorkerBudget,
		"maxHops":              p.MaxHops,
		"negativeCacheEnabled": p.NegativeCacheEnabled,
		"negativeDefaultTtlMs": p.NegativeDefaultTTLMS,
		"zones":                zones,
		"upstreams":            ups,
		"rules":                rules,
		"cacheSeeds":           cache,
		"answerSets":           answers,
	}
}

func v3Document(p *spec.Project) map[string]any {
	nodes := map[string]map[string]any{}
	for _, z := range p.Regions {
		nodes[z.ID] = map[string]any{
			"regionId": z.ID, "domain": z.Name, "rules": []map[string]any{}, "children": []map[string]any{},
		}
	}
	for _, r := range p.Rules {
		node := nodes[r.RegionID]
		rules := node["rules"].([]map[string]any)
		rules = append(rules, map[string]any{
			"ruleId": r.Number, "match": r.Suffix, "precedence": r.Priority, "upstreamRef": r.UpstreamIndex,
		})
		node["rules"] = rules
	}
	var roots []map[string]any
	for _, z := range p.Regions {
		node := nodes[z.ID]
		if z.ParentID == "" {
			roots = append(roots, node)
			continue
		}
		parent := nodes[z.ParentID]
		children := parent["children"].([]map[string]any)
		children = append(children, node)
		parent["children"] = children
	}

	pool := make([]map[string]any, 0, len(p.Upstreams))
	for _, u := range p.Upstreams {
		item := map[string]any{
			"number": u.Index, "name": u.ID, "literal": u.Literal,
			"timeout":             map[string]any{"milliseconds": u.TimeoutMS},
			"latencyMilliseconds": u.LatencyMS,
		}
		if u.ForwardTo != 0 {
			item["forwardToNumber"] = u.ForwardTo
		}
		pool = append(pool, item)
	}

	segments := make([]map[string]any, 0, len(p.CacheSeeds))
	for _, e := range p.CacheSeeds {
		entry := map[string]any{
			"qname": e.QName, "qtype": e.QType, "negative": e.Negative,
		}
		if e.Negative {
			entry["negativeExpiresAtMs"] = e.NegativeExpiresAtMS
		} else {
			recs := make([]map[string]any, 0, len(e.Records))
			for _, r := range e.Records {
				recs = append(recs, map[string]any{
					"name": r.Name, "type": r.Type, "value": r.Value, "expiresAtMs": r.ExpiresAtMS,
				})
			}
			entry["records"] = recs
		}
		segments = append(segments, map[string]any{"regionId": e.RegionID, "entries": []map[string]any{entry}})
	}

	answers := make([]map[string]any, 0, len(p.AnswerSets))
	for _, a := range p.AnswerSets {
		recs := make([]map[string]any, 0, len(a.Records))
		for _, r := range a.Records {
			recs = append(recs, map[string]any{"name": r.Name, "type": r.Type, "value": r.Value, "validForMs": r.TTLMS})
		}
		answers = append(answers, map[string]any{"qname": a.QName, "qtype": a.QType, "regionId": a.RegionID, "records": recs})
	}

	return map[string]any{
		"apiVersion": spec.SchemaV3,
		"metadata": map[string]any{
			"projectId": p.ProjectID, "seed": fmt.Sprint(p.Seed), "snapshotVersion": p.Version,
			"logicalClock": map[string]any{"referenceEpochMs": p.ReferenceNowMS},
		},
		"settings": map[string]any{
			"defaultTimeoutMs": p.DefaultTimeoutMS,
			"workerThreads":    p.WorkerBudget,
			"maxForwardHops":   p.MaxHops,
			"negativeCache":    map[string]any{"enabled": p.NegativeCacheEnabled, "defaultTtlMs": p.NegativeDefaultTTLMS},
		},
		"upstreamPool": pool,
		"regionTree":   roots,
		"cache":        map[string]any{"segments": segments},
		"answers":      answers,
	}
}

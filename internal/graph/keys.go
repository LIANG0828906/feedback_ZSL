package graph

import (
	"msworkbench/internal/domain"
)

// RefreshDirtyKeys recomputes content-addressed keys after an edit. Clean
// nodes deliberately retain their old key so persistent results can be reused.
func (g *Graph) RefreshDirtyKeys() {
	paramKey := ""
	if n := g.Nodes["param:adaptive"]; n != nil {
		paramKey = n.CacheKey
	}
	schemaID := "schema:" + g.Project.SourceSchema
	schemaKey := ""
	if n := g.Nodes[schemaID]; n != nil {
		schemaKey = n.CacheKey
	}
	for _, coord := range g.Project.Coords() {
		block := coord.String()
		contentHash := g.Sample.BlockHashes[block]
		if n := g.Nodes["input:"+block]; n != nil {
			contentHash = n.CacheKey
		}
		decision := g.Decisions[block]
		for _, b := range g.Project.Bands {
			id := quantNodeID(block, b.ID)
			n := g.Nodes[id]
			if n == nil || !n.Dirty || n.Kind != KindQuant {
				continue
			}
			bandKey := g.Nodes[bandNodeID(b.ID)].CacheKey
			upstreamIDs := []string{"input:" + block, bandNodeID(b.ID), "param:adaptive", schemaID}
			upstreamKeys := []string{contentHash, bandKey, paramKey, schemaKey}
			n.Upstream = upstreamKeys
			n.CacheKey = domain.HashJSON(map[string]any{
				"content_hash": contentHash, "band_id": b.ID,
				"param_version": domain.ParamVersion, "algo_version": domain.AlgorithmVersion,
				"decision": decision, "upstream_keys": upstreamKeys,
			})
			_ = upstreamIDs
		}
		rn := g.Nodes[reconNodeID(block)]
		if rn != nil && rn.Dirty && rn.Kind == KindRecon {
			keys := []string{}
			for _, b := range g.Project.Bands {
				if qn := g.Nodes[quantNodeID(block, b.ID)]; qn != nil {
					keys = append(keys, qn.CacheKey)
				}
			}
			rn.Upstream = keys
			rn.CacheKey = domain.HashJSON(map[string]any{
				"block": block, "param_version": domain.ParamVersion,
				"algo_version": domain.AlgorithmVersion, "quant_keys": keys,
				"white": g.Project.WhitePointBP,
			})
		}
	}
}

func QuantAggregateKey(g *Graph, block string) string {
	keys := map[string]string{}
	for _, b := range g.Project.Bands {
		if n := g.Nodes[quantNodeID(block, b.ID)]; n != nil {
			keys[b.ID] = n.CacheKey
		}
	}
	return domain.HashJSON(keys)
}

func BandCacheKeyMap(g *Graph, block string) map[string]string {
	out := map[string]string{}
	for _, b := range g.Project.Bands {
		if n := g.Nodes[quantNodeID(block, b.ID)]; n != nil {
			out[b.ID] = n.CacheKey
		}
	}
	return out
}

func ReconCacheKey(g *Graph, block string) string {
	if n := g.Nodes[reconNodeID(block)]; n != nil {
		return n.CacheKey
	}
	return ""
}

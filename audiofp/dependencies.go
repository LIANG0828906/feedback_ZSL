package audiofp

import (
	"fmt"
	"sort"
)

type DependencyNode struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	AssetID  string   `json:"asset_id,omitempty"`
	FrameNo  int      `json:"frame_no,omitempty"`
	Upstream []string `json:"upstream,omitempty"`
}

type DependencyGraph struct {
	RecipeKey string                    `json:"recipe_key"`
	Nodes     map[string]DependencyNode `json:"nodes"`
}

func BuildDependencyGraph(assets []PCMAsset, r Recipe) *DependencyGraph {
	graph := &DependencyGraph{RecipeKey: digestString("recipe-node|" + RecipeHash(r)), Nodes: make(map[string]DependencyNode)}
	graph.Nodes[graph.RecipeKey] = DependencyNode{ID: graph.RecipeKey, Kind: "recipe"}
	ordered := append([]PCMAsset(nil), assets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	queryKey := digestString("query-result|" + RecipeHash(r))
	graph.Nodes[queryKey] = DependencyNode{ID: queryKey, Kind: "query_result"}
	for _, asset := range ordered {
		contentHash := asset.ContentHash()
		contentKey := digestString("asset-content|" + contentHash)
		graph.Nodes[contentKey] = DependencyNode{ID: contentKey, Kind: "asset_content", AssetID: asset.ID}
		count := frameCount(len(asset.Samples), r)
		for frameNo := 0; frameNo < count; frameNo++ {
			frameKey := frameCacheKey(contentHash, frameNo, r)
			indexKey := indexCacheKey(frameKey, frameNo, FrameStartOffset(frameNo, r))
			graph.Nodes[frameKey] = DependencyNode{
				ID: frameKey, Kind: "fingerprint", AssetID: asset.ID, FrameNo: frameNo,
				Upstream: []string{contentKey, graph.RecipeKey},
			}
			graph.Nodes[indexKey] = DependencyNode{
				ID: indexKey, Kind: "index", AssetID: asset.ID, FrameNo: frameNo,
				Upstream: []string{frameKey},
			}
			node := graph.Nodes[queryKey]
			node.Upstream = append(node.Upstream, indexKey)
			graph.Nodes[queryKey] = node
		}
	}
	return graph
}

type Invalidation struct {
	AssetID    string `json:"asset_id"`
	Kind       string `json:"kind"`
	StartFrame int    `json:"start_frame"`
	EndFrame   int    `json:"end_frame"`
	Reason     string `json:"reason"`
}

func invalidateAsset(asset PCMAsset, r Recipe, kind, reason string) Invalidation {
	count := frameCount(len(asset.Samples), r)
	return Invalidation{
		AssetID: asset.ID, Kind: kind, StartFrame: 0, EndFrame: count, Reason: reason,
	}
}

func queryInvalidation(assetID, reason string) Invalidation {
	return Invalidation{AssetID: assetID, Kind: "query", StartFrame: 0, EndFrame: 0, Reason: reason}
}

func (g *DependencyGraph) Invalidate(affected map[string]PCMAsset, r Recipe, frameAffected bool, reason string) []Invalidation {
	ids := make([]string, 0, len(affected))
	for id := range affected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Invalidation
	for _, id := range ids {
		if frameAffected {
			out = append(out, invalidateAsset(affected[id], r, "fingerprint+index", reason))
		}
		out = append(out, queryInvalidation(id, reason))
	}
	return out
}

func (g *DependencyGraph) Summary() string {
	return fmt.Sprintf("%d dependency nodes", len(g.Nodes))
}

// TopologicalOrder returns a deterministic upstream-before-downstream order,
// or nil when the graph contains a cycle.
func (g *DependencyGraph) TopologicalOrder() []DependencyNode {
	indegree := make(map[string]int, len(g.Nodes))
	children := make(map[string][]string, len(g.Nodes))
	for id := range g.Nodes {
		indegree[id] = 0
	}
	for id, node := range g.Nodes {
		for _, upstream := range node.Upstream {
			if _, ok := g.Nodes[upstream]; !ok {
				continue
			}
			indegree[id]++
			children[upstream] = append(children[upstream], id)
		}
	}
	var ready []string
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	out := make([]DependencyNode, 0, len(g.Nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, g.Nodes[id])
		next := append([]string(nil), children[id]...)
		sort.Strings(next)
		for _, child := range next {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
			}
		}
		sort.Strings(ready)
	}
	if len(out) != len(g.Nodes) {
		return nil
	}
	return out
}

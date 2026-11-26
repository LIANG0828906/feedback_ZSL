package maskqc

import (
	"fmt"
	"sort"
)

type NodeKind string

const (
	NodeConvert   NodeKind = "conversion_parameters"
	NodeStroke    NodeKind = "stroke"
	NodeOrder     NodeKind = "composition_order"
	NodeChunk     NodeKind = "chunk_coverage"
	NodeTopology  NodeKind = "topology_verification"
	NodeDirection NodeKind = "direction_verification"
)

type DependencyNode struct {
	ID   string   `json:"id"`
	Kind NodeKind `json:"kind"`
}

type DependencyEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type DependencyGraph struct {
	Nodes []DependencyNode `json:"nodes"`
	Edges []DependencyEdge `json:"edges"`
}

func chunkNode(id int) string     { return fmt.Sprintf("chunk:%d:coverage", id) }
func strokeNode(id string) string { return "stroke:" + id }

func BuildDependencyGraph(p *Project, g ChunkGrid) DependencyGraph {
	dg := DependencyGraph{}
	nodeSet := map[string]bool{}
	addNode := func(id string, kind NodeKind) {
		if !nodeSet[id] {
			nodeSet[id] = true
			dg.Nodes = append(dg.Nodes, DependencyNode{ID: id, Kind: kind})
		}
	}
	addEdge := func(from, to, reason string) {
		dg.Edges = append(dg.Edges, DependencyEdge{From: from, To: to, Reason: reason})
	}
	addNode(ConvertVersion, NodeConvert)
	addNode(OrderVersion, NodeOrder)
	addNode("verification:topology", NodeTopology)
	addNode("verification:direction", NodeDirection)
	orders := PlanOrders(p, g)
	for cid := 0; cid < g.Count; cid++ {
		cn := chunkNode(cid)
		addNode(cn, NodeChunk)
		addNode(fmt.Sprintf("chunk:%d:candidate_input", cid), NodeChunk)
		addEdge(fmt.Sprintf("chunk:%d:candidate_input", cid), cn, "candidate pixels are chunk input")
		addEdge(OrderVersion, cn, orders[cid].Basis)
		addEdge(ConvertVersion, cn, "integer coordinate/weight conversion version")
		addEdge(cn, "verification:topology", "all coverage chunks form the accumulated mask")
		addEdge("verification:topology", "verification:direction", "boundary normals are checked after topology extraction")
		for _, sid := range orders[cid].StrokeIDs {
			sn := strokeNode(sid)
			addNode(sn, NodeStroke)
			addEdge(sn, cn, "rasterized stroke contributes coverage")
			addEdge(ConvertVersion, sn, "coordinate/weight conversion")
		}
	}
	sort.SliceStable(dg.Nodes, func(i, j int) bool { return dg.Nodes[i].ID < dg.Nodes[j].ID })
	sort.SliceStable(dg.Edges, func(i, j int) bool {
		if dg.Edges[i].From != dg.Edges[j].From {
			return dg.Edges[i].From < dg.Edges[j].From
		}
		return dg.Edges[i].To < dg.Edges[j].To
	})
	return dg
}

type Edit struct {
	Type      string  `json:"type"`
	StrokeID  string  `json:"stroke_id"`
	NewStroke *Stroke `json:"new_stroke,omitempty"`
}

type InvalidationItem struct {
	ChunkID         int      `json:"chunk_id"`
	TriggerStroke   string   `json:"trigger_stroke"`
	Change          string   `json:"change"`
	Reason          string   `json:"reason"`
	PropagationPath []string `json:"propagation_path"`
}

type InvalidationPlan struct {
	Items          []InvalidationItem `json:"items"`
	InvalidChunks  []int              `json:"invalid_chunks"`
	ReusableChunks []int              `json:"reusable_chunks"`
	Downstream     []string           `json:"downstream"`
}

func PlanEditInvalidation(before, after *Project, g ChunkGrid, edits []Edit) InvalidationPlan {
	plan := InvalidationPlan{Downstream: []string{"verification:topology", "verification:direction"}}
	invalidSet := map[int]map[string]InvalidationItem{}
	add := func(cid int, sid, change, reason string) {
		if cid < 0 {
			return
		}
		if invalidSet[cid] == nil {
			invalidSet[cid] = map[string]InvalidationItem{}
		}
		key := sid + ":" + change
		invalidSet[cid][key] = InvalidationItem{
			ChunkID: cid, TriggerStroke: sid, Change: change, Reason: reason,
			PropagationPath: []string{
				strokeNode(sid), chunkNode(cid), "verification:topology", "verification:direction",
			},
		}
	}
	for _, edit := range edits {
		var oldStroke, newStroke *Stroke
		if before != nil {
			if s, ok := before.StrokeByID(edit.StrokeID); ok {
				oldStroke = &s
			}
		}
		if edit.NewStroke != nil {
			newStroke = edit.NewStroke
		} else if after != nil {
			if s, ok := after.StrokeByID(edit.StrokeID); ok {
				newStroke = &s
			}
		}
		change := edit.Type
		if change == "" {
			change = "update"
		}
		if oldStroke != nil {
			for _, cid := range g.AffectedChunks(*oldStroke) {
				add(cid, edit.StrokeID, change, "old stroke bounding box intersects chunk")
			}
		}
		if newStroke != nil {
			for _, cid := range g.AffectedChunks(*newStroke) {
				reason := "new stroke bounding box intersects chunk"
				if oldStroke == nil {
					reason = "inserted stroke bounding box intersects chunk"
				}
				add(cid, edit.StrokeID, change, reason)
			}
		}
	}
	invalid := map[int]bool{}
	for cid, byKey := range invalidSet {
		invalid[cid] = true
		for _, item := range byKey {
			plan.Items = append(plan.Items, item)
		}
	}
	for cid := range invalid {
		plan.InvalidChunks = append(plan.InvalidChunks, cid)
	}
	sort.Ints(plan.InvalidChunks)
	sort.SliceStable(plan.Items, func(i, j int) bool {
		if plan.Items[i].ChunkID != plan.Items[j].ChunkID {
			return plan.Items[i].ChunkID < plan.Items[j].ChunkID
		}
		return plan.Items[i].TriggerStroke < plan.Items[j].TriggerStroke
	})
	for cid := 0; cid < g.Count; cid++ {
		if !invalid[cid] {
			plan.ReusableChunks = append(plan.ReusableChunks, cid)
		}
	}
	return plan
}

// CacheKeyInputs documents the exact deterministic cache-key composition.
type CacheKeyInputs struct {
	ChunkID        int      `json:"chunk_id"`
	ChunkInputHash string   `json:"chunk_input_hash"`
	StrokeIDs      []string `json:"stroke_ids"`
	ComponentKeys  []string `json:"component_keys"`
	OrderVersion   string   `json:"order_version"`
	ConvertVersion string   `json:"convert_version"`
	UpstreamKey    string   `json:"upstream_key"`
	FinalCacheKey  string   `json:"final_cache_key"`
}

func BuildChunkCacheKey(cid int, chunkInputHash string, components []StrokeComponent, order []string) CacheKeyInputs {
	keys := []string{}
	ids := []string{}
	byID := map[string]StrokeComponent{}
	for _, c := range components {
		byID[c.StrokeID] = c
	}
	for _, sid := range order {
		if c, ok := byID[sid]; ok {
			ids = append(ids, sid)
			keys = append(keys, c.CacheKey)
		}
	}
	upstream := HashBytes([]byte(joinStrings(keys, "|")))
	key := stableJSONHash(map[string]any{
		"chunk": cid, "input": chunkInputHash, "components": keys,
		"order": OrderVersion, "convert": ConvertVersion, "upstream": upstream,
	})
	return CacheKeyInputs{
		ChunkID: cid, ChunkInputHash: chunkInputHash, StrokeIDs: ids,
		ComponentKeys: keys, OrderVersion: OrderVersion, ConvertVersion: ConvertVersion,
		UpstreamKey: upstream, FinalCacheKey: key,
	}
}

func joinStrings(items []string, sep string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

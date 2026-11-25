package deps

import (
	"fmt"
	"sort"

	"maskqa/internal/engine"
	"maskqa/internal/model"
)

type Node struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Hash         string   `json:"hash,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

type Graph struct {
	Nodes []Node `json:"nodes"`
}

func BuildGraph(p *model.Project) Graph {
	g := Graph{}
	strokeByID := map[string]model.Stroke{}
	for _, s := range p.Strokes {
		strokeByID[s.ID] = s
		g.Nodes = append(g.Nodes, Node{ID: "stroke:" + s.ID, Kind: "stroke", Hash: s.ContentHash})
	}
	g.Nodes = append(g.Nodes, Node{
		ID: "order:adjudication", Kind: "order", Hash: model.OrderVersion,
		Dependencies: strokeNodeIDs(p.Strokes),
	})
	for _, c := range p.Chunks {
		var deps []string
		for _, s := range p.Strokes {
			if engine.StrokeIntersectsChunk(s, c) {
				deps = append(deps, "stroke:"+s.ID)
				compID := fmt.Sprintf("component:%s:%s", c.ID, s.ID)
				g.Nodes = append(g.Nodes, Node{ID: compID, Kind: "stroke_chunk_component", Dependencies: []string{"stroke:" + s.ID, "chunk_input:" + c.ID}})
				deps = append(deps, compID)
			}
		}
		deps = append(deps, "order:adjudication", "chunk_input:"+c.ID)
		g.Nodes = append(g.Nodes, Node{ID: "chunk:" + c.ID, Kind: "chunk_output", Dependencies: uniqueSorted(deps)})
	}
	chunkIDs := make([]string, 0, len(p.Chunks))
	for _, c := range p.Chunks {
		chunkIDs = append(chunkIDs, "chunk:"+c.ID)
		g.Nodes = append(g.Nodes, Node{ID: "chunk_input:" + c.ID, Kind: "chunk_input", Hash: ChunkInputHash(p.Candidate, c)})
	}
	g.Nodes = append(g.Nodes, Node{ID: "topology:verification", Kind: "topology", Hash: model.TopologyVersion, Dependencies: chunkIDs})
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	return g
}

func strokeNodeIDs(strokes []model.Stroke) []string {
	out := make([]string, 0, len(strokes))
	for _, s := range strokes {
		out = append(out, "stroke:"+s.ID)
	}
	return uniqueSorted(out)
}

func ChunkInputHash(candidate model.Mask, c model.Chunk) string {
	pix := engine.ChunkCoverageFromMask(candidate, c)
	return model.HashBytes("chunk-input-v1", pix)
}

func StrokeComponentKey(chunkInputHash, chunkID string, s model.Stroke, upstream ...string) string {
	return model.HashCanonical(map[string]any{
		"kind":             "stroke_component",
		"algorithm":        model.AlgorithmVersion,
		"order_version":    model.OrderVersion,
		"params_version":   model.ParamsVersion,
		"chunk_input_hash": chunkInputHash,
		"chunk_id":         chunkID,
		"stroke_id":        s.ID,
		"stroke_hash":      s.ContentHash,
		"upstream":         uniqueSorted(upstream),
	})
}

func ChunkOutputKey(c model.Chunk, inputHash string, orderedComponentKeys []string) string {
	return model.HashCanonical(map[string]any{
		"kind":             "chunk_output",
		"algorithm":        model.AlgorithmVersion,
		"order_version":    model.OrderVersion,
		"params_version":   model.ParamsVersion,
		"chunk_id":         c.ID,
		"chunk_input_hash": inputHash,
		"components":       orderedComponentKeys,
	})
}

func TopologyKey(chunkOutputKeys map[string]string) string {
	ids := make([]string, 0, len(chunkOutputKeys))
	for id := range chunkOutputKeys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, id+":"+chunkOutputKeys[id])
	}
	return model.HashCanonical(map[string]any{"kind": "topology", "version": model.TopologyVersion, "chunks": keys})
}

func PlanEdit(oldProject, newProject *model.Project) model.InvalidationPlan {
	plan := model.InvalidationPlan{}
	if oldProject == nil {
		for _, c := range newProject.Chunks {
			plan.AffectedChunks = append(plan.AffectedChunks, c.ID)
			plan.Items = append(plan.Items, model.InvalidationItem{Kind: "chunk", ID: c.ID, Reason: "no_persisted_graph_all_chunks_required"})
		}
		plan.InvalidateTopology = true
		sort.Strings(plan.AffectedChunks)
		return plan
	}
	if oldProject.Width != newProject.Width || oldProject.Height != newProject.Height || oldProject.ChunkSize != newProject.ChunkSize {
		for _, c := range newProject.Chunks {
			plan.AffectedChunks = append(plan.AffectedChunks, c.ID)
		}
		plan.AffectedStrokes = []string{"*"}
		plan.InvalidateTopology = true
		plan.Items = append(plan.Items, model.InvalidationItem{Kind: "geometry", ID: newProject.ID, Reason: "canvas_or_chunk_layout_changed"})
		return finalizePlan(plan, newProject)
	}

	oldByID := map[string]model.Stroke{}
	newByID := map[string]model.Stroke{}
	for _, s := range oldProject.Strokes {
		oldByID[s.ID] = s
	}
	for _, s := range newProject.Strokes {
		newByID[s.ID] = s
	}

	for id, oldS := range oldByID {
		newS, exists := newByID[id]
		if !exists {
			plan.AffectedStrokes = append(plan.AffectedStrokes, id)
			plan.Items = append(plan.Items, model.InvalidationItem{Kind: "stroke_deleted", ID: id, StrokeID: id, Reason: "stroke_deleted"})
			unionChunks(&plan, oldProject, oldS)
			continue
		}
		if oldS.ContentHash == newS.ContentHash {
			continue
		}
		plan.AffectedStrokes = append(plan.AffectedStrokes, id)
		changed := changedFields(oldS, newS)
		for _, change := range changed {
			plan.Items = append(plan.Items, model.InvalidationItem{
				Kind: "stroke_edited", ID: id, StrokeID: id, Field: change.field,
				Reason: change.reason, OldValue: change.old, NewValue: change.new,
			})
		}
		if geometryChanged(oldS, newS) {
			unionChunks(&plan, oldProject, oldS)
			unionChunks(&plan, newProject, newS)
		} else {
			unionChunks(&plan, newProject, newS)
		}
	}
	for id, newS := range newByID {
		if _, exists := oldByID[id]; exists {
			continue
		}
		plan.AffectedStrokes = append(plan.AffectedStrokes, id)
		plan.Items = append(plan.Items, model.InvalidationItem{Kind: "stroke_added", ID: id, StrokeID: id, Reason: "stroke_added"})
		unionChunks(&plan, newProject, newS)
	}
	return finalizePlan(plan, newProject)
}

type fieldChange struct {
	field  string
	reason string
	old    string
	new    string
}

func changedFields(oldS, newS model.Stroke) []fieldChange {
	var out []fieldChange
	if oldS.From != newS.From {
		out = append(out, fieldChange{"from", "coordinate_changed_old_and_new_footprints_invalid", fmt.Sprint(oldS.From), fmt.Sprint(newS.From)})
	}
	if oldS.To != newS.To {
		out = append(out, fieldChange{"to", "coordinate_changed_old_and_new_footprints_invalid", fmt.Sprint(oldS.To), fmt.Sprint(newS.To)})
	}
	if oldS.WeightBP != newS.WeightBP {
		out = append(out, fieldChange{"weight_bp", "weight_changed_components_invalid", fmt.Sprint(oldS.WeightBP), fmt.Sprint(newS.WeightBP)})
	}
	if oldS.HardnessBP != newS.HardnessBP {
		out = append(out, fieldChange{"hardness_bp", "hardness_changed_components_invalid", fmt.Sprint(oldS.HardnessBP), fmt.Sprint(newS.HardnessBP)})
	}
	if oldS.WidthPx != newS.WidthPx {
		out = append(out, fieldChange{"width_px", "brush_width_changed_footprint_invalid", fmt.Sprint(oldS.WidthPx), fmt.Sprint(newS.WidthPx)})
	}
	if oldS.Priority != newS.Priority || oldS.No != newS.No {
		out = append(out, fieldChange{"order", "priority_or_stroke_no_changed_adjudication_invalid", fmt.Sprintf("p%d/no%d", oldS.Priority, oldS.No), fmt.Sprintf("p%d/no%d", newS.Priority, newS.No)})
	}
	if len(out) == 0 && oldS.ContentHash != newS.ContentHash {
		out = append(out, fieldChange{"content", "stroke_content_hash_changed", oldS.ContentHash[:12], newS.ContentHash[:12]})
	}
	return out
}

func geometryChanged(a, b model.Stroke) bool {
	return a.From != b.From || a.To != b.To || a.WidthPx != b.WidthPx
}

func unionChunks(plan *model.InvalidationPlan, p *model.Project, s model.Stroke) {
	for _, id := range engine.AffectedChunks(s, p.Chunks) {
		plan.AffectedChunks = append(plan.AffectedChunks, id)
		plan.Items = append(plan.Items, model.InvalidationItem{
			Kind: "chunk_invalidated", ID: id, StrokeID: s.ID,
			Reason: "downstream_chunk_depends_on_affected_stroke",
		})
	}
}

func finalizePlan(plan model.InvalidationPlan, p *model.Project) model.InvalidationPlan {
	plan.AffectedStrokes = uniqueSorted(plan.AffectedStrokes)
	plan.AffectedChunks = uniqueSorted(plan.AffectedChunks)
	affected := map[string]bool{}
	for _, id := range plan.AffectedChunks {
		affected[id] = true
	}
	for _, c := range p.Chunks {
		if !affected[c.ID] {
			plan.ReusedChunks = append(plan.ReusedChunks, c.ID)
		}
	}
	sort.Strings(plan.ReusedChunks)
	if len(plan.AffectedChunks) > 0 {
		plan.InvalidateTopology = true
		plan.Items = append(plan.Items, model.InvalidationItem{Kind: "topology_invalidated", ID: "topology:verification", Reason: "downstream_topology_depends_on_invalid_chunks"})
	}
	plan.Items = append(plan.Items, model.InvalidationItem{Kind: "order_invalidated", ID: "order:adjudication", Reason: "order_node_recomputed_from_deterministic_priority_no_rules"})
	return plan
}

func uniqueSorted(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

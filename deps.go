package terrainbake

import (
	"fmt"
	"math"
	"sort"
)

// EditKind selects the mutation used by incremental invalidation.
type EditKind string

const (
	EditHeight   EditKind = "height"
	EditMaterial EditKind = "material"
	EditWeight   EditKind = "weight"
)

// Edit is a serializable edit operation. For height edits X/Y/Value replace a
// sample. For material edits MaterialID/Field/Value mutate a global layer
// parameter. Weight edits replace one quantized layer weight and rebalance a
// texel deterministically.
type Edit struct {
	Kind       EditKind `json:"kind"`
	BlockID    string   `json:"block_id,omitempty"`
	X          int      `json:"x,omitempty"`
	Y          int      `json:"y,omitempty"`
	Value      float64  `json:"value,omitempty"`
	MaterialID string   `json:"material_id,omitempty"`
	Field      string   `json:"field,omitempty"`
	PixelX     int      `json:"pixel_x,omitempty"`
	PixelY     int      `json:"pixel_y,omitempty"`
	Layer      int      `json:"layer,omitempty"`
	Weight     int      `json:"weight,omitempty"`
}

// DependencyGraph records bilateral seam dependencies plus material users.
// Seam edges are explicitly inserted in both directions.
type DependencyGraph struct {
	Blocks        []string            `json:"blocks"`
	SeamEdges     map[string][]string `json:"seam_edges"`
	MaterialUsers map[string][]string `json:"material_users"`
}

type invalidationSource struct {
	id     string
	reason string
	chain  []string
}

func BuildDependencyGraph(p *Project) *DependencyGraph {
	g := &DependencyGraph{
		Blocks:        canonicalBlockIDs(p),
		SeamEdges:     make(map[string][]string),
		MaterialUsers: make(map[string][]string),
	}
	for _, b := range p.Blocks {
		g.SeamEdges[b.ID] = []string{}
	}
	for _, s := range p.Seams {
		if s.BlockA == "" || s.BlockB == "" || s.BlockA == s.BlockB {
			continue
		}
		g.SeamEdges[s.BlockA] = appendUnique(g.SeamEdges[s.BlockA], s.BlockB)
		g.SeamEdges[s.BlockB] = appendUnique(g.SeamEdges[s.BlockB], s.BlockA)
	}
	for k := range g.SeamEdges {
		sort.Strings(g.SeamEdges[k])
	}
	for _, b := range p.Blocks {
		for _, mid := range b.Materials {
			g.MaterialUsers[mid] = appendUnique(g.MaterialUsers[mid], b.ID)
		}
	}
	for k := range g.MaterialUsers {
		sort.Strings(g.MaterialUsers[k])
	}
	return g
}

func appendUnique(in []string, v string) []string {
	for _, x := range in {
		if x == v {
			return in
		}
	}
	return append(in, v)
}

// ApplyEdits mutates the project and returns exact invalidation records. The
// propagation follows bilateral seam dependencies; material edits seed every
// block that consumes the edited material, then propagate through seams.
func ApplyEdits(p *Project, g *DependencyGraph, edits []Edit) ([]InvalidationItem, error) {
	if len(edits) == 0 {
		return nil, nil
	}
	sources := make(map[string]invalidationSource)
	for i, e := range edits {
		switch e.Kind {
		case EditHeight:
			b, ok := FindBlock(p, e.BlockID)
			if !ok {
				return nil, &Error{Code: CodeSchema, Object: "edit", Field: "block_id", BlockID: e.BlockID, Message: fmt.Sprintf("edit %d references a missing block", i)}
			}
			if e.X < 0 || e.X >= b.Width || e.Y < 0 || e.Y >= b.Height {
				return nil, &Error{Code: CodeSchema, Object: "block:" + b.ID, Field: "heights", BlockID: b.ID, X: e.X, Y: e.Y, Message: "height edit is outside the block"}
			}
			if !validFinite(e.Value) {
				return nil, &Error{Code: CodeParam, Object: "edit", Field: "value", BlockID: b.ID, Message: "height value must be finite"}
			}
			if b.Elevation[e.Y*b.Width+e.X] != e.Value {
				b.Elevation[e.Y*b.Width+e.X] = e.Value
				addSource(sources, b.ID, fmt.Sprintf("height edited at (%d,%d)", e.X, e.Y), []string{b.ID})
			}
		case EditMaterial:
			m, ok := FindMaterial(p, e.MaterialID)
			if !ok {
				return nil, &Error{Code: CodeSchema, Object: "edit", Field: "material_id", Message: fmt.Sprintf("edit %d references missing material %q", i, e.MaterialID)}
			}
			if !validFinite(e.Value) {
				return nil, &Error{Code: CodeParam, Object: "material:" + m.ID, Field: e.Field, Message: "material parameter must be finite"}
			}
			changed := false
			switch e.Field {
			case "normal_scale":
				if e.Value <= 0 {
					return nil, &Error{Code: CodeParam, Object: "material:" + m.ID, Field: "normal_scale", Message: "normal_scale must be positive"}
				}
				if m.NormalScale != e.Value {
					m.NormalScale = e.Value
					changed = true
				}
			case "rotation_deg":
				if m.RotationDeg != e.Value {
					m.RotationDeg = e.Value
					changed = true
				}
			default:
				return nil, &Error{Code: CodeParam, Object: "material:" + m.ID, Field: "field", Message: "supported fields are normal_scale and rotation_deg"}
			}
			if changed {
				for _, bid := range g.MaterialUsers[m.ID] {
					addSource(sources, bid, fmt.Sprintf("material %s.%s changed", m.ID, e.Field), []string{"material:" + m.ID, bid})
				}
			}
		case EditWeight:
			b, ok := FindBlock(p, e.BlockID)
			if !ok {
				return nil, &Error{Code: CodeSchema, Object: "edit", Field: "block_id", BlockID: e.BlockID, Message: fmt.Sprintf("edit %d references a missing block", i)}
			}
			if e.PixelX < 0 || e.PixelX >= b.Width || e.PixelY < 0 || e.PixelY >= b.Height || e.Layer < 0 || e.Layer >= len(b.Materials) {
				return nil, &Error{Code: CodeSchema, Object: "block:" + b.ID, Field: "weights", BlockID: b.ID, Message: "weight edit index is outside the block or layer range"}
			}
			if e.Weight < 0 || e.Weight > WeightQuantum {
				return nil, &Error{Code: CodeParam, Object: "block:" + b.ID, Field: "weights", BlockID: b.ID, Message: fmt.Sprintf("weight must be in [0,%d]", WeightQuantum)}
			}
			nLayers := len(b.Materials)
			off := (e.PixelY*b.Width + e.PixelX) * nLayers
			if b.Weights[off+e.Layer] != e.Weight {
				old := b.Weights[off+e.Layer]
				b.Weights[off+e.Layer] = e.Weight
				rebalanceWeights(b.Weights[off:off+nLayers], e.Layer, old-e.Weight)
				addSource(sources, b.ID, fmt.Sprintf("material weight edited at (%d,%d) layer %d", e.PixelX, e.PixelY, e.Layer), []string{b.ID})
			}
		default:
			return nil, &Error{Code: CodeParam, Object: "edit", Field: "kind", Message: fmt.Sprintf("unsupported edit kind %q", e.Kind)}
		}
	}
	if len(sources) == 0 {
		return nil, nil
	}
	return propagateInvalidation(g, sources), nil
}

func rebalanceWeights(weights []int, changedLayer, delta int) {
	if delta == 0 || len(weights) < 2 {
		return
	}
	// delta is oldWeight-newWeight. A positive delta means the edited layer
	// lost weight and the others must gain it; a negative delta means the
	// others must give weight back.
	if delta > 0 {
		idx := (changedLayer + 1) % len(weights)
		weights[idx] += delta
		return
	}
	remaining := -delta
	for remaining > 0 {
		idx := -1
		for i, w := range weights {
			if i == changedLayer || w == 0 {
				continue
			}
			if idx == -1 || w > weights[idx] {
				idx = i
			}
		}
		if idx < 0 {
			return
		}
		take := weights[idx]
		if take > remaining {
			take = remaining
		}
		weights[idx] -= take
		remaining -= take
	}
}

func addSource(sources map[string]invalidationSource, id, reason string, chain []string) {
	if _, exists := sources[id]; exists {
		return
	}
	sources[id] = invalidationSource{id: id, reason: reason, chain: append([]string(nil), chain...)}
}

func propagateInvalidation(g *DependencyGraph, sources map[string]invalidationSource) []InvalidationItem {
	seedIDs := make([]string, 0, len(sources))
	for id := range sources {
		seedIDs = append(seedIDs, id)
	}
	sort.Strings(seedIDs)
	type queueItem struct {
		id     string
		reason string
		chain  []string
	}
	queue := make([]queueItem, 0, len(seedIDs))
	seen := make(map[string]bool, len(seedIDs))
	for _, id := range seedIDs {
		s := sources[id]
		queue = append(queue, queueItem{id, s.reason, s.chain})
		seen[id] = true
	}
	var out []InvalidationItem
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		out = append(out, InvalidationItem{
			BlockID: item.id, Reason: item.reason, Chain: append([]string(nil), item.chain...),
		})
		for _, next := range g.SeamEdges[item.id] {
			if seen[next] {
				continue
			}
			seen[next] = true
			chain := append(append([]string(nil), item.chain...), next)
			queue = append(queue, queueItem{
				id: next, reason: fmt.Sprintf("downstream seam dependency via %s", item.id), chain: chain,
			})
		}
	}
	return out
}

// TopologicalOrder returns a deterministic SCC-aware order for the selected
// blocks. Seam dependencies are bilateral, so true cycles are condensed into
// single scheduling units; this is the correct topological interpretation of
// an undirected seam graph.
func (g *DependencyGraph) TopologicalOrder(selected []string) []string {
	want := make(map[string]bool, len(selected))
	for _, id := range selected {
		want[id] = true
	}
	nodes := make([]string, 0, len(want))
	for id := range want {
		nodes = append(nodes, id)
	}
	sort.Strings(nodes)
	if len(nodes) == 0 {
		return nil
	}
	index := 0
	indices := make(map[string]int)
	low := make(map[string]int)
	onStack := make(map[string]bool)
	var stack []string
	var comps [][]string
	var strongConnect func(string)
	strongConnect = func(v string) {
		indices[v] = index
		low[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range g.SeamEdges[v] {
			if !want[w] {
				continue
			}
			if _, ok := indices[w]; !ok {
				strongConnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] && indices[w] < low[v] {
				low[v] = indices[w]
			}
		}
		if low[v] == indices[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			sort.Strings(comp)
			comps = append(comps, comp)
		}
	}
	for _, n := range nodes {
		if _, ok := indices[n]; !ok {
			strongConnect(n)
		}
	}
	compOf := make(map[string]int)
	for i, c := range comps {
		for _, n := range c {
			compOf[n] = i
		}
	}
	indeg := make([]int, len(comps))
	deps := make([][]int, len(comps))
	for i := range comps {
		for _, n := range comps[i] {
			for _, w := range g.SeamEdges[n] {
				j, ok := compOf[w]
				if !ok || i == j {
					continue
				}
				deps[i] = append(deps[i], j)
			}
		}
	}
	for i := range deps {
		deps[i] = uniqueInts(deps[i])
		sort.Ints(deps[i])
		for _, j := range deps[i] {
			indeg[j]++
		}
	}
	ready := make([]int, 0, len(comps))
	for i := range comps {
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return comps[ready[i]][0] < comps[ready[j]][0] })
	var out []string
	for len(ready) > 0 {
		ci := ready[0]
		ready = ready[1:]
		out = append(out, comps[ci]...)
		for _, j := range deps[ci] {
			indeg[j]--
			if indeg[j] == 0 {
				ready = append(ready, j)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return comps[ready[i]][0] < comps[ready[j]][0] })
	}
	return out
}

func uniqueInts(in []int) []int {
	if len(in) == 0 {
		return nil
	}
	sort.Ints(in)
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// Retained for callers that want to validate edits without applying them.
func ValidateEdit(p *Project, e Edit) error {
	switch e.Kind {
	case EditHeight:
		b, ok := FindBlock(p, e.BlockID)
		if !ok {
			return &Error{Code: CodeSchema, Object: "edit", Field: "block_id", Message: "missing block"}
		}
		if e.X < 0 || e.X >= b.Width || e.Y < 0 || e.Y >= b.Height {
			return &Error{Code: CodeSchema, Object: "block:" + b.ID, Field: "heights", Message: "pixel outside block"}
		}
		if !validFinite(e.Value) {
			return &Error{Code: CodeParam, Object: "edit", Field: "value", Message: "non-finite height"}
		}
	case EditMaterial:
		if _, ok := FindMaterial(p, e.MaterialID); !ok {
			return &Error{Code: CodeSchema, Object: "edit", Field: "material_id", Message: "missing material"}
		}
		if !validFinite(e.Value) || math.IsNaN(e.Value) {
			return &Error{Code: CodeParam, Object: "edit", Field: "value", Message: "non-finite material parameter"}
		}
		if e.Field != "normal_scale" && e.Field != "rotation_deg" {
			return &Error{Code: CodeParam, Object: "edit", Field: "field", Message: "unsupported material field"}
		}
	default:
		return &Error{Code: CodeParam, Object: "edit", Field: "kind", Message: "unsupported edit kind"}
	}
	return nil
}

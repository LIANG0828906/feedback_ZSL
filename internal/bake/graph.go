package bake

import (
	"fmt"
	"sort"

	"terrainkernel/internal/model"
	"terrainkernel/internal/schema"
	"terrainkernel/internal/terrain"
)

type Node struct {
	Kind string
	ID   string
}

type Invalidation struct {
	ChunkID int
	LayerID string
	Reason  string
}

type Edit struct {
	LayerID    string
	Reason     string
	Before     *model.Layer
	After      *model.Layer
	NormBefore string
	NormAfter  string
}

type Graph struct {
	project     *model.Project
	downstream  map[Node][]Node
	chunkLayers map[int][]string
}

func (g *Graph) ChunkCount() int {
	if g == nil || g.project == nil {
		return 0
	}
	return g.project.ChunkCount()
}

func BuildGraph(p *model.Project) *Graph {
	g := &Graph{
		project:     p,
		downstream:  make(map[Node][]Node),
		chunkLayers: make(map[int][]string, p.ChunkCount()),
	}
	normalization := Node{Kind: "normalization", ID: p.NormMode}
	for id := 0; id < p.ChunkCount(); id++ {
		chunk := Node{Kind: "chunk", ID: fmt.Sprintf("%d", id)}
		composite := Node{Kind: "composite", ID: fmt.Sprintf("%d", id)}
		g.add(chunk, composite)
		g.add(normalization, composite)
		for i := range p.Layers {
			layer := &p.Layers[i]
			curve := Node{Kind: "curve", ID: layer.ID}
			params := Node{Kind: "params", ID: layer.ID}
			g.add(curve, chunk)
			g.add(params, chunk)
			g.add(chunk, composite)
			g.chunkLayers[id] = append(g.chunkLayers[id], layer.ID)
		}
		sort.Strings(g.chunkLayers[id])
	}
	return g
}

func (g *Graph) add(from, to Node) {
	for _, existing := range g.downstream[from] {
		if existing == to {
			return
		}
	}
	g.downstream[from] = append(g.downstream[from], to)
}

func (g *Graph) ChunkDependencies(chunkID int) []string {
	deps := []string{
		fmt.Sprintf("chunk:%d", chunkID),
		fmt.Sprintf("chunk:%d.composite", chunkID),
		"normalization:" + g.project.NormMode,
	}
	for _, layerID := range g.chunkLayers[chunkID] {
		deps = append(deps, "layer:"+layerID+":curve", "layer:"+layerID+":params")
	}
	sort.Strings(deps)
	return deps
}

func (g *Graph) LayersForChunk(chunkID int) []string {
	return append([]string(nil), g.chunkLayers[chunkID]...)
}

func (g *Graph) Propagate(edits []Edit) []Invalidation {
	seen := make(map[string]struct{})
	var out []Invalidation
	add := func(chunkID int, layerID, reason string) {
		key := fmt.Sprintf("%d\x00%s\x00%s", chunkID, layerID, reason)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, Invalidation{ChunkID: chunkID, LayerID: layerID, Reason: reason + " -> chunk weights -> composite"})
	}
	for _, edit := range edits {
		if edit.NormBefore != "" && edit.NormAfter != "" && edit.NormBefore != edit.NormAfter {
			for id := 0; id < g.project.ChunkCount(); id++ {
				add(id, "", "normalization changed")
			}
			continue
		}
		layerID := edit.LayerID
		if edit.Before != nil {
			layerID = edit.Before.ID
		}
		if edit.After != nil && layerID == "" {
			layerID = edit.After.ID
		}
		reason := edit.Reason
		if reason == "" {
			reason = "layer dependency changed"
		}
		for id := 0; id < g.project.ChunkCount(); id++ {
			affected := false
			if edit.Before != nil && edit.After != nil {
				affected = layerChangeAffectsChunk(g.project, id, edit.Before, edit.After)
			} else if edit.Before == nil && edit.After == nil && layerID == "" {
				affected = true
			} else {
				layer := g.project.LayerByID(layerID)
				if layer == nil {
					continue
				}
				affected = layerTouchesChunk(g.project, layer, id)
			}
			if affected {
				add(id, layerID, reason)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ChunkID == out[j].ChunkID {
			if out[i].LayerID == out[j].LayerID {
				return out[i].Reason < out[j].Reason
			}
			return out[i].LayerID < out[j].LayerID
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	return out
}

func InvalidationChunks(invalidations []Invalidation) []int {
	set := make(map[int]struct{})
	for _, inv := range invalidations {
		set[inv.ChunkID] = struct{}{}
	}
	ids := make([]int, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// TopoChunks returns deterministic dependency order. Chunks are independent,
// while each chunk's composite follows its weight node.
func (g *Graph) TopoChunks(ids []int) []int {
	out := append([]int(nil), ids...)
	sort.Ints(out)
	return out
}

func layerChangeAffectsChunk(p *model.Project, chunkID int, before, after *model.Layer) bool {
	originX, originY, ok := p.ChunkOrigin(chunkID)
	if !ok {
		return false
	}
	for y := 0; y < p.ChunkSizePx; y++ {
		for x := 0; x < p.ChunkSizePx; x++ {
			px, py := originX+x, originY+y
			height := terrain.HeightMM(p.Seed, px, py)
			slope := terrain.SlopeURad(p.Seed, px, py)
			if evalLayerRaw(before, height, slope, px, py) != evalLayerRaw(after, height, slope, px, py) {
				return true
			}
		}
	}
	return false
}

func evalLayerRaw(layer *model.Layer, height, slope int64, px, py int) int64 {
	if layer == nil {
		return 0
	}
	driverValue := height
	if layer.Curve != nil && layer.Curve.Driver == "slope" {
		driverValue = slope
	}
	value := layer.Constant
	if layer.Curve != nil {
		value = schema.EvalCurve(layer.Curve, driverValue)
	}
	if layer.Noise != nil {
		value += terrain.NoiseOffset(layer.Noise.Seed, layer.Noise.Amplitude, layer.Noise.ScaleMM, px, py)
	}
	if value < 0 {
		value = 0
	}
	if layer.SlopeCapURad > 0 && slope > layer.SlopeCapURad {
		value = 0
	}
	return value
}

func layerTouchesChunk(p *model.Project, layer *model.Layer, chunkID int) bool {
	if layer == nil || layer.Curve == nil || len(layer.Curve.Points) == 0 {
		return layer != nil
	}
	originX, originY, ok := p.ChunkOrigin(chunkID)
	if !ok {
		return false
	}
	minPoint, maxPoint := layer.Curve.Points[0].X, layer.Curve.Points[0].X
	for _, pt := range layer.Curve.Points[1:] {
		if pt.X < minPoint {
			minPoint = pt.X
		}
		if pt.X > maxPoint {
			maxPoint = pt.X
		}
	}
	var minDriver, maxDriver int64
	first := true
	for y := 0; y < p.ChunkSizePx; y++ {
		for x := 0; x < p.ChunkSizePx; x++ {
			var v int64
			if layer.Curve.Driver == "slope" {
				v = terrain.SlopeURad(p.Seed, originX+x, originY+y)
			} else {
				v = terrain.HeightMM(p.Seed, originX+x, originY+y)
			}
			if first {
				minDriver, maxDriver = v, v
				first = false
			} else {
				if v < minDriver {
					minDriver = v
				}
				if v > maxDriver {
					maxDriver = v
				}
			}
		}
	}
	return rangesOverlap(minDriver, maxDriver, minPoint, maxPoint)
}

func rangesOverlap(aMin, aMax, bMin, bMax int64) bool {
	return aMin <= bMax && bMin <= aMax
}

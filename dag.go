package vpf

import (
	"fmt"
	"sort"
)

type DependencyNode struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	LayerID    string `json:"layer_id,omitempty"`
	GroupID    string `json:"group_id,omitempty"`
	SourceID   string `json:"source_id,omitempty"`
	ChunkIndex int    `json:"chunk_index,omitempty"`
}

type DependencyGraph struct {
	FrameIndex   int                        `json:"frame_index"`
	Nodes        map[string]*DependencyNode `json:"nodes"`
	Dependencies map[string][]string        `json:"dependencies"`
	Dependents   map[string][]string        `json:"dependents"`
	ChunkIndices []int                      `json:"chunk_indices"`
	p            *Project
}

type InvalidationPlan struct {
	Changed        []string       `json:"changed"`
	VisitedNodes   []string       `json:"visited_nodes"`
	InvalidChunks  []int          `json:"invalid_chunks"`
	ReusableChunks []int          `json:"reusable_chunks"`
	ReasonByChunk  map[int]string `json:"reason_by_chunk"`
	UnknownChanges []string       `json:"unknown_changes,omitempty"`
}

func TilesForRect(p *Project, rect Rect) []int {
	canvas := Rect{X: 0, Y: 0, W: p.Width, H: p.Height}
	clipped := rect.Intersection(canvas)
	if clipped.W <= 0 || clipped.H <= 0 {
		return nil
	}
	cols, _ := p.TileGrid()
	firstCol := clipped.X / p.TileWidth
	lastCol := (clipped.X + clipped.W - 1) / p.TileWidth
	firstRow := clipped.Y / p.TileHeight
	lastRow := (clipped.Y + clipped.H - 1) / p.TileHeight
	var tiles []int
	for row := firstRow; row <= lastRow; row++ {
		for col := firstCol; col <= lastCol; col++ {
			tiles = append(tiles, row*cols+col)
		}
	}
	return tiles
}

func BuildDependencyGraph(p *Project, frameIndex int) (*DependencyGraph, error) {
	frame, ok := p.FrameByIndex(frameIndex)
	if !ok {
		return nil, schemaError(p.SourceVersion, "$.frames", "frame %d not found", frameIndex)
	}
	g := &DependencyGraph{
		FrameIndex: frameIndex, Nodes: map[string]*DependencyNode{},
		Dependencies: map[string][]string{}, Dependents: map[string][]string{}, p: p,
	}
	for i := 0; i < p.ChunkCount(); i++ {
		id := fmt.Sprintf("chunk:%d:%d", frameIndex, i)
		g.addNode(&DependencyNode{ID: id, Kind: "chunk_final", ChunkIndex: i})
		g.ChunkIndices = append(g.ChunkIndices, i)
	}

	groupNodes := map[string]Node{}
	var collectGroups func(Node)
	collectGroups = func(n Node) {
		if n.Kind == NodeGroup {
			groupNodes[n.ID] = n
		}
		for _, child := range n.Children {
			collectGroups(child)
		}
	}
	collectGroups(p.Root)

	for _, group := range groupNodes {
		groupID := "visibility:group:" + group.ID
		g.addNode(&DependencyNode{ID: groupID, Kind: "group_visibility", GroupID: group.ID})
		for _, layerID := range DescendantLayerIDs(group) {
			layerVis := "visibility:layer:" + layerID
			g.addNode(&DependencyNode{ID: layerVis, Kind: "layer_visibility", LayerID: layerID})
			g.addEdge(groupID, layerVis)
		}
	}

	for _, layer := range p.Layers {
		layerTiles := TilesForRect(p, layer.Rect)
		opacityID := "param:" + layer.ID + ":opacity"
		blendID := "param:" + layer.ID + ":blend"
		alphaID := "alpha:" + layer.ID + ":convention"
		layerVis := "visibility:layer:" + layer.ID
		g.addNode(&DependencyNode{ID: opacityID, Kind: "layer_param", LayerID: layer.ID})
		g.addNode(&DependencyNode{ID: blendID, Kind: "layer_param", LayerID: layer.ID})
		g.addNode(&DependencyNode{ID: alphaID, Kind: "alpha_convention", LayerID: layer.ID})
		g.addNode(&DependencyNode{ID: layerVis, Kind: "layer_visibility", LayerID: layer.ID})

		for _, chunkIndex := range layerTiles {
			layerChunkID := fmt.Sprintf("layerchunk:%d:%d:%s", frameIndex, chunkIndex, layer.ID)
			g.addNode(&DependencyNode{ID: layerChunkID, Kind: "layer_chunk", LayerID: layer.ID, ChunkIndex: chunkIndex})
			g.addEdge(opacityID, layerChunkID)
			g.addEdge(blendID, layerChunkID)
			g.addEdge(alphaID, layerChunkID)
			g.addEdge(layerVis, layerChunkID)
			g.addEdge(layerChunkID, fmt.Sprintf("chunk:%d:%d", frameIndex, chunkIndex))
		}

		if layer.Kind == KindRipple {
			sources := EffectiveRipples(p, layer, frame)
			for _, source := range sources {
				sourceID := "ripple:" + layer.ID + ":" + source.ID
				g.addNode(&DependencyNode{ID: sourceID, Kind: "ripple_source", LayerID: layer.ID, SourceID: source.ID})
				influenceRect := Rect{
					X: source.Center.X - source.Radius - 1,
					Y: source.Center.Y - source.Radius - 1,
					W: 2*source.Radius + 3,
					H: 2*source.Radius + 3,
				}
				for _, chunkIndex := range TilesForRect(p, influenceRect) {
					layerChunkID := fmt.Sprintf("layerchunk:%d:%d:%s", frameIndex, chunkIndex, layer.ID)
					g.addEdge(sourceID, layerChunkID)
				}
			}
		}
	}
	return g, nil
}

func (g *DependencyGraph) addNode(node *DependencyNode) {
	if _, exists := g.Nodes[node.ID]; !exists {
		g.Nodes[node.ID] = node
	}
}

func (g *DependencyGraph) addEdge(from, to string) {
	g.Dependents[from] = appendUnique(g.Dependents[from], to)
	g.Dependencies[to] = appendUnique(g.Dependencies[to], from)
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// Invalidate performs explicit downstream propagation from changed dependency IDs.
func (g *DependencyGraph) Invalidate(changed ...string) *InvalidationPlan {
	plan := &InvalidationPlan{Changed: changed, ReasonByChunk: map[int]string{}}
	queue := append([]string(nil), changed...)
	visited := map[string]bool{}
	reasonByNode := map[string]string{}
	for _, id := range changed {
		if _, ok := g.Nodes[id]; !ok {
			plan.UnknownChanges = append(plan.UnknownChanges, id)
			continue
		}
		reasonByNode[id] = id
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		plan.VisitedNodes = append(plan.VisitedNodes, id)
		if node := g.Nodes[id]; node != nil && node.Kind == "chunk_final" {
			if _, exists := plan.ReasonByChunk[node.ChunkIndex]; !exists {
				plan.ReasonByChunk[node.ChunkIndex] = reasonByNode[id]
			}
			plan.InvalidChunks = append(plan.InvalidChunks, node.ChunkIndex)
		}
		for _, next := range g.Dependents[id] {
			if _, ok := reasonByNode[next]; !ok {
				reasonByNode[next] = reasonByNode[id]
			}
			queue = append(queue, next)
		}
	}
	plan.InvalidChunks = sortedUniqueInts(plan.InvalidChunks)
	invalidSet := map[int]bool{}
	for _, index := range plan.InvalidChunks {
		invalidSet[index] = true
	}
	for _, index := range g.ChunkIndices {
		if !invalidSet[index] {
			plan.ReusableChunks = append(plan.ReusableChunks, index)
		}
	}
	sort.Ints(plan.ReusableChunks)
	sort.Strings(plan.VisitedNodes)
	sort.Strings(plan.UnknownChanges)
	return plan
}

func sortedUniqueInts(values []int) []int {
	set := map[int]bool{}
	for _, value := range values {
		set[value] = true
	}
	out := make([]int, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

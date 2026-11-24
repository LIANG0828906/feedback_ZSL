package engine

import "fixedsampler/internal/model"

type DependencyEdge struct {
	FromStage    int        `json:"from_stage"`
	FromChunk    int        `json:"from_chunk"`
	ToStage      int        `json:"to_stage"`
	ToChunk      int        `json:"to_chunk"`
	Plane        string     `json:"plane"`
	InputRegion  model.Rect `json:"input_region"`
	OutputRegion model.Rect `json:"output_region"`
	Reason       string     `json:"reason"`
	CacheKey     string     `json:"upstream_cache_key"`
}

type PlaneRegion struct {
	Plane string     `json:"plane"`
	Rect  model.Rect `json:"rect"`
}

type StageGraphNode struct {
	Seq              int           `json:"seq"`
	Type             string        `json:"type"`
	Planes           []string      `json:"planes"`
	PlaneRegions     []PlaneRegion `json:"plane_regions"`
	CoveredChunks    []int         `json:"covered_chunks"`
	DownstreamStages []int         `json:"downstream_stages"`
}

type DependencyGraph struct {
	ChunkCount int              `json:"chunk_count"`
	Stages     []StageGraphNode `json:"stages"`
	Edges      []DependencyEdge `json:"edges"`
}

func BuildGraph(p *model.Project, res *RunResult) *DependencyGraph {
	g := &DependencyGraph{ChunkCount: res.Grid.Count(), Edges: append([]DependencyEdge(nil), res.DependencyEdges...)}
	for seq := 1; seq <= len(p.Config); seq++ {
		stage := p.Config[seq-1]
		node := StageGraphNode{Seq: seq, Type: stage.Type}
		switch stage.Type {
		case "resample", "quantize":
			node.Planes = append([]string(nil), stage.Planes...)
		case "color":
			node.Planes = append(append([]string(nil), stage.InputNames...), stage.OutputNames...)
		}
		if planes := res.LevelPlanes[seq]; planes != nil {
			for _, name := range node.Planes {
				if rp := planes[name]; rp != nil {
					node.PlaneRegions = append(node.PlaneRegions, PlaneRegion{
						Plane: name, Rect: model.Rect{X1: rp.Width, Y1: rp.Height},
					})
				}
			}
		}
		for idx := 0; idx < res.Grid.Count(); idx++ {
			chunk := res.Artifacts[idx]
			if chunk == nil || seq >= len(chunk.Levels) {
				continue
			}
			level := chunk.Levels[seq]
			nonEmpty := false
			for _, tile := range level.Tiles {
				if !tile.Rect.Empty() {
					nonEmpty = true
					break
				}
			}
			if nonEmpty {
				node.CoveredChunks = append(node.CoveredChunks, idx)
			}
		}
		for later := seq + 1; later <= len(p.Config); later++ {
			node.DownstreamStages = append(node.DownstreamStages, later)
		}
		g.Stages = append(g.Stages, node)
	}
	return g
}

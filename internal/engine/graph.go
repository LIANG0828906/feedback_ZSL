package engine

import (
	"fmt"
	"sort"

	"atelier/internal/model"
)

type DependencyNode struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	ZoneID    string   `json:"zoneId,omitempty"`
	DependsOn []string `json:"dependsOn"`
}

type DependencyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type DependencyGraph struct {
	Nodes []DependencyNode `json:"nodes"`
	Edges []DependencyEdge `json:"edges"`
}

func (e *Engine) buildGraph() DependencyGraph {
	g := DependencyGraph{}
	add := func(id, kind, zoneID string, deps ...string) {
		sort.Strings(deps)
		g.Nodes = append(g.Nodes, DependencyNode{ID: id, Kind: kind, ZoneID: zoneID, DependsOn: deps})
		for _, dep := range deps {
			g.Edges = append(g.Edges, DependencyEdge{From: dep, To: id})
		}
	}
	add("terrain:raster", "terrain", "", "")
	add("config:sampling", "config", "", "terrain:raster")
	for _, ex := range e.model.Region.Exclusions {
		add("exclusion:"+ex.ID, "exclusion", "", "terrain:raster")
	}
	for _, zone := range e.zoneOrder {
		params := "zone:" + zone.ID + ":params"
		sampling := "zone:" + zone.ID + ":sampling"
		estimate := "zone:" + zone.ID + ":estimate"
		points := "zone:" + zone.ID + ":points"
		add(params, "zone-params", zone.ID, "terrain:raster")
		deps := []string{params, "config:sampling"}
		for _, ex := range e.zoneRelevantExclusions(zone) {
			deps = append(deps, "exclusion:"+ex.ID)
		}
		add(sampling, "stratified-sampling", zone.ID, deps...)
		add(estimate, "density-estimate", zone.ID, sampling, params)
		add(points, "reconstruction", zone.ID, estimate, params)
	}
	if e.heat != nil {
		tilesX := (e.heat.Width + e.heat.TileSize - 1) / e.heat.TileSize
		tilesY := (e.heat.Height + e.heat.TileSize - 1) / e.heat.TileSize
		for ty := 0; ty < tilesY; ty++ {
			for tx := 0; tx < tilesX; tx++ {
				id := fmt.Sprintf("heat:tile:%d:%d", tx, ty)
				zones := map[string]bool{}
				startRow, startCol := ty*e.heat.TileSize, tx*e.heat.TileSize
				endRow := minInt(startRow+e.heat.TileSize, e.heat.Height)
				endCol := minInt(startCol+e.heat.TileSize, e.heat.Width)
				for row := startRow; row < endRow; row++ {
					for col := startCol; col < endCol; col++ {
						if z := e.heat.ZoneIDs[row*e.heat.Width+col]; z != "" {
							zones[z] = true
						}
					}
				}
				var deps []string
				for z := range zones {
					deps = append(deps, "zone:"+z+":estimate")
				}
				sort.Strings(deps)
				add(id, "heatmap-tile", "", deps...)
			}
		}
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		return g.Edges[i].To < g.Edges[j].To
	})
	return g
}

func cacheTrace(states []model.ZoneState) []CacheTrace {
	out := make([]CacheTrace, 0, len(states))
	for _, state := range states {
		out = append(out, CacheTrace{
			Ordinal:     state.Ordinal,
			ZoneID:      state.ZoneID,
			SamplingKey: state.SamplingKey,
			EstimateKey: state.EstimateKey,
			PointsKey:   state.PointsKey,
			ContentHash: state.ContentHash,
		})
	}
	return out
}

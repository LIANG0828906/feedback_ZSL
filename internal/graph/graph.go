package graph

import (
	"sort"

	"terrainpatch/internal/model"
)

type Edge struct {
	BoundaryID string `json:"boundaryId"`
	From       string `json:"from"`
	To         string `json:"to"`
}

type Graph struct {
	Tiles        []string            `json:"tiles"`
	Edges        []Edge              `json:"edges"`
	Neighbors    map[string][]string `json:"neighbors"`
	Boundaries   map[string][]string `json:"boundaries"`
	Dependencies map[string][]string `json:"dependencies"`
}

func Build(p *model.Project) *Graph {
	g := &Graph{
		Tiles:        model.SortedTileIDs(p.Tiles),
		Neighbors:    map[string][]string{},
		Boundaries:   map[string][]string{},
		Dependencies: map[string][]string{},
	}
	edgeSet := map[string]map[string]bool{}
	for _, b := range p.Boundaries {
		a, c := b.SideA.TileID, b.SideB.TileID
		g.Edges = append(g.Edges, Edge{BoundaryID: b.ID, From: a, To: c})
		g.Boundaries[a] = append(g.Boundaries[a], b.ID)
		g.Boundaries[c] = append(g.Boundaries[c], b.ID)
		if edgeSet[a] == nil {
			edgeSet[a] = map[string]bool{}
		}
		if edgeSet[c] == nil {
			edgeSet[c] = map[string]bool{}
		}
		edgeSet[a][c] = true
		edgeSet[c][a] = true
	}
	for id, set := range edgeSet {
		for other := range set {
			g.Neighbors[id] = append(g.Neighbors[id], other)
		}
		sort.Strings(g.Neighbors[id])
		deps := append([]string{}, g.Neighbors[id]...)
		deps = append(deps, g.Boundaries[id]...)
		sort.Strings(deps)
		g.Dependencies[id] = deps
	}
	for _, id := range g.Tiles {
		sort.Strings(g.Boundaries[id])
	}
	return g
}

// Impact returns the edited tile and every adjacent tile whose shared boundary
// must be realigned. The graph is bidirectional; schedule orientation is only
// used to produce a stable deterministic order.
func (g *Graph) Impact(editedTile, field string) map[string]string {
	out := map[string]string{editedTile: "edited:" + field}
	for _, n := range g.Neighbors[editedTile] {
		boundaryID := ""
		for _, e := range g.Edges {
			if (e.From == editedTile && e.To == n) || (e.From == n && e.To == editedTile) {
				boundaryID = e.BoundaryID
				break
			}
		}
		out[n] = "realign:" + boundaryID
	}
	return out
}

func (g *Graph) Order(ids map[string]string) []string {
	order := make([]string, 0, len(ids))
	for id := range ids {
		order = append(order, id)
	}
	rank := map[string]int{}
	for i, id := range g.Tiles {
		rank[id] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		return rank[order[i]] < rank[order[j]]
	})
	return order
}

func (g *Graph) TopoOrderAll() []string {
	indeg := map[string]int{}
	adj := map[string][]string{}
	for _, e := range g.Edges {
		lo, hi := e.From, e.To
		if rankLess(g, hi, lo) {
			lo, hi = hi, lo
		}
		adj[lo] = append(adj[lo], hi)
		indeg[hi]++
	}
	available := []string{}
	for _, id := range g.Tiles {
		if indeg[id] == 0 {
			available = append(available, id)
		}
	}
	result := []string{}
	for len(available) > 0 {
		sort.SliceStable(available, func(i, j int) bool { return rankLess(g, available[i], available[j]) })
		id := available[0]
		available = available[1:]
		result = append(result, id)
		for _, next := range adj[id] {
			indeg[next]--
			if indeg[next] == 0 {
				available = append(available, next)
			}
		}
	}
	if len(result) != len(g.Tiles) {
		return append([]string{}, g.Tiles...)
	}
	return result
}

func rankLess(g *Graph, a, b string) bool {
	ra, rb := len(g.Tiles), len(g.Tiles)
	for i, id := range g.Tiles {
		if id == a {
			ra = i
		}
		if id == b {
			rb = i
		}
	}
	return ra < rb
}

// Package deps records explicit acoustic dependency edges and plans invalidation.
package deps

import (
	"fmt"
	"sort"
	"sync"

	"acousticfield/internal/acoustic"
)

type Edit struct {
	Kind        string `json:"kind"`
	ChunkID     string `json:"chunk_id"`
	BandID      int    `json:"band_id,omitempty"`
	SourceID    string `json:"source_id,omitempty"`
	SurfaceID   string `json:"surface_id,omitempty"`
	NewChunkID  string `json:"new_chunk_id,omitempty"`
	FromChunkID string `json:"from_chunk_id,omitempty"`
	ToChunkID   string `json:"to_chunk_id,omitempty"`
}

type edge struct {
	dependencyKey string
	chunkID       string
	reason        string
}

type Graph struct {
	mu           sync.Mutex
	byDependency map[string][]string
	downstream   map[string]map[string]string
}

func NewGraph() *Graph {
	return &Graph{byDependency: map[string][]string{}, downstream: map[string]map[string]string{}}
}

func (g *Graph) AddDependency(key, chunkID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.byDependency[key] = appendUnique(g.byDependency[key], chunkID)
}

func (g *Graph) AddDownstream(fromChunkID, toChunkID, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.downstream[fromChunkID] == nil {
		g.downstream[fromChunkID] = map[string]string{}
	}
	g.downstream[fromChunkID][toChunkID] = reason
	key := "boundary_transfer:" + fromChunkID + "->" + toChunkID
	g.byDependency[key] = appendUnique(g.byDependency[key], toChunkID)
}

func appendUnique(in []string, v string) []string {
	for _, x := range in {
		if x == v {
			return in
		}
	}
	return append(in, v)
}

// Plan returns the deterministic invalidation set for an edit. It never expands
// through chunks that have no boundary/downstream dependency edge.
func (g *Graph) Plan(edit Edit) []acoustic.Invalidation {
	g.mu.Lock()
	defer g.mu.Unlock()

	set := map[string]acoustic.Invalidation{}
	add := func(chunkID, reason, key string) {
		if chunkID == "" {
			return
		}
		if existing, ok := set[chunkID]; ok {
			if existing.Reason != reason {
				existing.Reason = existing.Reason + ";" + reason
				set[chunkID] = existing
			}
			return
		}
		set[chunkID] = acoustic.Invalidation{ChunkID: chunkID, Reason: reason, DependencyKey: key}
	}
	addDownstreams := func(from string) {
		for to, reason := range g.downstream[from] {
			add(to, reason, "boundary_transfer:"+from+"->"+to)
		}
	}

	switch edit.Kind {
	case "absorption":
		key := fmt.Sprintf("absorption:%s:%d", edit.ChunkID, edit.BandID)
		for _, id := range g.byDependency[key] {
			add(id, "absorption_table_changed", key)
			addDownstreams(id)
		}
		if _, ok := g.byDependency[key]; !ok {
			add(edit.ChunkID, "absorption_table_changed", key)
			addDownstreams(edit.ChunkID)
		}
	case "surface_geometry":
		key := "surface:" + edit.ChunkID + ":" + edit.SurfaceID + ":geometry"
		for _, id := range g.byDependency[key] {
			add(id, "reflection_geometry_changed", key)
			addDownstreams(id)
		}
		add(edit.ChunkID, "reflection_geometry_changed", key)
		addDownstreams(edit.ChunkID)
	case "source_emission":
		key := "source:" + edit.SourceID + ":emission"
		affected := g.byDependency[key]
		if len(affected) == 0 {
			affected = []string{edit.ChunkID}
		}
		for _, id := range affected {
			add(id, "source_emission_changed", key)
			addDownstreams(id)
		}
	case "source_position":
		key := "source:" + edit.SourceID + ":position"
		affected := g.byDependency[key]
		if len(affected) == 0 {
			affected = []string{edit.ChunkID}
		}
		for _, id := range affected {
			add(id, "source_position_changed", key)
			addDownstreams(id)
		}
		if edit.NewChunkID != "" && edit.NewChunkID != edit.ChunkID {
			add(edit.NewChunkID, "source_position_changed_new_chunk", key)
			addDownstreams(edit.NewChunkID)
		}
	case "boundary_transfer":
		key := "boundary_transfer:" + edit.FromChunkID + "->" + edit.ToChunkID
		for _, id := range g.byDependency[key] {
			add(id, "boundary_transfer_changed", key)
		}
		add(edit.ToChunkID, "boundary_transfer_changed", key)
	default:
		key := edit.Kind
		for _, id := range g.byDependency[key] {
			add(id, "dependency_changed", key)
		}
	}

	out := make([]acoustic.Invalidation, 0, len(set))
	for _, inv := range set {
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChunkID < out[j].ChunkID })
	return out
}

func BuildFromResults(results []acoustic.ChunkResult) *Graph {
	g := NewGraph()
	for _, r := range results {
		for _, dep := range r.Dependencies {
			g.AddDependency(dep, r.ChunkID)
		}
		for _, t := range r.Transfers {
			g.AddDownstream(t.FromChunkID, t.ToChunkID, "upstream_boundary_energy_changed")
		}
	}
	return g
}

package pipeline

import (
	"sort"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/model"
)

// Node kinds in the explicit zone dependency graph.
const (
	NodeZoneParams   = "zone_params"
	NodeNoCollect    = "nocollect"
	NodeSampleLayout = "sample_layout"
	NodeDensityEst   = "density_estimate"
	NodeReconstruct  = "reconstruction"
	NodeHeatTile     = "heat_tile"
)

// Edge is a directed dependency edge from upstream node to downstream node.
type Edge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	ZoneID int    `json:"zoneId"`
}

// DependencyGraph stores per-zone nodes and their directed edges.
type DependencyGraph struct {
	ZoneIDs []int  `json:"zoneIds"`
	Edges   []Edge `json:"edges"`
}

// BuildGraph constructs the fixed topology used by every region.
func BuildGraph(region *model.Region) DependencyGraph {
	graph := DependencyGraph{}
	for _, zone := range region.Zones {
		id := zone.ID
		graph.ZoneIDs = append(graph.ZoneIDs, id)
		graph.Edges = append(graph.Edges,
			Edge{NodeZoneParams, NodeSampleLayout, id},
			Edge{NodeNoCollect, NodeSampleLayout, id},
			Edge{NodeSampleLayout, NodeDensityEst, id},
			Edge{NodeZoneParams, NodeDensityEst, id},
			Edge{NodeDensityEst, NodeReconstruct, id},
			Edge{NodeNoCollect, NodeReconstruct, id},
			Edge{NodeReconstruct, NodeHeatTile, id},
		)
	}
	sort.Ints(graph.ZoneIDs)
	return graph
}

// Downstream returns the node set reachable from a changed node within one zone.
func (g DependencyGraph) Downstream(zoneID int, changedNode string) []string {
	adj := make(map[string][]string)
	for _, e := range g.Edges {
		if e.ZoneID == zoneID {
			adj[e.From] = append(adj[e.From], e.To)
		}
	}
	seen := map[string]bool{changedNode: true}
	queue := []string{changedNode}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range adj[node] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for node := range seen {
		out = append(out, node)
	}
	sort.Strings(out)
	return out
}

// Change describes one parameter edit applied to the region before rerun.
type Change struct {
	ZoneID int
	Node   string
	Reason string
}

// PlanInvalidation marks only zones reachable from the supplied changes. Any
// claimed invalidation that has no supporting dependency edge is rejected with
// E_DEP naming the zone and edge instead of silently widening the blast radius.
func (g DependencyGraph) PlanInvalidation(changes []Change) (map[int][]string, []model.InvalidationEntry, error) {
	affected := make(map[int]map[string]bool)
	reasons := make(map[int]string)
	for _, change := range changes {
		knownZone := false
		for _, id := range g.ZoneIDs {
			if id == change.ZoneID {
				knownZone = true
			}
		}
		if !knownZone {
			return nil, nil, errors.WithContext(errors.EDep,
				map[string]any{"zoneId": change.ZoneID, "edge": change.Node},
				"change targets unknown zone %d", change.ZoneID)
		}
		if !g.nodeExists(change.ZoneID, change.Node) {
			return nil, nil, errors.WithContext(errors.EDep,
				map[string]any{"zoneId": change.ZoneID, "edge": change.Node},
				"no dependency edge starts at node %s for zone %d", change.Node, change.ZoneID)
		}
		down := g.Downstream(change.ZoneID, change.Node)
		if affected[change.ZoneID] == nil {
			affected[change.ZoneID] = map[string]bool{}
		}
		for _, node := range down {
			affected[change.ZoneID][node] = true
		}
		if prior, ok := reasons[change.ZoneID]; ok {
			reasons[change.ZoneID] = prior + ";" + change.Reason
		} else {
			reasons[change.ZoneID] = change.Reason
		}
	}

	zoneInvalid := make(map[int][]string)
	entries := make([]model.InvalidationEntry, 0, len(g.ZoneIDs))
	for _, id := range g.ZoneIDs {
		nodes, hit := affected[id]
		if hit {
			list := make([]string, 0, len(nodes))
			for node := range nodes {
				list = append(list, node)
			}
			sort.Strings(list)
			zoneInvalid[id] = list
			entries = append(entries, model.InvalidationEntry{
				ZoneID: id, Reason: reasons[id], Reused: false,
				Edge: g.firstEdge(id),
			})
		} else {
			entries = append(entries, model.InvalidationEntry{
				ZoneID: id, Reason: "unchanged", Reused: true, Edge: g.firstEdge(id),
			})
		}
	}
	return zoneInvalid, entries, nil
}

func (g DependencyGraph) nodeExists(zoneID int, node string) bool {
	for _, e := range g.Edges {
		if e.ZoneID == zoneID && e.From == node {
			return true
		}
	}
	return false
}

func (g DependencyGraph) firstEdge(zoneID int) string {
	for _, e := range g.Edges {
		if e.ZoneID == zoneID {
			return e.From + "->" + e.To
		}
	}
	return ""
}

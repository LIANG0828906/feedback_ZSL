package lightprobe

import (
	"sort"
)

const (
	NodeRadiance = "probe_radiance"
	NodeSH       = "sh_coefficients"
	NodeRecon    = "probe_reconstruction"
	NodeBlend    = "cross_domain_blend"
	NodeQuery    = "interpolation_query"
	NodePixel    = "screen_pixel"
)

type GraphNode struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	RoomID      string `json:"room_id,omitempty"`
	ContentHash string `json:"content_hash"`
	CacheKey    string `json:"cache_key"`
}

type Invalidation struct {
	NodeID   string `json:"node_id"`
	NodeType string `json:"node_type"`
	Reason   string `json:"reason"`
}

type QueryDiff struct {
	Changed []string `json:"changed"`
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

type DependencyGraph struct {
	Nodes      map[string]*GraphNode        `json:"nodes"`
	Downstream map[string]map[string]string `json:"downstream"`
	Upstream   map[string]map[string]string `json:"upstream"`
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{
		Nodes:      map[string]*GraphNode{},
		Downstream: map[string]map[string]string{},
		Upstream:   map[string]map[string]string{},
	}
}

func (g *DependencyGraph) AddNode(n GraphNode) {
	cp := n
	g.Nodes[n.ID] = &cp
	if g.Downstream[n.ID] == nil {
		g.Downstream[n.ID] = map[string]string{}
	}
	if g.Upstream[n.ID] == nil {
		g.Upstream[n.ID] = map[string]string{}
	}
}

func (g *DependencyGraph) AddEdge(from, to, reason string) {
	if g.Nodes[from] == nil || g.Nodes[to] == nil {
		return
	}
	if g.Downstream[from] == nil {
		g.Downstream[from] = map[string]string{}
	}
	if g.Upstream[to] == nil {
		g.Upstream[to] = map[string]string{}
	}
	g.Downstream[from][to] = reason
	g.Upstream[to][from] = reason
}

func ComputeCacheKey(selfContent, roomID, resolutionVersion, paramVersion string, upstreamKeys []string) string {
	keys := append([]string(nil), upstreamKeys...)
	sort.Strings(keys)
	return canonicalHash("cache-key:", struct {
		Self              string   `json:"self"`
		RoomID            string   `json:"room_id"`
		ResolutionVersion string   `json:"resolution_version"`
		ParamVersion      string   `json:"param_version"`
		UpstreamKeys      []string `json:"upstream_keys"`
	}{selfContent, roomID, resolutionVersion, paramVersion, keys})
}

func (g *DependencyGraph) RecomputeKeys(scene *Scene) {
	var visit func(id string) string
	visit = func(id string) string {
		n := g.Nodes[id]
		if n == nil {
			return ""
		}
		if n.CacheKey != "" {
			return n.CacheKey
		}
		upIDs := make([]string, 0, len(g.Upstream[id]))
		for up := range g.Upstream[id] {
			upIDs = append(upIDs, up)
		}
		sort.Strings(upIDs)
		upKeys := make([]string, 0, len(upIDs))
		var room *Room
		if n.RoomID != "" {
			room = scene.Room(n.RoomID)
		}
		resVersion := ""
		if room != nil {
			resVersion = room.ResolutionVersion
		}
		for _, up := range upIDs {
			upKeys = append(upKeys, visit(up))
		}
		n.CacheKey = ComputeCacheKey(n.ContentHash, n.RoomID, resVersion, scene.Params.ParamVersion, upKeys)
		return n.CacheKey
	}
	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g.Nodes[id].CacheKey = ""
	}
	for _, id := range ids {
		_ = visit(id)
	}
}

func BuildDependencyGraph(scene *Scene, queryResults []QueryResult) *DependencyGraph {
	g := NewDependencyGraph()
	for _, p := range scene.Probes {
		g.AddNode(GraphNode{ID: "radiance:" + p.ID, Type: NodeRadiance, RoomID: p.RoomID, ContentHash: p.ContentHash})
		g.AddNode(GraphNode{ID: "sh:" + p.ID, Type: NodeSH, RoomID: p.RoomID, ContentHash: p.ContentHash + ":sh"})
		g.AddNode(GraphNode{ID: "recon:" + p.ID, Type: NodeRecon, RoomID: p.RoomID, ContentHash: p.ContentHash})
		g.AddEdge("radiance:"+p.ID, "recon:"+p.ID, "lattice-radiance")
		g.AddEdge("sh:"+p.ID, "recon:"+p.ID, "fixed-sh-reconstruction")
	}

	for _, qr := range queryResults {
		qid := qr.Query.ID
		queryNode := "query:" + qid
		g.AddNode(GraphNode{ID: queryNode, Type: NodeQuery, RoomID: qr.Query.PrimaryRoomID, ContentHash: canonicalHash("query:", qr.Query)})
		if qr.Neighbor != nil {
			blendID := "blend:" + qid
			g.AddNode(GraphNode{ID: blendID, Type: NodeBlend, RoomID: qr.Query.PrimaryRoomID,
				ContentHash: canonicalHash("blend:", struct {
					Beta  float64 `json:"beta"`
					Query string  `json:"query"`
				}{qr.BlendRatioQuantized, qid})})
			for _, t := range qr.Primary.Terms {
				g.AddEdge("recon:"+t.ProbeID, blendID, "primary-trilinear-corner")
			}
			for _, t := range qr.Neighbor.Terms {
				g.AddEdge("recon:"+t.ProbeID, blendID, "neighbor-trilinear-corner")
			}
			g.AddEdge(blendID, queryNode, "cross-domain-blend")
		} else {
			for _, t := range qr.Primary.Terms {
				g.AddEdge("recon:"+t.ProbeID, queryNode, "trilinear-corner")
			}
		}
		pixelID := "pixel:" + qr.Query.PixelID
		g.AddNode(GraphNode{ID: pixelID, Type: NodePixel, ContentHash: qr.Query.PixelID})
		g.AddEdge(queryNode, pixelID, "camera-ray")
	}
	for _, px := range scene.Pixels {
		pixelID := "pixel:" + px.ID
		if g.Nodes[pixelID] == nil {
			g.AddNode(GraphNode{ID: pixelID, Type: NodePixel, ContentHash: px.ID})
		}
		queryID := "query:" + px.QueryID
		g.AddEdge(queryID, pixelID, "screen-raster")
	}
	g.RecomputeKeys(scene)
	return g
}

func (g *DependencyGraph) InvalidateFrom(startID, reason string) []Invalidation {
	if g.Nodes[startID] == nil {
		return nil
	}
	invalidated := map[string]Invalidation{
		startID: {NodeID: startID, NodeType: g.Nodes[startID].Type, Reason: reason},
	}
	queue := []string{startID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		children := make([]string, 0, len(g.Downstream[current]))
		for child := range g.Downstream[current] {
			children = append(children, child)
		}
		sort.Strings(children)
		for _, child := range children {
			edgeReason := g.Downstream[current][child]
			if _, ok := invalidated[child]; ok {
				continue
			}
			invalidated[child] = Invalidation{
				NodeID: child, NodeType: g.Nodes[child].Type,
				Reason: "depends-on " + current + " via " + edgeReason,
			}
			queue = append(queue, child)
		}
	}
	out := make([]Invalidation, 0, len(invalidated))
	for _, inv := range invalidated {
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

func (g *DependencyGraph) InvalidateProbeRadiance(probeID string) []Invalidation {
	return g.InvalidateFrom("radiance:"+probeID, "radiance-content-changed")
}

func (g *DependencyGraph) InvalidateRoomResolution(roomID, newResolutionVersion string) []Invalidation {
	var roots []string
	for id, n := range g.Nodes {
		if n.RoomID == roomID && (n.Type == NodeRadiance || n.Type == NodeSH || n.Type == NodeRecon) {
			roots = append(roots, id)
		}
	}
	sort.Strings(roots)
	seen := map[string]Invalidation{}
	for _, root := range roots {
		for _, inv := range g.InvalidateFrom(root, "resolution-version-changed:"+newResolutionVersion) {
			seen[inv.NodeID] = inv
		}
	}
	out := make([]Invalidation, 0, len(seen))
	for _, inv := range seen {
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

func DiffQueryTrajectory(oldQueries, newQueries []Query) QueryDiff {
	oldMap := map[string]Query{}
	newMap := map[string]Query{}
	for _, q := range oldQueries {
		oldMap[q.ID] = q
	}
	for _, q := range newQueries {
		newMap[q.ID] = q
	}
	diff := QueryDiff{}
	for id, nq := range newMap {
		oq, ok := oldMap[id]
		if !ok {
			diff.Added = append(diff.Added, id)
			continue
		}
		if canonicalHash("query:", oq) != canonicalHash("query:", nq) {
			diff.Changed = append(diff.Changed, id)
		}
	}
	for id := range oldMap {
		if _, ok := newMap[id]; !ok {
			diff.Removed = append(diff.Removed, id)
		}
	}
	sort.Strings(diff.Changed)
	sort.Strings(diff.Added)
	sort.Strings(diff.Removed)
	return diff
}

func (g *DependencyGraph) InvalidateQueryDiff(diff QueryDiff) []Invalidation {
	seen := map[string]Invalidation{}
	for _, id := range diff.Changed {
		for _, inv := range g.InvalidateFrom("query:"+id, "query-trajectory-content-changed") {
			seen[inv.NodeID] = inv
		}
	}
	for _, id := range diff.Removed {
		for _, inv := range g.InvalidateFrom("query:"+id, "query-removed-from-trajectory") {
			seen[inv.NodeID] = inv
		}
	}
	out := make([]Invalidation, 0, len(seen))
	for _, inv := range seen {
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

type CacheReconciliation struct {
	Reused  []string          `json:"reused"`
	Rebuilt []string          `json:"rebuilt"`
	NewKeys map[string]string `json:"new_keys"`
}

func (g *DependencyGraph) ReconcileCache(previousKeys map[string]string) CacheReconciliation {
	out := CacheReconciliation{NewKeys: map[string]string{}}
	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		key := g.Nodes[id].CacheKey
		out.NewKeys[id] = key
		if previousKeys != nil && previousKeys[id] == key {
			out.Reused = append(out.Reused, id)
		} else {
			out.Rebuilt = append(out.Rebuilt, id)
		}
	}
	return out
}

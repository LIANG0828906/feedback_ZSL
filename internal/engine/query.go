package engine

import (
	"sort"

	"graphlab/internal/model"
)

const (
	LayoutIndex = "index"
	LayoutScan  = "scan"
)

type adjacency struct {
	out map[string][]model.Edge
}

func buildAdjacency(p *model.Project) adjacency {
	edges := append([]model.Edge(nil), p.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].DeclOrder == edges[j].DeclOrder {
			return edges[i].ID < edges[j].ID
		}
		return edges[i].DeclOrder < edges[j].DeclOrder
	})
	out := make(map[string][]model.Edge)
	for _, e := range edges {
		out[e.From] = append(out[e.From], e)
		if !e.Directed {
			out[e.To] = append(out[e.To], e)
		}
	}
	return adjacency{out: out}
}

func (a adjacency) outgoing(id string) []model.Edge { return a.out[id] }

type evalContext struct {
	project    *model.Project
	indexes    *IndexStore
	layout     string
	vertexRank map[string]int
	adj        adjacency
	candidates map[string]bool
	probeKeys  []string
	depSet     map[string]bool
}

func newEvalContext(project *model.Project, indexes *IndexStore, layout string, labels []string) *evalContext {
	ctx := &evalContext{
		project:    project,
		indexes:    indexes,
		layout:     layout,
		vertexRank: make(map[string]int, len(project.Vertices)),
		adj:        buildAdjacency(project),
		candidates: make(map[string]bool),
		depSet:     make(map[string]bool),
	}
	for i, v := range project.Vertices {
		ctx.vertexRank[v.ID] = i
	}
	ctx.probeKeys = make([]string, 0, len(labels))
	for _, label := range labels {
		ctx.probeKeys = append(ctx.probeKeys, LabelKey(label))
	}
	if ctx.layout == LayoutIndex {
		ctx.candidates = ctx.indexCandidates(labels)
	} else {
		for _, v := range project.Vertices {
			if hasAllLabels(v, labels) {
				ctx.candidates[v.ID] = true
			}
		}
	}
	return ctx
}

func (ctx *evalContext) indexCandidates(labels []string) map[string]bool {
	out := make(map[string]bool)
	if len(labels) == 0 {
		for _, v := range ctx.project.Vertices {
			out[v.ID] = true
		}
		return out
	}
	var seed map[string]bool
	for _, label := range labels {
		ik := ctx.indexes.Keys[LabelKey(label)]
		entities := []string(nil)
		if ik != nil {
			entities = ik.Entities
		}
		next := make(map[string]bool)
		for _, ent := range entities {
			if len(ent) > 2 && ent[:2] == "v:" {
				id := ent[2:]
				if seed == nil || seed[id] {
					next[id] = true
				}
			}
		}
		seed = next
	}
	// Intersect with a direct pass so the result remains correct even if an
	// index has not yet been populated (for example during a recovery probe).
	for id := range seed {
		if _, ok := ctx.vertexByID(id); ok {
			out[id] = true
		}
	}
	// Labels are an intersection condition. The index union above may contain
	// false positives when a key is shared across shards; validate explicitly.
	for id := range out {
		if !hasAllLabelsByID(ctx.project, id, labels) {
			delete(out, id)
		}
	}
	return out
}

func (ctx *evalContext) vertexByID(id string) (model.Vertex, bool) {
	for _, v := range ctx.project.Vertices {
		if v.ID == id {
			return v, true
		}
	}
	return model.Vertex{}, false
}

func hasAllLabels(v model.Vertex, labels []string) bool {
	set := make(map[string]bool, len(v.Labels))
	for _, l := range v.Labels {
		set[l] = true
	}
	for _, l := range labels {
		if !set[l] {
			return false
		}
	}
	return true
}

func hasAllLabelsByID(p *model.Project, id string, labels []string) bool {
	for _, v := range p.Vertices {
		if v.ID == id {
			return hasAllLabels(v, labels)
		}
	}
	return false
}

func (ctx *evalContext) addVertexDeps(id string) {
	if key := VertexIdentityKey(id); key != "" {
		ctx.depSet[key] = true
	}
	for _, v := range ctx.project.Vertices {
		if v.ID == id {
			ctx.depSet[ShardDependency(v.ShardID)] = true
			break
		}
	}
}

func (ctx *evalContext) addEdgeDeps(e model.Edge) {
	ctx.depSet[EdgeIdentityKey(e.ID)] = true
	ctx.depSet[ShardDependency(e.ShardID)] = true
}

func (ctx *evalContext) addProbeDeps() {
	for _, key := range ctx.probeKeys {
		ctx.depSet[key] = true
	}
}

func (ctx *evalContext) dependencies() []string {
	out := make([]string, 0, len(ctx.depSet))
	for dep := range ctx.depSet {
		out = append(out, dep)
	}
	sort.Strings(out)
	return out
}

func (ctx *evalContext) sortedResults(ids []string, start string, labels []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool)
	for _, id := range ids {
		if id == start || seen[id] || !ctx.matchesLabels(id, labels) {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := ctx.vertexRank[out[i]], ctx.vertexRank[out[j]]
		if ri == rj {
			return out[i] < out[j]
		}
		return ri < rj
	})
	return out
}

func (ctx *evalContext) indexTrace(extra ...string) []string {
	trace := append([]string(nil), ctx.probeKeys...)
	trace = append(trace, extra...)
	return sortedUnique(trace)
}

func (ctx *evalContext) matchesLabels(id string, labels []string) bool {
	if ctx.layout == LayoutIndex {
		return ctx.candidates[id]
	}
	return hasAllLabelsByID(ctx.project, id, labels)
}

func evaluateQuery(p *model.Project, indexes *IndexStore, q model.Query, layout string) model.QueryResult {
	if layout != LayoutIndex && layout != LayoutScan {
		layout = LayoutIndex
	}
	ctx := newEvalContext(p, indexes, layout, q.Labels)
	res := model.QueryResult{
		QueryID: q.ID, CacheID: q.CacheID(), ProjectVersion: p.Version,
		VisitedVertices: []string{}, VisitedEdges: []string{},
		ResultVertices: []string{}, IndexKeys: ctx.indexTrace(),
	}
	ctx.addProbeDeps()
	startExists := false
	for _, v := range p.Vertices {
		if v.ID == q.Start {
			startExists = true
			break
		}
	}
	if !startExists {
		res.Status = model.OutcomeStartMissing
		ctx.depSet[VertexIdentityKey(q.Start)] = true
		res.Dependencies = ctx.dependencies()
		return res
	}
	if q.MaxDepth == 0 {
		res.Status = model.OutcomeDepthZero
		res.VisitedVertices = []string{q.Start}
		ctx.addVertexDeps(q.Start)
		res.Dependencies = ctx.dependencies()
		return res
	}

	visited := map[string]bool{q.Start: true}
	distance := map[string]int{q.Start: 0}
	frontier := []string{q.Start}
	res.VisitedVertices = []string{q.Start}
	ctx.addVertexDeps(q.Start)
	parentVertex := make(map[string]string)
	parentEdge := make(map[string]string)
	foundTarget := q.Target == q.Start && ctx.matchesLabels(q.Start, q.Labels)
	shortest := q.Kind == model.QueryShortestPath

	for depth := 0; depth < q.MaxDepth && len(frontier) > 0 && !foundTarget; depth++ {
		next := make([]string, 0)
		for _, vertexID := range frontier {
			outgoing := ctx.adj.outgoing(vertexID)
			if outKey := OutKey(vertexID); outKey != "" {
				ctx.depSet[outKey] = true
			}
			for _, e := range outgoing {
				ctx.addEdgeDeps(e)
				other := e.To
				if e.From != vertexID {
					other = e.From
				}
				res.VisitedEdges = append(res.VisitedEdges, e.ID)
				if shortest && other == q.Target && ctx.matchesLabels(q.Target, q.Labels) {
					parentVertex[other] = vertexID
					parentEdge[other] = e.ID
					foundTarget = true
					break
				}
				if visited[other] {
					if shortest && other == q.Target && ctx.matchesLabels(q.Target, q.Labels) {
						parentVertex[other] = vertexID
						parentEdge[other] = e.ID
						foundTarget = true
						break
					}
					continue
				}
				visited[other] = true
				distance[other] = depth + 1
				parentVertex[other] = vertexID
				parentEdge[other] = e.ID
				res.VisitedVertices = append(res.VisitedVertices, other)
				ctx.addVertexDeps(other)
				next = append(next, other)
			}
			if shortest && foundTarget {
				break
			}
		}
		frontier = next
	}
	if shortest {
		if q.Target == q.Start && ctx.matchesLabels(q.Start, q.Labels) {
			res.Status = model.OutcomeOK
			res.PathVertices = []string{q.Start}
			res.PathEdges = []string{}
		} else if foundTarget {
			res.Status = model.OutcomeOK
			cur := q.Target
			res.PathVertices = []string{cur}
			for cur != q.Start {
				eid, ok := parentEdge[cur]
				if !ok {
					break
				}
				res.PathEdges = append(res.PathEdges, eid)
				cur = parentVertex[cur]
				res.PathVertices = append(res.PathVertices, cur)
			}
			reverseStrings(res.PathVertices)
			reverseStrings(res.PathEdges)
			res.ResultVertices = []string{q.Target}
		} else {
			res.Status = model.OutcomeTargetUnreachable
		}
	} else {
		res.Status = model.OutcomeOK
		res.ResultVertices = ctx.sortedResults(res.VisitedVertices, q.Start, q.Labels)
	}
	res.Dependencies = ctx.dependencies()
	return res
}

func reverseStrings(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

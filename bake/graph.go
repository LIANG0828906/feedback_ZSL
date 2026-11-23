package bake

import (
	"sort"

	"terrain-bake/project"
)

type DependencyEdge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	SeamID   string `json:"seamId"`
	Normal   bool   `json:"normal"`
	Material bool   `json:"material"`
}

type InvalidationEntry struct {
	BlockID string   `json:"blockId"`
	Reason  string   `json:"reason"`
	Path    []string `json:"path"`
	Depth   int      `json:"depth"`
}

type InvalidationReport struct {
	Source      string              `json:"source"`
	ChangeKind  string              `json:"changeKind"`
	Affected    []InvalidationEntry `json:"affected"`
	HitBlocks   []string            `json:"hitBlocks"`
	Constraints []DependencyEdge    `json:"constraints"`
}

// Graph is the explicit seam constraint graph. Every seam produces two
// invalidation edges because both blocks constrain each other. The graph is
// cyclic for connected terrain components; deterministic bake ordering is
// defined over the condensation of strongly connected components.
type Graph struct {
	Nodes     []string
	order     map[string]int
	adj       map[string][]DependencyEdge
	seams     map[string][]project.Seam
	allSeams  []project.Seam
	blockDeps map[string][]string
}

func NewGraph(p *project.Project) (*Graph, *project.Error) {
	g := &Graph{
		Nodes:     append([]string(nil), p.BlockOrder...),
		order:     make(map[string]int, len(p.BlockOrder)),
		adj:       make(map[string][]DependencyEdge, len(p.BlockOrder)),
		seams:     make(map[string][]project.Seam, len(p.BlockOrder)),
		blockDeps: make(map[string][]string, len(p.BlockOrder)),
	}
	for i, id := range p.BlockOrder {
		g.order[id] = i
		g.adj[id] = nil
	}
	for _, seam := range p.Manifest.Seams {
		if _, ok := g.order[seam.A]; !ok {
			return nil, project.NewError(project.CodeSchema, seam.A, "seam.a", "unknown block")
		}
		if _, ok := g.order[seam.B]; !ok {
			return nil, project.NewError(project.CodeSchema, seam.B, "seam.b", "unknown block")
		}
		g.seams[seam.A] = append(g.seams[seam.A], seam)
		g.seams[seam.B] = append(g.seams[seam.B], seam)
		g.adj[seam.A] = append(g.adj[seam.A], DependencyEdge{From: seam.A, To: seam.B, SeamID: seam.ID, Normal: true, Material: true})
		g.adj[seam.B] = append(g.adj[seam.B], DependencyEdge{From: seam.B, To: seam.A, SeamID: seam.ID, Normal: true, Material: true})
		g.allSeams = append(g.allSeams, seam)
	}
	for id := range g.adj {
		sort.Slice(g.adj[id], func(i, j int) bool {
			if g.order[g.adj[id][i].To] != g.order[g.adj[id][j].To] {
				return g.order[g.adj[id][i].To] < g.order[g.adj[id][j].To]
			}
			return g.adj[id][i].SeamID < g.adj[id][j].SeamID
		})
	}
	for id := range g.adj {
		g.blockDeps[id] = g.Dependencies(id)
	}
	for id := range g.seams {
		sort.Slice(g.seams[id], func(i, j int) bool { return g.seams[id][i].ID < g.seams[id][j].ID })
	}
	sort.Slice(g.allSeams, func(i, j int) bool { return g.allSeams[i].ID < g.allSeams[j].ID })
	return g, nil
}

func (g *Graph) Dependencies(id string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, e := range g.adj[id] {
		if !seen[e.To] {
			seen[e.To] = true
			out = append(out, e.To)
		}
	}
	sort.Slice(out, func(i, j int) bool { return g.order[out[i]] < g.order[out[j]] })
	return out
}

func (g *Graph) BlockDependencies(id string) []string {
	return append([]string(nil), g.blockDeps[id]...)
}

func (g *Graph) SeamsForBlock(id string) []project.Seam {
	return append([]project.Seam(nil), g.seams[id]...)
}

func (g *Graph) AllSeams() []project.Seam {
	return append([]project.Seam(nil), g.allSeams...)
}

func (g *Graph) Constraints() []DependencyEdge {
	var out []DependencyEdge
	for _, id := range g.Nodes {
		out = append(out, g.adj[id]...)
	}
	return out
}

func (g *Graph) Invalidate(source, changeKind string) *InvalidationReport {
	report := &InvalidationReport{
		Source:      source,
		ChangeKind:  changeKind,
		Constraints: g.Constraints(),
	}
	if _, ok := g.order[source]; !ok {
		return report
	}
	type visit struct {
		id    string
		path  []string
		depth int
	}
	visited := map[string]bool{source: true}
	queue := []visit{{id: source, path: []string{source}}}
	entries := map[string]InvalidationEntry{
		source: {BlockID: source, Reason: "edit:" + changeKind, Path: []string{source}, Depth: 0},
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range g.adj[cur.id] {
			if visited[e.To] {
				continue
			}
			visited[e.To] = true
			path := append(append([]string(nil), cur.path...), e.To)
			reason := "seam-normal:" + e.SeamID
			if changeKind == "material" {
				reason = "seam-material:" + e.SeamID
			}
			entries[e.To] = InvalidationEntry{BlockID: e.To, Reason: reason, Path: path, Depth: cur.depth + 1}
			queue = append(queue, visit{id: e.To, path: path, depth: cur.depth + 1})
		}
	}
	for id, entry := range entries {
		_ = id
		report.Affected = append(report.Affected, entry)
	}
	sort.Slice(report.Affected, func(i, j int) bool {
		return g.order[report.Affected[i].BlockID] < g.order[report.Affected[j].BlockID]
	})
	for _, id := range g.Nodes {
		if !visited[id] {
			report.HitBlocks = append(report.HitBlocks, id)
		}
	}
	return report
}

// Order returns a stable dependency-condensation order for the requested
// nodes. Blocks in one strongly connected seam component are ranked by their
// manifest order; independent components are topologically sorted.
func (g *Graph) Order(ids []string) []string {
	inSet := make(map[string]bool, len(ids))
	ordered := make([]string, 0, len(ids))
	for _, id := range g.Nodes {
		if contains(ids, id) {
			inSet[id] = true
			ordered = append(ordered, id)
		}
	}
	if len(ordered) == 0 {
		return nil
	}

	type comp struct {
		index int
		nodes []string
	}
	var comps []comp
	index := 0
	indices := make(map[string]int)
	low := make(map[string]int)
	onStack := make(map[string]bool)
	var stack []string
	var visit func(string)
	visit = func(v string) {
		indices[v] = index
		low[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, e := range g.adj[v] {
			if !inSet[e.To] {
				continue
			}
			if _, seen := indices[e.To]; !seen {
				visit(e.To)
				if low[e.To] < low[v] {
					low[v] = low[e.To]
				}
			} else if onStack[e.To] && indices[e.To] < low[v] {
				low[v] = indices[e.To]
			}
		}
		if low[v] == indices[v] {
			var members []string
			for {
				n := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[n] = false
				members = append(members, n)
				if n == v {
					break
				}
			}
			sort.Slice(members, func(i, j int) bool { return g.order[members[i]] < g.order[members[j]] })
			comps = append(comps, comp{nodes: members})
		}
	}
	for _, id := range ordered {
		if _, seen := indices[id]; !seen {
			visit(id)
		}
	}

	compOf := make(map[string]int, len(ordered))
	for ci := range comps {
		for _, n := range comps[ci].nodes {
			compOf[n] = ci
		}
	}
	type edge struct{ from, to int }
	edges := map[edge]bool{}
	indeg := make([]int, len(comps))
	out := make([][]int, len(comps))
	for _, from := range ordered {
		for _, e := range g.adj[from] {
			if !inSet[e.To] {
				continue
			}
			c1, c2 := compOf[from], compOf[e.To]
			if c1 == c2 {
				continue
			}
			key := edge{c1, c2}
			if edges[key] {
				continue
			}
			edges[key] = true
			out[c1] = append(out[c1], c2)
			indeg[c2]++
		}
	}
	ready := make([]int, 0, len(comps))
	for i := range comps {
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	sortReady := func(a []int) {
		sort.Slice(a, func(i, j int) bool {
			return g.order[comps[a[i]].nodes[0]] < g.order[comps[a[j]].nodes[0]]
		})
	}
	sortReady(ready)
	var result []string
	for len(ready) > 0 {
		c := ready[0]
		ready = ready[1:]
		result = append(result, comps[c].nodes...)
		for _, next := range out[c] {
			indeg[next]--
			if indeg[next] == 0 {
				ready = append(ready, next)
			}
		}
		sortReady(ready)
	}
	return result
}

func (g *Graph) OrderAll() []string {
	return g.Order(g.Nodes)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

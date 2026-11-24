package editorpick

import "sort"

type DependencyEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type Change struct {
	ObjectID string
	Reason   string
}

type InvalidObject struct {
	ObjectID string `json:"object_id"`
	Kind     string `json:"kind"`
	Reason   string `json:"reason"`
}

type InvalidationReport struct {
	Revision int64            `json:"revision"`
	Objects  []InvalidObject  `json:"objects"`
	Edges    []DependencyEdge `json:"-"`
}

type DepGraph struct {
	Edges []DependencyEdge `json:"edges"`
	adj   map[string]map[string]string
}

func NewDepGraph() *DepGraph {
	return &DepGraph{adj: make(map[string]map[string]string)}
}

func (g *DepGraph) AddEdge(from, to, reason string) {
	if g.adj == nil {
		g.adj = make(map[string]map[string]string)
	}
	if _, ok := g.adj[from]; !ok {
		g.adj[from] = make(map[string]string)
	}
	// First reason is stable for the deterministic graph construction.
	if _, exists := g.adj[from][to]; !exists {
		g.adj[from][to] = reason
		g.Edges = append(g.Edges, DependencyEdge{From: from, To: to, Reason: reason})
	}
}

func (g *DepGraph) Invalidate(changed ...string) []InvalidObject {
	changes := make([]Change, 0, len(changed))
	for _, id := range changed {
		changes = append(changes, Change{ObjectID: id, Reason: "source-change"})
	}
	return g.InvalidateChanges(changes)
}

func (g *DepGraph) InvalidateChanges(changes []Change) []InvalidObject {
	type queueItem struct {
		id     string
		reason string
	}
	visited := make(map[string]map[string]string)
	queue := make([]queueItem, 0, len(changes))
	for _, change := range changes {
		if visited[change.ObjectID] == nil {
			visited[change.ObjectID] = make(map[string]string)
		}
		reason := change.Reason
		if reason == "" {
			reason = "source-change"
		}
		if _, ok := visited[change.ObjectID][reason]; !ok {
			visited[change.ObjectID][reason] = reason
			queue = append(queue, queueItem{id: change.ObjectID, reason: reason})
		}
	}
	for head := 0; head < len(queue); head++ {
		item := queue[head]
		for dependent, reason := range g.adj[item.id] {
			if visited[dependent] == nil {
				visited[dependent] = make(map[string]string)
			}
			if _, seen := visited[dependent][reason]; !seen {
				visited[dependent][reason] = reason
				queue = append(queue, queueItem{id: dependent, reason: reason})
			}
		}
	}
	out := make([]InvalidObject, 0)
	for id, reasons := range visited {
		for reason := range reasons {
			out = append(out, InvalidObject{ObjectID: id, Kind: objectKind(id), Reason: reason})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ObjectID != out[j].ObjectID {
			return out[i].ObjectID < out[j].ObjectID
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

func objectKind(id string) string {
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			return id[:i]
		}
	}
	return "object"
}

func localObject(id string) string  { return "local:" + id }
func worldObject(id string) string  { return "world:" + id }
func geometryObject(i int) string   { return "geometry:" + itoa(i) }
func entryObject(id string) string  { return "entry:" + id }
func queryObject(key string) string { return "query:" + key }
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

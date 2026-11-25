package engine

import (
	"fmt"
	"sort"

	"replayaudit/internal/model"
)

// Node kinds used in the explicit time dependency graph.
const (
	NodeEvent   = "event"
	NodeFlag    = "flag"
	NodeMutex   = "mutex"
	NodeSync    = "sync"
	NodeSegment = "segment"
)

// ChangeItem is one row of the invalidation manifest.
type ChangeItem struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Reason      string `json:"reason,omitempty"`
	Invalidated bool   `json:"invalidated"`
	Reused      bool   `json:"reused"`
}

// Graph is an explicit directed dependency graph. Edges point from a
// dependency to its dependent (propagation direction).
type Graph struct {
	Nodes map[string]string // namespaced id -> kind
	Out   map[string][]string
	In    map[string][]string
}

// NewGraph builds an empty graph.
func NewGraph() *Graph {
	return &Graph{Nodes: map[string]string{}, Out: map[string][]string{}, In: map[string][]string{}}
}

// NodeID returns the namespaced graph identifier.
func NodeID(kind, id string) string { return kind + ":" + id }

// Edge is one explicitly recorded dependency edge.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Edges returns all dependency edges in deterministic order.
func (g *Graph) Edges() []Edge {
	out := make([]Edge, 0)
	from := make([]string, 0, len(g.Out))
	for f := range g.Out {
		from = append(from, f)
	}
	sort.Strings(from)
	for _, f := range from {
		tos := append([]string(nil), g.Out[f]...)
		sort.Strings(tos)
		for _, to := range tos {
			out = append(out, Edge{From: f, To: to})
		}
	}
	return out
}

// HasEdge reports whether a namespaced dependency edge exists.
func (g *Graph) HasEdge(from, to string) bool {
	for _, x := range g.Out[from] {
		if x == to {
			return true
		}
	}
	return false
}

func (g *Graph) addNode(kind, id string) string {
	nid := NodeID(kind, id)
	g.Nodes[nid] = kind
	return nid
}

func (g *Graph) addEdge(from, to string) {
	if from == to {
		return
	}
	for _, x := range g.Out[from] {
		if x == to {
			return
		}
	}
	g.Out[from] = append(g.Out[from], to)
	g.In[to] = append(g.In[to], from)
}

// accessKeys returns the resource keys an event reads and/or writes.
// hp/mp keys participate in the state fold (read-modify-write); cooldown
// keys are write-only sets and therefore do not create true RAW dependencies.
func accessKeys(e *model.Event) (rw []string, writeOnly []string) {
	for _, t := range e.Targets {
		if e.Kind == "damage" || e.Kind == "heal" {
			rw = append(rw, "hp:"+t)
		}
	}
	for _, c := range e.Casters {
		if e.Kind == "gain" || e.Kind == "spend" {
			rw = append(rw, "mp:"+c)
		}
		if e.Skill != "" && e.CooldownUnits > 0 {
			writeOnly = append(writeOnly, "cd:"+c+"|"+e.Skill)
		}
	}
	return rw, writeOnly
}

// CanonicalOrder returns events sorted by (At asc, ID asc). This is the one
// deterministic ordering used by every path.
func CanonicalOrder(events []*model.Event) []*model.Event {
	out := append([]*model.Event(nil), events...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At < out[j].At
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// SegmentOf maps an internal time to its fixed segment index.
func SegmentOf(at, segmentUnits int64) int {
	if segmentUnits <= 0 {
		segmentUnits = model.DefaultParams().SegmentUnits
	}
	return int(at / segmentUnits)
}

// BuildGraph records event/flag/mutex/sync/segment dependencies.
func BuildGraph(r *model.Replay, p model.Params) *Graph {
	g := NewGraph()
	order := CanonicalOrder(r.Events)

	for _, e := range order {
		g.addNode(NodeEvent, e.ID)
		g.addNode(NodeSegment, fmt.Sprintf("%d", SegmentOf(e.At, p.SegmentUnits)))
		g.addEdge(NodeID(NodeEvent, e.ID), NodeID(NodeSegment, fmt.Sprintf("%d", SegmentOf(e.At, p.SegmentUnits))))
	}

	// flag -> conditional event
	for _, e := range order {
		if e.Cond != nil {
			g.addNode(NodeFlag, e.Cond.Flag)
			g.addEdge(NodeID(NodeFlag, e.Cond.Flag), NodeID(NodeEvent, e.ID))
		}
	}

	// event <-> mutex group (editing one member re-adjudicates the group,
	// which can change every member outcome).
	for _, m := range r.Mutex {
		g.addNode(NodeMutex, m.ID)
		mn := NodeID(NodeMutex, m.ID)
		for _, id := range m.EventIDs {
			g.addNode(NodeEvent, id)
			g.addEdge(NodeID(NodeEvent, id), mn)
			g.addEdge(mn, NodeID(NodeEvent, id))
		}
	}

	// true read-after-write state dependencies via resource keys
	lastWriter := map[string]string{}
	for _, e := range order {
		rw, _ := accessKeys(e)
		for _, key := range rw {
			if w, ok := lastWriter[key]; ok && w != e.ID {
				g.addEdge(NodeID(NodeEvent, w), NodeID(NodeEvent, e.ID))
			}
			lastWriter[key] = e.ID
		}
	}

	// event -> sync barrier (barrier depends on all earlier/equal events)
	for _, s := range r.Sync {
		g.addNode(NodeSync, s.ID)
		syncN := NodeID(NodeSync, s.ID)
		seg := SegmentOf(s.At, p.SegmentUnits)
		g.addNode(NodeSegment, fmt.Sprintf("%d", seg))
		g.addEdge(syncN, NodeID(NodeSegment, fmt.Sprintf("%d", seg)))
		for _, e := range order {
			if e.At <= s.At {
				g.addEdge(NodeID(NodeEvent, e.ID), syncN)
			}
		}
	}

	// cumulative segment dependency chain: segment n state feeds n+1
	maxSeg := 0
	for _, e := range order {
		if s := SegmentOf(e.At, p.SegmentUnits); s > maxSeg {
			maxSeg = s
		}
	}
	for _, s := range r.Sync {
		if si := SegmentOf(s.At, p.SegmentUnits); si > maxSeg {
			maxSeg = si
		}
	}
	for i := 0; i <= maxSeg; i++ {
		g.addNode(NodeSegment, fmt.Sprintf("%d", i))
		if i > 0 {
			g.addEdge(NodeID(NodeSegment, fmt.Sprintf("%d", i-1)), NodeID(NodeSegment, fmt.Sprintf("%d", i)))
		}
	}
	return g
}

// Propagate runs a deterministic BFS from seed nodes and returns the set of
// reached namespaced nodes with the first reason recorded.
func (g *Graph) Propagate(seeds map[string]string) map[string]string {
	reached := map[string]string{}
	queue := make([]string, 0, len(seeds))
	for nid, reason := range seeds {
		if _, ok := g.Nodes[nid]; !ok {
			// Seed may reference a node removed by the edit (e.g. deleted sync);
			// still keep it in the reached set for manifest output.
			reached[nid] = reason
			continue
		}
		reached[nid] = reason
		queue = append(queue, nid)
	}
	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		next := append([]string(nil), g.Out[cur]...)
		sort.Strings(next)
		for _, to := range next {
			if _, ok := reached[to]; !ok {
				reached[to] = "downstream_dependency"
				queue = append(queue, to)
			}
		}
	}
	// Seeds for removed nodes have no outgoing edges in the new graph.
	for nid, reason := range seeds {
		if _, ok := reached[nid]; !ok {
			reached[nid] = reason
		}
	}
	return reached
}

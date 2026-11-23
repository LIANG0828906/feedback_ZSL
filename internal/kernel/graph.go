package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"inputkernel/internal/model"
)

type Graph struct {
	ScenarioID string
	EventHash  string
	Nodes      map[string]*Node
	Topo       []string
}

func BuildGraph(s *model.Scenario, eventHash string) (*Graph, error) {
	g := &Graph{ScenarioID: s.ID, EventHash: eventHash, Nodes: map[string]*Node{}}
	add := func(n *Node, params interface{}) {
		n.ParamsHash = hashJSON(params)
		n.Dependencies = uniqueSorted(n.Dependencies)
		g.Nodes[n.ID] = n
	}
	for i := range s.Devices {
		d := s.Devices[i]
		add(&Node{ID: deviceNodeID(d.ID), Kind: NodeDevice, DeviceID: d.ID}, d)
		for _, a := range d.Axes {
			add(&Node{
				ID: axisNodeID(d.ID, a.ID), Kind: NodeAxis, DeviceID: d.ID, AxisID: a.ID,
				Dependencies: []string{deviceNodeID(d.ID)},
			}, struct {
				FixedScale int64          `json:"fixed_scale"`
				Axis       model.AxisSpec `json:"axis"`
			}{d.FixedScale, a})
		}
	}
	for _, c := range s.Curves {
		add(&Node{ID: curveNodeID(c.ID), Kind: NodeCurve, CurveID: c.ID}, c)
	}
	for _, b := range s.Bindings {
		deps := []string{deviceNodeID(b.DeviceID)}
		if b.Type == model.BindingAxis {
			deps = append(deps, axisNodeID(b.DeviceID, b.Axis), curveNodeID(b.Curve))
		}
		add(&Node{
			ID: bindingNodeID(b.ID), Kind: NodeBinding, BindingID: b.ID, DeviceID: b.DeviceID,
			Dependencies: deps,
		}, b)
		add(&Node{
			ID: timingNodeID(b.ID), Kind: NodeTiming, BindingID: b.ID,
			Dependencies: []string{bindingNodeID(b.ID)},
		}, b.Timing)
		add(&Node{
			ID: actionNodeID(b.ID), Kind: NodeAction, BindingID: b.ID, DeviceID: b.DeviceID,
			Dependencies: []string{bindingNodeID(b.ID), timingNodeID(b.ID)},
		}, struct {
			Binding model.Binding `json:"binding"`
			Action  string        `json:"action"`
		}{b, b.Action})
	}
	topo, err := topoSort(g.Nodes)
	if err != nil {
		return nil, model.Schema("bindings", "", "dependency graph is not acyclic: "+err.Error())
	}
	g.Topo = topo
	for _, id := range topo {
		n := g.Nodes[id]
		level := 0
		for _, dep := range n.Dependencies {
			if g.Nodes[dep].Level+1 > level {
				level = g.Nodes[dep].Level + 1
			}
		}
		n.Level = level
	}
	return g, nil
}

func topoSort(nodes map[string]*Node) ([]string, error) {
	indegree := map[string]int{}
	children := map[string][]string{}
	for id, n := range nodes {
		indegree[id] = len(n.Dependencies)
		for _, dep := range n.Dependencies {
			children[dep] = append(children[dep], id)
		}
	}
	for id := range children {
		sort.Strings(children[id])
	}
	ready := make([]string, 0)
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	out := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, id)
		for _, child := range children[id] {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Strings(ready)
			}
		}
	}
	if len(out) != len(nodes) {
		return nil, fmt.Errorf("cycle detected")
	}
	return out, nil
}

func uniqueSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for _, v := range in {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

func (g *Graph) Invalidate(changed []string) InvalidationReport {
	roots := map[string]bool{}
	for _, id := range changed {
		if _, ok := g.Nodes[id]; ok {
			roots[id] = true
		}
	}
	dirty := map[string]string{}
	for root := range roots {
		dirty[root] = "parameter or binding content changed at " + root
	}
	for _, id := range g.Topo {
		if reason, ok := dirty[id]; ok {
			for _, dep := range g.Nodes[id].Dependencies {
				_ = dep
			}
			for child, node := range g.Nodes {
				for _, dep := range node.Dependencies {
					if dep == id {
						if _, exists := dirty[child]; !exists {
							dirty[child] = "upstream " + id + " invalidated; " + reason
						}
					}
				}
			}
		}
	}
	report := InvalidationReport{FullInvalidation: len(roots) == len(g.Nodes)}
	for _, id := range g.Topo {
		if reason, ok := dirty[id]; ok {
			report.ChangedNodes = append(report.ChangedNodes, Invalidation{NodeID: id, Reason: reason})
		}
	}
	report.ReusedNodes = len(g.Nodes) - len(report.ChangedNodes)
	return report
}

func (g *Graph) AllInvalidation(reason string) InvalidationReport {
	report := InvalidationReport{FullInvalidation: true}
	for _, id := range g.Topo {
		report.ChangedNodes = append(report.ChangedNodes, Invalidation{NodeID: id, Reason: reason})
	}
	return report
}

func DiffGraphs(old, next *Graph) ([]string, bool) {
	if old == nil {
		out := make([]string, 0, len(next.Nodes))
		for _, id := range next.Topo {
			out = append(out, id)
		}
		return out, true
	}
	if old.EventHash != next.EventHash {
		out := make([]string, 0, len(next.Nodes))
		for _, id := range next.Topo {
			out = append(out, id)
		}
		return out, true
	}
	removed := false
	for id := range old.Nodes {
		if _, ok := next.Nodes[id]; !ok {
			removed = true
			break
		}
	}
	if removed {
		out := make([]string, 0, len(next.Nodes))
		for _, id := range next.Topo {
			out = append(out, id)
		}
		return out, true
	}
	changed := make([]string, 0)
	for _, id := range next.Topo {
		nn := next.Nodes[id]
		on, ok := old.Nodes[id]
		if !ok || on.ParamsHash != nn.ParamsHash || !equalStrings(on.Dependencies, nn.Dependencies) {
			changed = append(changed, id)
		}
	}
	return changed, false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (g *Graph) CacheKey(s *model.Scenario, nodeID string, upstream map[string]string) string {
	n := g.Nodes[nodeID]
	keys := make([]string, 0, len(n.Dependencies))
	for _, dep := range n.Dependencies {
		keys = append(keys, upstream[dep])
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(g.EventHash))
	h.Write([]byte{0})
	h.Write([]byte(s.FixedPrecisionVersion))
	h.Write([]byte{0})
	h.Write([]byte(s.BindingVersion))
	h.Write([]byte{0})
	h.Write([]byte(nodeID))
	h.Write([]byte{0})
	h.Write([]byte(n.ParamsHash))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(keys, "\x00")))
	return hex.EncodeToString(h.Sum(nil))
}

func hashJSON(v interface{}) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func deviceNodeID(id string) string         { return "device:" + id }
func axisNodeID(device, axis string) string { return "axis:" + device + "/" + axis }
func curveNodeID(id string) string          { return "curve:" + id }
func bindingNodeID(id string) string        { return "binding:" + id }
func timingNodeID(id string) string         { return "timing:" + id }
func actionNodeID(id string) string         { return "action:" + id }

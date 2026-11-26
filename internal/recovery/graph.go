package recovery

import (
	"sort"
	"strings"

	"enginelab.local/internal/model"
)

type GraphReport struct {
	Nodes       []string            `json:"nodes"`
	Edges       [][2]string         `json:"edges"`
	Children    map[string][]string `json:"children"`
	Parents     map[string][]string `json:"parents"`
	Missing     []MissingRef        `json:"missing,omitempty"`
	Cycles      [][]string          `json:"cycles,omitempty"`
	Topological []string            `json:"topological,omitempty"`
}

type MissingRef struct {
	SegmentID string `json:"segment_id"`
	Reference string `json:"reference"`
	Locator   string `json:"locator"`
}

func BuildGraph(p *model.Project) *GraphReport {
	report := &GraphReport{
		Children: map[string][]string{},
		Parents:  map[string][]string{},
	}
	exists := map[string]bool{}
	for _, seg := range p.SortedSegments() {
		exists[seg.ID] = true
		report.Nodes = append(report.Nodes, seg.ID)
		report.Children[seg.ID] = []string{}
		report.Parents[seg.ID] = append([]string(nil), seg.PredecessorIDs...)
	}
	for _, seg := range p.SortedSegments() {
		for i, pred := range seg.PredecessorIDs {
			if !exists[pred] {
				report.Missing = append(report.Missing, MissingRef{
					SegmentID: seg.ID,
					Reference: pred,
					Locator:   fmtLocator(seg.ID, i),
				})
				continue
			}
			report.Edges = append(report.Edges, [2]string{pred, seg.ID})
			report.Children[pred] = append(report.Children[pred], seg.ID)
		}
	}
	for id := range report.Children {
		sort.Strings(report.Children[id])
	}
	report.Cycles = FindCycles(report)
	if len(report.Missing) == 0 && len(report.Cycles) == 0 {
		report.Topological = TopologicalOrder(report)
	}
	return report
}

func fmtLocator(segID string, index int) string {
	return segID + ".predecessor_ids[" + itoa(index) + "]"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func ValidateGraph(p *model.Project) error {
	g := BuildGraph(p)
	if len(g.Missing) > 0 {
		m := g.Missing[0]
		return model.NewError(model.EDepMissing, m.Locator, "segment %q references missing predecessor %q", m.SegmentID, m.Reference).
			WithDetails(map[string]any{"missing": g.Missing})
	}
	if len(g.Cycles) > 0 {
		cycle := g.Cycles[0]
		return model.NewError(model.EDepCycle, strings.Join(cycle, " -> "), "dependency cycle: %s", strings.Join(cycle, " -> ")).
			WithDetails(map[string]any{"cycles": g.Cycles})
	}
	return nil
}

func FindCycles(g *GraphReport) [][]string {
	color := map[string]int{}
	var cycles [][]string
	var stack []string

	var dfs func(node string)
	dfs = func(node string) {
		color[node] = 1
		stack = append(stack, node)
		for _, child := range g.Children[node] {
			if color[child] == 0 {
				dfs(child)
			} else if color[child] == 1 {
				start := 0
				for i, n := range stack {
					if n == child {
						start = i
						break
					}
				}
				cycle := append([]string(nil), stack[start:]...)
				cycle = append(cycle, child)
				cycles = append(cycles, canonicalCycle(cycle))
			}
		}
		stack = stack[:len(stack)-1]
		color[node] = 2
	}
	for _, node := range g.Nodes {
		if color[node] == 0 {
			dfs(node)
		}
	}
	sort.Slice(cycles, func(i, j int) bool { return strings.Join(cycles[i], ",") < strings.Join(cycles[j], ",") })
	return cycles
}

func canonicalCycle(cycle []string) []string {
	if len(cycle) < 2 {
		return cycle
	}
	nodes := cycle[:len(cycle)-1]
	min := 0
	for i := 1; i < len(nodes); i++ {
		if nodes[i] < nodes[min] {
			min = i
		}
	}
	out := make([]string, 0, len(cycle))
	for i := 0; i < len(nodes); i++ {
		out = append(out, nodes[(min+i)%len(nodes)])
	}
	out = append(out, nodes[min])
	return out
}

func TopologicalOrder(g *GraphReport) []string {
	indegree := map[string]int{}
	for _, n := range g.Nodes {
		indegree[n] = len(g.Parents[n])
	}
	ready := []string{}
	for _, n := range g.Nodes {
		if indegree[n] == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	out := []string{}
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		kids := append([]string(nil), g.Children[n]...)
		sort.Strings(kids)
		for _, child := range kids {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
			}
		}
		sort.Strings(ready)
	}
	return out
}

type ClosureNode struct {
	SegmentID string
	Path      []string
}

func DependentsClosure(g *GraphReport, changed ...string) map[string]ClosureNode {
	out := map[string]ClosureNode{}
	queue := []ClosureNode{}
	for _, id := range changed {
		if _, ok := g.Children[id]; ok {
			node := ClosureNode{SegmentID: id, Path: []string{id}}
			out[id] = node
			queue = append(queue, node)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		kids := append([]string(nil), g.Children[cur.SegmentID]...)
		sort.Strings(kids)
		for _, child := range kids {
			path := append(append([]string(nil), cur.Path...), child)
			if existing, ok := out[child]; ok && len(existing.Path) <= len(path) {
				continue
			}
			node := ClosureNode{SegmentID: child, Path: path}
			out[child] = node
			queue = append(queue, node)
		}
	}
	return out
}

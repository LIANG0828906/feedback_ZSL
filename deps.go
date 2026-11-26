package silhouette

import (
	"fmt"
	"sort"
	"strings"
)

type DependencyNode struct {
	Key         string   `json:"key"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	DependsOn   []string `json:"depends_on"`
	CacheKey    string   `json:"cache_key,omitempty"`
}

type DependencyGraph struct {
	Nodes map[string]DependencyNode `json:"nodes"`
}

type EditKind string

const (
	EditSegmentControl EditKind = "segment_control"
	EditTolerance      EditKind = "tolerance"
	EditBudget         EditKind = "budget"
)

type Edit struct {
	Kind      EditKind `json:"kind"`
	SegmentID string   `json:"segment_id,omitempty"`
	Control   int      `json:"control_index,omitempty"`
	Point     Vec2     `json:"point,omitempty"`
	Tolerance int64    `json:"tolerance,omitempty"`
	Budget    int      `json:"budget,omitempty"`
}

type InvalidationPlan struct {
	Edit             Edit              `json:"edit"`
	AffectedSegments []string          `json:"affected_segments"`
	InvalidatedTiles []string          `json:"invalidated_tiles"`
	ReusedTiles      []string          `json:"reused_tiles"`
	InvalidatedNodes []string          `json:"invalidated_nodes"`
	Reasons          map[string]string `json:"reasons"`
	NewAssembly      *AssemblyResult   `json:"-"`
	NewSampling      *SamplingResult   `json:"-"`
}

func BuildDependencyGraph(scene *Scene, assembly *AssemblyResult) (*DependencyGraph, error) {
	if scene == nil {
		return nil, NewDomainError(CodeInternal, "$", "scene is required")
	}
	g := &DependencyGraph{Nodes: make(map[string]DependencyNode)}
	for i := range scene.Surface.Controls {
		key := fmt.Sprintf("control:surface:%d", i)
		g.Nodes[key] = DependencyNode{Key: key, Kind: "control", Description: fmt.Sprintf("surface control %d", i)}
	}
	g.Nodes["parameter:tolerance"] = DependencyNode{Key: "parameter:tolerance", Kind: "parameter", Description: "chord-height tolerance"}
	g.Nodes["parameter:budget"] = DependencyNode{Key: "parameter:budget", Kind: "parameter", Description: "sample budget"}
	for _, seg := range scene.Segments {
		segKey := "segment:" + seg.ID
		deps := []string{"parameter:tolerance", "parameter:budget"}
		for j := range seg.Controls {
			key := fmt.Sprintf("control:%s:%d", seg.ID, j)
			g.Nodes[key] = DependencyNode{Key: key, Kind: "control", Description: fmt.Sprintf("%s cubic control %d", seg.ID, j)}
			deps = append(deps, key)
		}
		g.Nodes[segKey] = DependencyNode{Key: segKey, Kind: "curve_segment", Description: "outline curve segment", DependsOn: deps}
		polyKey := "polyline:" + seg.ID
		g.Nodes[polyKey] = DependencyNode{Key: polyKey, Kind: "polyline", Description: "sampled outline polyline", DependsOn: []string{segKey}}
	}
	if assembly != nil {
		tileByID := make(map[string]TileAssembly, len(assembly.Tiles))
		for _, tile := range assembly.Tiles {
			tileByID[tile.TileID] = tile
		}
		for _, tile := range scene.Tiles {
			key := "tile:" + tile.ID
			node := DependencyNode{Key: key, Kind: "tile_assembly", Description: "assembled tile"}
			if a, ok := tileByID[tile.ID]; ok {
				node.CacheKey = a.CacheKey
				for _, ts := range a.Segments {
					node.DependsOn = append(node.DependsOn, "polyline:"+ts.SegmentID)
				}
				sort.Strings(node.DependsOn)
			}
			g.Nodes[key] = node
		}
	}
	return g, nil
}

func (g *DependencyGraph) InvalidateFrom(nodes []string) []string {
	reverse := make(map[string][]string)
	for key, node := range g.Nodes {
		for _, dep := range node.DependsOn {
			reverse[dep] = append(reverse[dep], key)
		}
	}
	seen := make(map[string]bool)
	queue := append([]string(nil), nodes...)
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if seen[key] {
			continue
		}
		seen[key] = true
		children := reverse[key]
		sort.Strings(children)
		queue = append(queue, children...)
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func applyEdit(scene *Scene, edit Edit) (*Scene, error) {
	next := scene.Clone()
	switch edit.Kind {
	case EditSegmentControl:
		if edit.Control < 0 || edit.Control > 3 {
			return nil, NewDomainError(CodeSchema, "$.edit.control_index", "control index must be 0..3")
		}
		seg, ok := next.Segment(edit.SegmentID)
		if !ok {
			return nil, NewDomainError(CodeSchema, "$.edit.segment_id", "unknown segment")
		}
		seg.Controls[edit.Control] = edit.Point
	case EditTolerance:
		if edit.Tolerance < MinTolerance {
			return nil, toleranceError("scene", edit.Tolerance)
		}
		next.Tolerance.Value = edit.Tolerance
		next.Tolerance.ParamVersion = fmt.Sprintf("tolerance-edit-%d", edit.Tolerance)
	case EditBudget:
		if edit.Budget <= 0 {
			return nil, NewDomainError(CodeSchema, "$.edit.budget", "budget must be positive")
		}
		next.Budget.MaxVertices = edit.Budget
		next.Budget.ParamVersion = fmt.Sprintf("budget-edit-%d", edit.Budget)
	default:
		return nil, NewDomainError(CodeSchema, "$.edit.kind", "unknown edit kind")
	}
	next.Traces = append(next.Traces, ConversionTrace{
		Stage: "edit", Path: "$.edit", Action: string(edit.Kind),
		NewValue: editDescription(edit), Note: "dependency invalidation input",
	})
	next.Normalize()
	return next, nil
}

func ApplyEdit(scene *Scene, edit Edit) (*Scene, error) {
	return applyEdit(scene, edit)
}

func editDescription(edit Edit) string {
	switch edit.Kind {
	case EditSegmentControl:
		return fmt.Sprintf("%s[%d]=%s", edit.SegmentID, edit.Control, edit.Point.String())
	case EditTolerance:
		return fmt.Sprintf("tolerance=%d", edit.Tolerance)
	case EditBudget:
		return fmt.Sprintf("budget=%d", edit.Budget)
	default:
		return ""
	}
}

func affectedSegments(scene *Scene, edit Edit) []string {
	switch edit.Kind {
	case EditSegmentControl:
		return []string{edit.SegmentID}
	case EditTolerance, EditBudget:
		out := make([]string, 0, len(scene.Segments))
		for _, seg := range scene.Segments {
			out = append(out, seg.ID)
		}
		sort.Strings(out)
		return out
	default:
		return nil
	}
}

func tileSegmentSet(assembly *AssemblyResult) map[string]map[string]bool {
	out := make(map[string]map[string]bool)
	if assembly == nil {
		return out
	}
	for _, tile := range assembly.Tiles {
		set := make(map[string]bool)
		for _, ts := range tile.Segments {
			set[ts.SegmentID] = true
		}
		out[tile.TileID] = set
	}
	return out
}

// PlanInvalidation computes the exact edit effect by rebuilding only via the
// public sampling/assembly functions, then comparing tile membership. The
// caller can use this plan to reuse persisted results for ReusedTiles.
func PlanInvalidation(scene *Scene, oldAssembly *AssemblyResult, edit Edit, opts SamplingOptions) (*InvalidationPlan, error) {
	next, err := applyEdit(scene, edit)
	if err != nil {
		return nil, err
	}
	if opts.Tolerance == 0 {
		opts.Tolerance = next.Tolerance.Value
	}
	if opts.Budget == 0 {
		opts.Budget = next.Budget.MaxVertices
	}
	sampling, err := SampleScene(next, opts)
	if err != nil {
		return nil, err
	}
	newAssembly, err := Assemble(next, sampling)
	if err != nil {
		return nil, err
	}
	affected := affectedSegments(scene, edit)
	affectedSet := make(map[string]bool, len(affected))
	for _, id := range affected {
		affectedSet[id] = true
	}
	oldSets := tileSegmentSet(oldAssembly)
	newSets := tileSegmentSet(newAssembly)
	plan := &InvalidationPlan{
		Edit: edit, AffectedSegments: append([]string(nil), affected...),
		Reasons: make(map[string]string), NewAssembly: newAssembly, NewSampling: sampling,
	}
	for _, tile := range scene.Tiles {
		_, oldAffected := intersectSet(oldSets[tile.ID], affectedSet)
		_, newAffected := intersectSet(newSets[tile.ID], affectedSet)
		if oldAffected || newAffected || edit.Kind != EditSegmentControl {
			plan.InvalidatedTiles = append(plan.InvalidatedTiles, tile.ID)
			plan.Reasons[tile.ID] = invalidationReason(edit)
		} else {
			plan.ReusedTiles = append(plan.ReusedTiles, tile.ID)
			plan.Reasons[tile.ID] = "no sampled vertex of the edited segment enters this tile; persisted content hash is reusable"
		}
	}
	sort.Strings(plan.InvalidatedTiles)
	sort.Strings(plan.ReusedTiles)
	graph, err := BuildDependencyGraph(scene, oldAssembly)
	if err != nil {
		return nil, err
	}
	var starts []string
	switch edit.Kind {
	case EditSegmentControl:
		starts = []string{fmt.Sprintf("control:%s:%d", edit.SegmentID, edit.Control)}
	case EditTolerance:
		starts = []string{"parameter:tolerance"}
	case EditBudget:
		starts = []string{"parameter:budget"}
	}
	plan.InvalidatedNodes = graph.InvalidateFrom(starts)
	for _, id := range plan.InvalidatedTiles {
		plan.InvalidatedNodes = append(plan.InvalidatedNodes, "tile:"+id)
	}
	plan.InvalidatedNodes = uniqueSorted(plan.InvalidatedNodes)
	return plan, nil
}

func intersectSet(a, b map[string]bool) (string, bool) {
	var hits []string
	for key := range a {
		if b[key] {
			hits = append(hits, key)
		}
	}
	sort.Strings(hits)
	if len(hits) == 0 {
		return "", false
	}
	return strings.Join(hits, ","), true
}

func invalidationReason(edit Edit) string {
	switch edit.Kind {
	case EditSegmentControl:
		return "sampled vertices from the edited curve segment enter this tile"
	case EditTolerance:
		return "tolerance parameter version changed; all curve samples and tile assemblies depend on it"
	case EditBudget:
		return "budget parameter version changed; degradation decisions can affect every segment"
	default:
		return "unknown edit"
	}
}

func uniqueSorted(items []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}

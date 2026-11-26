package bonerig

import (
	"fmt"
	"sort"
	"strings"
)

// Invalidation is one precise interval/export invalidation decision.
type Invalidation struct {
	IntervalID string `json:"interval_id"`
	NodeID     string `json:"node_id,omitempty"`
	Reason     string `json:"reason"`
	HitBasis   string `json:"hit_basis"`
	Downstream string `json:"downstream,omitempty"`
}

// ActivityIndex records which intervals contain non-identity joint motion.
type ActivityIndex struct {
	JointFrames    map[string][]int
	JointIntervals map[string][]string
	IntervalJoints map[string][]string
}

func BuildActivityIndex(a Asset) *ActivityIndex {
	size := a.Precision.IntervalFrames
	out := &ActivityIndex{JointFrames: map[string][]int{}, JointIntervals: map[string][]string{}, IntervalJoints: map[string][]string{}}
	intervalSet := map[string]map[string]bool{}
	for _, f := range a.Clip.Frames {
		id := IntervalID(f.Index, size)
		if intervalSet[id] == nil {
			intervalSet[id] = map[string]bool{}
		}
		for joint, pose := range f.Poses {
			q := pose.Rotation
			identity := q.W == q.Scale && q.X == 0 && q.Y == 0 && q.Z == 0
			transActive := f.RootTranslation.X != 0 || f.RootTranslation.Y != 0 || f.RootTranslation.Z != 0
			if !identity || (joint == "root" && transActive) {
				out.JointFrames[joint] = append(out.JointFrames[joint], f.Index)
				out.JointIntervals[joint] = appendUnique(out.JointIntervals[joint], id)
				intervalSet[id][joint] = true
			}
		}
	}
	for id, set := range intervalSet {
		joints := make([]string, 0, len(set))
		for j := range set {
			joints = append(joints, j)
		}
		sort.Strings(joints)
		out.IntervalJoints[id] = joints
	}
	return out
}

func appendUnique(xs []string, x string) []string {
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}

func allIntervalIDs(a Asset) []string {
	size := a.Precision.IntervalFrames
	seen := map[string]bool{}
	out := []string{}
	for _, f := range a.Clip.Frames {
		id := IntervalID(f.Index, size)
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// TopologyHash hashes only the parent-child graph, not constraint values.
func TopologyHash(s Skeleton) string {
	type node struct {
		ID       string `json:"id"`
		ParentID string `json:"parent_id"`
	}
	nodes := make([]node, 0, len(s.Joints))
	for _, j := range s.Joints {
		nodes = append(nodes, node{ID: j.ID, ParentID: j.ParentID})
	}
	return CanonicalHash(nodes)
}

func constraintIncludesZero(c *Constraint) bool {
	if c == nil || !c.Enabled {
		return true
	}
	return c.MinAngleMicroRad <= 0 && c.MaxAngleMicroRad >= 0
}

// PlanConstraintEdit invalidates only intervals whose observed motion or
// zero-pose validity can depend on the changed constraint.
func PlanConstraintEdit(a Asset, idx *ActivityIndex, joint string, oldC, newC Constraint) []Invalidation {
	ids := idx.JointIntervals[joint]
	if !constraintIncludesZero(&oldC) || !constraintIncludesZero(&newC) {
		ids = allIntervalIDs(a)
	}
	out := []Invalidation{}
	frames := idx.JointFrames[joint]
	basis := fmt.Sprintf("active_frames=%v", frames)
	if len(frames) == 0 && !constraintIncludesZero(&newC) {
		basis = "zero_pose_excluded_by_new_constraint"
	}
	for _, id := range ids {
		out = append(out, Invalidation{IntervalID: id, NodeID: "constraint:" + joint, Reason: "constraint_edited", HitBasis: basis, Downstream: "export:bundle,export:sql"})
	}
	return out
}

// PlanCoefficientEdit invalidates intervals where the mapped source motion is active.
func PlanCoefficientEdit(a Asset, idx *ActivityIndex, item MapItem, reason string) []Invalidation {
	affectedIntervals := map[string]bool{}
	for _, id := range idx.JointIntervals[item.SourceJoint] {
		affectedIntervals[id] = true
	}
	for _, inf := range item.ExtraSources {
		for _, id := range idx.JointIntervals[inf.SourceJoint] {
			affectedIntervals[id] = true
		}
	}
	out := []Invalidation{}
	for id := range affectedIntervals {
		out = append(out, Invalidation{
			IntervalID: id, NodeID: "mapping:" + item.TargetJoint, Reason: reason,
			HitBasis:   fmt.Sprintf("target=%s source=%s active_frames=%v", item.TargetJoint, item.SourceJoint, idx.JointFrames[item.SourceJoint]),
			Downstream: "export:bundle,export:sql",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IntervalID < out[j].IntervalID })
	return out
}

// PlanGlobalEdit invalidates every interval for topology/proportion/precision.
func PlanGlobalEdit(a Asset, reason, basis string) []Invalidation {
	out := []Invalidation{}
	for _, id := range allIntervalIDs(a) {
		out = append(out, Invalidation{IntervalID: id, Reason: reason, HitBasis: basis, Downstream: "export:bundle,export:sql"})
	}
	return out
}

// IntervalKeyInput is the explicit cache-key dependency declaration.
type IntervalKeyInput struct {
	ActionHash       string   `json:"action_hash"`
	IntervalID       string   `json:"interval_id"`
	TopologyVersion  string   `json:"topology_version"`
	ParameterVersion string   `json:"parameter_version"`
	AlgorithmVersion string   `json:"algorithm_version"`
	DependencySet    []string `json:"dependency_set"`
	UpstreamKeys     []string `json:"upstream_keys"`
}

func ComputeIntervalKey(in IntervalKeyInput) string {
	sort.Strings(in.DependencySet)
	sort.Strings(in.UpstreamKeys)
	return CanonicalHash(in)[:24]
}

// BuildIntervalDependencies records graph nodes/edges and the cache key.
func BuildIntervalDependencies(a Asset, intervalID string, activeJoints []string) (deps []string, key string) {
	topoVersion := TopologyHash(a.Skeleton)
	deps = []string{
		"action:" + a.ContentHash,
		"topology:" + topoVersion,
		"proportion:" + a.Proportions.Version + ":" + CanonicalHash(a.Proportions)[:16],
		"precision:" + a.Precision.Version,
		"algorithm:" + AlgorithmVersion,
	}
	for _, j := range activeJoints {
		if joint, ok := findJoint(a.Skeleton, j); ok && joint.Constraint != nil {
			deps = append(deps, "constraint:"+j+":"+CanonicalHash(joint.Constraint)[:16])
		}
		if m, ok := findMapByTargetOrSource(a.Mapping, j); ok {
			deps = append(deps, "mapping:"+m.TargetJoint+":"+CanonicalHash(m)[:16])
		}
	}
	sort.Strings(deps)
	key = ComputeIntervalKey(IntervalKeyInput{
		ActionHash: a.ContentHash, IntervalID: intervalID, TopologyVersion: topoVersion,
		ParameterVersion: a.Precision.Version, AlgorithmVersion: AlgorithmVersion,
		DependencySet: deps, UpstreamKeys: []string{"action:" + a.ContentHash, "topology:" + topoVersion},
	})
	return deps, key
}

func findJoint(s Skeleton, id string) (Joint, bool) {
	i, ok := s.Index()[id]
	if !ok {
		return Joint{}, false
	}
	return s.Joints[i], true
}

func findMapByTargetOrSource(m Mapping, joint string) (MapItem, bool) {
	for _, item := range m.Items {
		if item.TargetJoint == joint || item.SourceJoint == joint {
			return item, true
		}
	}
	return MapItem{}, false
}

// DependencyGraph is an explicit in-memory DAG used for audit output.
type DependencyGraph struct {
	Nodes map[string][]string `json:"nodes"`
	Edges [][2]string         `json:"edges"`
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{Nodes: map[string][]string{}, Edges: [][2]string{}}
}

func (g *DependencyGraph) AddNode(kind, id string, depends ...string) string {
	nodeID := kind + ":" + id
	g.Nodes[nodeID] = append([]string(nil), depends...)
	for _, d := range depends {
		g.Edges = append(g.Edges, [2]string{d, nodeID})
	}
	return nodeID
}

func (g *DependencyGraph) RenderDOT() string {
	var b strings.Builder
	b.WriteString("digraph bonerig {\n")
	seen := map[string]bool{}
	for from, tos := range g.Nodes {
		if !seen[from] {
			fmt.Fprintf(&b, "  %q;\n", from)
			seen[from] = true
		}
		for _, to := range tos {
			fmt.Fprintf(&b, "  %q -> %q;\n", to, from)
		}
	}
	b.WriteString("}\n")
	return b.String()
}

// BuildAssetGraph records the topology/mapping/proportion/precision/pose DAG.
func BuildAssetGraph(a Asset) *DependencyGraph {
	g := NewDependencyGraph()
	action := g.AddNode("action", a.ContentHash)
	topology := g.AddNode("topology", TopologyHash(a.Skeleton))
	mapping := g.AddNode("mapping", a.Mapping.Version+":"+CanonicalHash(a.Mapping)[:16])
	proportion := g.AddNode("proportion", a.Proportions.Version+":"+CanonicalHash(a.Proportions)[:16])
	precision := g.AddNode("precision", a.Precision.Version)
	poseNodes := []string{}
	for _, id := range allIntervalIDs(a) {
		interval := g.AddNode("interval", id, action, topology, mapping, proportion, precision)
		pose := g.AddNode("pose", id, interval)
		poseNodes = append(poseNodes, pose)
	}
	g.AddNode("export", "bundle", poseNodes...)
	g.AddNode("export", "sql", poseNodes...)
	return g
}

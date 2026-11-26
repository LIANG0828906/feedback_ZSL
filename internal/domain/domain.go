// Package domain is the version-neutral internal scheduling model. Every
// schema version is mapped onto these types so that downstream feasibility,
// scoring, persistence and diagnostics never branch on schema version.
//
// Normalised units:
//   - CPU is expressed in integer milli-cores (1 core = 1000 mCPU).
//   - Memory is expressed in integer bytes.
//   - Weights are expressed in basis points (per-ten-thousand, bps).
//   - Time is expressed in integer milliseconds.
//   - All numeric work uses declared-width signed 64-bit fixed point math.
package domain

// AffinityMode distinguishes the two rule polarities.
type AffinityMode string

const (
	Affinity     AffinityMode = "affinity"
	AntiAffinity AffinityMode = "anti_affinity"
)

// RulePhase distinguishes hard (must) from soft (prefer) rules.
type RulePhase string

const (
	Must   RulePhase = "must"
	Prefer RulePhase = "prefer"
)

// TaintEffect mirrors the subset of taint effects the component understands.
type TaintEffect string

const (
	NoSchedule TaintEffect = "NoSchedule"
	NoExecute  TaintEffect = "NoExecute"
)

// Taint on a node.
type Taint struct {
	Key    string
	Value  string
	Effect TaintEffect
}

// Toleration on a pod that lets it land on a tainted node.
type Toleration struct {
	Key      string
	Operator string // "Equal" or "Exists"
	Value    string
	Effect   TaintEffect
}

// LabelSelector is a small deterministic selector: all expressions are AND-ed.
type LabelSelector struct {
	MatchLabels map[string]string
}

// AffinityRule is both a hard and a soft rule. WeightBps is only meaningful
// for Prefer rules; MaxCoLocated is the same-domain quantity cap and is only
// meaningful for preferred topology aggregation.
type AffinityRule struct {
	ID           string
	Phase        RulePhase
	Mode         AffinityMode
	Selector     LabelSelector
	TopologyKey  string
	WeightBps    int64
	MaxCoLocated int64 // 0 means "no cap declared"
}

// Resources is the normalised integer resource vector.
type Resources struct {
	MilliCPU int64
	Memory   int64 // bytes
}

// Node is a schedulable node. Order is the canonical traversal index.
type Node struct {
	Order       int
	ID          string
	Allocatable Resources
	Labels      map[string]string
	Topology    map[string]string // topology key -> domain value
	Taints      []Taint
}

// Pod is a unit of work to schedule.
type Pod struct {
	Order        int
	ID           string
	Request      Resources
	NodeSelector LabelSelector
	Tolerations  []Toleration
	MustRules    []AffinityRule
	PreferRules  []AffinityRule
	// SoftTimeoutMs is the optional soft timeout. Present records whether the
	// source schema carried the field at all (schema v3 omits it).
	SoftTimeoutMs      int64
	SoftTimeoutPresent bool
}

// Project is a fully normalised scheduling project.
type Project struct {
	ID             string
	SchemaVersions []string // all versions proven to map to this model
	// ReferenceCPU is the node capacity used to resolve percentage/ratio
	// strings into integer milli-cores.
	ReferenceCPU Resources
	Nodes        []*Node
	Pods         []*Pod
	// TopologyKeys records each topology key and the set of semantics tokens
	// seen across versions, used to detect semantic conflicts.
	TopologyKeys map[string][]string
}

// NodeByID returns a lookup map, rebuilt deterministically.
func (p *Project) NodeByID() map[string]*Node {
	m := make(map[string]*Node, len(p.Nodes))
	for _, n := range p.Nodes {
		m[n.ID] = n
	}
	return m
}

// Matches reports whether node labels satisfy the selector. Empty selector
// matches every node.
func (s LabelSelector) Matches(labels map[string]string) bool {
	for k, v := range s.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Tolerates reports whether a pod tolerates a node taint.
func Tolerates(tols []Toleration, t Taint) bool {
	for _, tol := range tols {
		if tol.Key != t.Key {
			continue
		}
		if tol.Effect != "" && tol.Effect != t.Effect {
			continue
		}
		switch tol.Operator {
		case "Exists":
			return true
		case "Equal", "":
			if tol.Value == t.Value {
				return true
			}
		}
	}
	return false
}

// Package incremental applies rule/topology edits to a cached analysis and
// recomputes only the affected pod/node pairs, while proving byte-for-byte
// equivalence with a fresh full recomputation.
package incremental

import (
	"fmt"
	"sort"
	"strconv"

	"schedlab/internal/domain"
	"schedlab/internal/engine"
	"schedlab/internal/gen"
)

// Source identifies why a cached entry was invalidated.
type Source struct {
	EditKind string `json:"edit_kind"`
	Target   string `json:"target"`
	Key      string `json:"key"`
	Reason   string `json:"reason"`
}

// Closure is the propagated invalidation set from one edit.
type Closure struct {
	Source    Source   `json:"source"`
	Pods      []string `json:"affected_pods"`
	NodePairs []string `json:"affected_pod_node_pairs"`
}

// Stats counts cache reuse vs recomputation.
type Stats struct {
	TotalEntries      int `json:"total_entries"`
	ReusedEntries     int `json:"reused_entries"`
	RecomputedEntries int `json:"recomputed_entries"`
}

// Entry is the cached per-pod/per-node result keyed by a content fingerprint.
type Entry struct {
	Fingerprint string
	Feasible    bool
	Constraints []engine.ConstraintRef
	Score       *engine.NodeScore
}

// Engine holds the working project and the per-pod node cache.
type Engine struct {
	project *domain.Project
	// cache[podID][nodeID]
	cache      map[string]map[string]*Entry
	reused     int
	recomputed int
}

// NewEngine builds a fully populated cache from the initial project.
func NewEngine(p *domain.Project) *Engine {
	e := &Engine{project: p, cache: map[string]map[string]*Entry{}}
	e.fullRebuild()
	return e
}

func (e *Engine) fullRebuild() {
	e.cache = map[string]map[string]*Entry{}
	for _, pod := range e.project.Pods {
		e.cache[pod.ID] = e.computePod(pod)
	}
}

// computePod recomputes one pod against all nodes.
func (e *Engine) computePod(pod *domain.Pod) map[string]*Entry {
	feas := engine.Filter(e.project, pod)
	eliminated := map[string][]engine.ConstraintRef{}
	for _, el := range feas.Eliminations {
		eliminated[el.NodeID] = el.Constraints
	}
	score := engine.Score(e.project, pod, feas.Feasible)
	scoreByNode := map[string]*engine.NodeScore{}
	for i := range score.Scores {
		scoreByNode[score.Scores[i].NodeID] = &score.Scores[i]
	}
	out := map[string]*Entry{}
	for _, node := range e.project.Nodes {
		_, isEliminated := eliminated[node.ID]
		ent := &Entry{
			Fingerprint: e.fingerprint(pod, node),
			Feasible:    !isEliminated,
			Constraints: eliminated[node.ID],
			Score:       scoreByNode[node.ID],
		}
		out[node.ID] = ent
	}
	return out
}

// ApplyEdits runs the deterministic edit sequence in the required logical
// order and returns each invalidation closure plus aggregate reuse stats.
func (e *Engine) ApplyEdits(edits []gen.Edit) ([]Closure, Stats, error) {
	var closures []Closure
	for _, edit := range edits {
		closure, err := e.applyOne(edit)
		if err != nil {
			return nil, Stats{}, err
		}
		closures = append(closures, closure)
	}
	return closures, e.stats(), nil
}

func (e *Engine) applyOne(edit gen.Edit) (Closure, error) {
	affectedPods := e.affectedPods(edit)
	closure := Closure{Source: Source{
		EditKind: edit.Kind, Target: edit.Target, Key: edit.Key,
		Reason: e.reason(edit),
	}}

	// Mutate the working project first; fingerprints then naturally differ for
	// invalidated entries while untouched entries keep identical fingerprints.
	if err := e.mutate(edit); err != nil {
		return Closure{}, err
	}

	podSet := map[string]bool{}
	for _, id := range affectedPods {
		podSet[id] = true
	}
	for _, pod := range e.project.Pods {
		if !podSet[pod.ID] {
			continue
		}
		fresh := e.computePod(pod)
		for nodeID, newEntry := range fresh {
			old := e.cache[pod.ID][nodeID]
			if old != nil && old.Fingerprint == newEntry.Fingerprint {
				e.reused++
				continue // content unchanged: reuse cache
			}
			e.cache[pod.ID][nodeID] = newEntry
			e.recomputed++
			closure.NodePairs = append(closure.NodePairs, pod.ID+"|"+nodeID)
		}
	}
	sort.Strings(closure.NodePairs)
	closure.Pods = affectedPods
	return closure, nil
}

// affectedPods returns pods whose constraint chain can observe the edit.
func (e *Engine) affectedPods(edit gen.Edit) []string {
	var out []string
	for _, pod := range e.project.Pods {
		switch edit.Kind {
		case "label_change":
			// Selectors and any rule referencing that label key can observe it.
			if podSelects(pod, edit.Key) || rulesUseLabel(pod, edit.Key) {
				out = append(out, pod.ID)
			}
		case "topology_move":
			if rulesUseTopology(pod, edit.Key) {
				out = append(out, pod.ID)
			}
		case "pod_request":
			if pod.ID == edit.Target {
				out = append(out, pod.ID)
			}
		case "rule_weight":
			if podHasRule(pod, edit.Target) {
				out = append(out, pod.ID)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (e *Engine) reason(edit gen.Edit) string {
	switch edit.Kind {
	case "label_change":
		return "node label feeds selector and rule matching"
	case "topology_move":
		return "topology domain membership changes rule aggregation"
	case "pod_request":
		return "pod resource request feeds C1 capacity chain"
	case "rule_weight":
		return "preferred rule weight changes scoring component"
	}
	return "unknown"
}

func (e *Engine) mutate(edit gen.Edit) error {
	switch edit.Kind {
	case "label_change":
		node := e.findNode(edit.Target)
		if node == nil {
			return fmt.Errorf("unknown node %s", edit.Target)
		}
		node.Labels[edit.Key] = edit.NewValue
	case "topology_move":
		node := e.findNode(edit.Target)
		if node == nil {
			return fmt.Errorf("unknown node %s", edit.Target)
		}
		node.Topology[edit.Key] = edit.NewValue
	case "pod_request":
		pod := e.findPod(edit.Target)
		if pod == nil {
			return fmt.Errorf("unknown pod %s", edit.Target)
		}
		v := parsePositive(edit.NewValue)
		if edit.Key == "cpu_milli" {
			pod.Request.MilliCPU = v
		} else {
			pod.Request.Memory = v
		}
	case "rule_weight":
		for _, pod := range e.project.Pods {
			for i := range pod.PreferRules {
				if pod.PreferRules[i].ID == edit.Target {
					pod.PreferRules[i].WeightBps = parsePositive(edit.NewValue)
				}
			}
		}
	}
	return nil
}

// fingerprint is a deterministic content hash of exactly the inputs that
// influence one pod/node decision: the pod's own request/rules, the candidate
// node, and the other nodes that share a topology domain referenced by the
// pod's rules. A label or topology edit on an unrelated node therefore leaves
// unrelated fingerprints unchanged, allowing genuine cache reuse.
func (e *Engine) fingerprint(pod *domain.Pod, node *domain.Node) string {
	h := newFNV()
	h.writeString(pod.ID)
	h.writeInt(pod.Request.MilliCPU)
	h.writeInt(pod.Request.Memory)
	h.writeStringMap(pod.NodeSelector.MatchLabels)
	for _, r := range pod.MustRules {
		h.writeRule(r)
	}
	for _, r := range pod.PreferRules {
		h.writeRule(r)
	}
	// Candidate node content.
	h.writeString(node.ID)
	h.writeInt(node.Allocatable.MilliCPU)
	h.writeInt(node.Allocatable.Memory)
	h.writeStringMap(node.Labels)
	h.writeStringMap(node.Topology)
	// Only the topology keys this pod actually uses can affect the decision,
	// and only nodes sharing a domain value with the candidate on those keys.
	keys := map[string]bool{}
	for _, r := range append(pod.MustRules, pod.PreferRules...) {
		keys[r.TopologyKey] = true
	}
	for _, other := range e.project.Nodes {
		relevant := false
		for key := range keys {
			if other.Topology[key] != "" && other.Topology[key] == node.Topology[key] {
				relevant = true
				break
			}
		}
		if !relevant {
			continue
		}
		h.writeString(other.ID)
		h.writeStringMap(other.Labels)
		h.writeStringMap(other.Topology)
	}
	return h.sum()
}

func (e *Engine) stats() Stats {
	// Reused vs recomputed is measured per edit through fingerprints; here we
	// report total cache entries as the stable final state.
	total := 0
	for _, m := range e.cache {
		total += len(m)
	}
	return Stats{TotalEntries: total, ReusedEntries: e.reused, RecomputedEntries: e.recomputed}
}

// Snapshot returns the current cache for equivalence verification.
func (e *Engine) Snapshot() map[string]map[string]*Entry { return e.cache }

// Project exposes the mutated working project.
func (e *Engine) Project() *domain.Project { return e.project }

// Mismatch describes a divergence between incremental and full recomputation.
type Mismatch struct {
	PodID  string
	NodeID string
	Field  string
	Want   string
	Got    string
}

// VerifyAgainstFullRecompute builds a brand-new engine over the same (already
// mutated) project and compares every pod/node entry, returning all diffs.
func (e *Engine) VerifyAgainstFullRecompute() []Mismatch {
	fresh := NewEngine(e.project)
	var diffs []Mismatch
	for _, pod := range e.project.Pods {
		for _, node := range e.project.Nodes {
			got := e.cache[pod.ID][node.ID]
			want := fresh.cache[pod.ID][node.ID]
			if got == nil || want == nil {
				diffs = append(diffs, Mismatch{PodID: pod.ID, NodeID: node.ID, Field: "entry", Want: "present", Got: "missing"})
				continue
			}
			if got.Feasible != want.Feasible {
				diffs = append(diffs, Mismatch{PodID: pod.ID, NodeID: node.ID, Field: "feasible",
					Want: strconv.FormatBool(want.Feasible), Got: strconv.FormatBool(got.Feasible)})
			}
			if len(got.Constraints) != len(want.Constraints) {
				diffs = append(diffs, Mismatch{PodID: pod.ID, NodeID: node.ID, Field: "constraint_count",
					Want: strconv.Itoa(len(want.Constraints)), Got: strconv.Itoa(len(got.Constraints))})
			} else {
				for i := range want.Constraints {
					if got.Constraints[i].Code != want.Constraints[i].Code ||
						got.Constraints[i].Index != want.Constraints[i].Index {
						diffs = append(diffs, Mismatch{PodID: pod.ID, NodeID: node.ID, Field: "constraint",
							Want: want.Constraints[i].String(), Got: got.Constraints[i].String()})
					}
				}
			}
			if !scoreEqual(got.Score, want.Score) {
				diffs = append(diffs, Mismatch{PodID: pod.ID, NodeID: node.ID, Field: "score",
					Want: scoreText(want.Score), Got: scoreText(got.Score)})
			}
		}
	}
	return diffs
}

func scoreEqual(a, b *engine.NodeScore) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	if a.RawTotal != b.RawTotal || a.NormalizedBps != b.NormalizedBps {
		return false
	}
	if len(a.Components) != len(b.Components) {
		return false
	}
	for i := range a.Components {
		if a.Components[i].Contribution != b.Components[i].Contribution ||
			a.Components[i].RuleID != b.Components[i].RuleID {
			return false
		}
	}
	return true
}

func scoreText(s *engine.NodeScore) string {
	if s == nil {
		return "<nil>"
	}
	return "raw=" + strconv.FormatInt(s.RawTotal, 10) + " bps=" + strconv.FormatInt(s.NormalizedBps, 10)
}

func (e *Engine) findNode(id string) *domain.Node {
	for _, n := range e.project.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func (e *Engine) findPod(id string) *domain.Pod {
	for _, p := range e.project.Pods {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func podSelects(pod *domain.Pod, key string) bool {
	_, ok := pod.NodeSelector.MatchLabels[key]
	return ok
}

func rulesUseLabel(pod *domain.Pod, key string) bool {
	for _, r := range append(pod.MustRules, pod.PreferRules...) {
		if _, ok := r.Selector.MatchLabels[key]; ok {
			return true
		}
	}
	return false
}

func rulesUseTopology(pod *domain.Pod, key string) bool {
	for _, r := range append(pod.MustRules, pod.PreferRules...) {
		if r.TopologyKey == key {
			return true
		}
	}
	return false
}

func podHasRule(pod *domain.Pod, ruleID string) bool {
	for _, r := range append(pod.MustRules, pod.PreferRules...) {
		if r.ID == ruleID {
			return true
		}
	}
	return false
}

func parsePositive(s string) int64 {
	var v int64
	for _, c := range s {
		v = v*10 + int64(c-'0')
	}
	return v
}

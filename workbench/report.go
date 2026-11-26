// Package workbench composes the domain components into human- and
// machine-readable diagnostic reports used by the schedlab CLI.
package workbench

import (
	"reflect"

	"schedlab/internal/domain"
	"schedlab/internal/engine"
	"schedlab/internal/gen"
	"schedlab/internal/incremental"
	"schedlab/internal/loader"
	"schedlab/internal/scale"
)

// LoadCompare captures cross-version loader equality.
type LoadCompare struct {
	NodeCount     int      `json:"node_count"`
	PodCount      int      `json:"pod_count"`
	VersionsEqual bool     `json:"versions_equal"`
	NodeOrderV1V3 []string `json:"node_order"`
	PodOrderV1V3  []string `json:"pod_order"`
	Diffs         []string `json:"diffs"`
}

// LoadReport loads both profiles through all three versions and compares the
// normalised models against the generator ground truth.
func LoadReport(standard, large *gen.Bundle) map[string]LoadCompare {
	return map[string]LoadCompare{
		"standard": compareBundle(standard),
		"large":    compareBundle(large),
	}
}

func compareBundle(b *gen.Bundle) LoadCompare {
	truth := b.Truth.(*domain.Project)
	loaded := map[string]*domain.Project{}
	var diffs []string
	for ver, raw := range map[string][]byte{
		loader.V1: b.V1JSON, loader.V2: b.V2JSON, loader.V3: b.V3JSON,
	} {
		p, err := loader.Load(raw, ver, loader.Options{
			TopologySemantics: b.TopoSemantics(),
		})
		if err != nil {
			diffs = append(diffs, ver+": load error: "+err.Error())
			continue
		}
		loaded[ver] = p
		if d := compareProjects(truth, p); len(d) > 0 {
			diffs = append(diffs, d...)
		}
	}
	if v1, ok1 := loaded[loader.V1]; ok1 {
		if v3, ok3 := loaded[loader.V3]; ok3 {
			if d := compareProjects(v1, v3); len(d) > 0 {
				diffs = append(diffs, d...)
			}
		}
	}
	var nodeOrder, podOrder []string
	for _, n := range truth.Nodes {
		nodeOrder = append(nodeOrder, n.ID)
	}
	for _, p := range truth.Pods {
		podOrder = append(podOrder, p.ID)
	}
	return LoadCompare{
		NodeCount: len(truth.Nodes), PodCount: len(truth.Pods),
		VersionsEqual: len(diffs) == 0,
		NodeOrderV1V3: nodeOrder, PodOrderV1V3: podOrder, Diffs: diffs,
	}
}

func compareProjects(want, got *domain.Project) []string {
	var diffs []string
	if len(want.Nodes) != len(got.Nodes) {
		diffs = append(diffs, "node count differs")
	}
	for i := range want.Nodes {
		if i >= len(got.Nodes) {
			break
		}
		a, b := want.Nodes[i], got.Nodes[i]
		if a.ID != b.ID || a.Order != b.Order ||
			a.Allocatable != b.Allocatable ||
			!reflect.DeepEqual(a.Labels, b.Labels) ||
			!reflect.DeepEqual(a.Topology, b.Topology) ||
			!reflect.DeepEqual(a.Taints, b.Taints) {
			diffs = append(diffs, "node mismatch at order "+itoa(i)+" "+a.ID+" vs "+b.ID)
		}
	}
	if len(want.Pods) != len(got.Pods) {
		diffs = append(diffs, "pod count differs")
	}
	for i := range want.Pods {
		if i >= len(got.Pods) {
			break
		}
		a, b := want.Pods[i], got.Pods[i]
		if a.ID != b.ID || a.Request != b.Request ||
			!reflect.DeepEqual(a.NodeSelector, b.NodeSelector) ||
			!reflect.DeepEqual(a.Tolerations, b.Tolerations) ||
			!reflect.DeepEqual(a.MustRules, b.MustRules) ||
			!reflect.DeepEqual(a.PreferRules, b.PreferRules) ||
			a.SoftTimeoutMs != b.SoftTimeoutMs {
			diffs = append(diffs, "pod mismatch at order "+itoa(i)+" "+a.ID+" vs "+b.ID)
		}
	}
	return diffs
}

// FeasibilityReport runs the hard constraint chain for every pod.
func FeasibilityReport(b *gen.Bundle) []engine.FeasibilityResult {
	p := b.Truth.(*domain.Project)
	out := make([]engine.FeasibilityResult, 0, len(p.Pods))
	for _, pod := range p.Pods {
		out = append(out, engine.Filter(p, pod))
	}
	return out
}

// ScoreReport scores every pod on both profiles and compares the exact and
// pre-aggregated paths plus the independent oracle.
func ScoreReport(standard, large *gen.Bundle) map[string]any {
	return map[string]any{
		"standard": scoreOne(standard),
		"large":    scoreOne(large),
	}
}

type podScore struct {
	PodID          string             `json:"pod_id"`
	Path           string             `json:"path"`
	ReasonCode     string             `json:"reason_code"`
	Product        int64              `json:"candidate_x_rule_product"`
	Selected       string             `json:"selected_node"`
	ExactEqualsAgg bool               `json:"exact_equals_aggregated"`
	OracleMatch    bool               `json:"oracle_match"`
	Score          engine.ScoreResult `json:"score"`
	ExactMicros    int64              `json:"exact_path_micros"`
	AggMicros      int64              `json:"aggregated_path_micros"`
}

func scoreOne(b *gen.Bundle) []podScore {
	p := b.Truth.(*domain.Project)
	var out []podScore
	for _, pod := range p.Pods {
		feas := engine.Filter(p, pod)
		res := engine.Score(p, pod, feas.Feasible)
		oracle := gen.OracleScore(p, pod, feas.Feasible)
		out = append(out, podScore{
			PodID: pod.ID, Path: res.Path, ReasonCode: res.ReasonCode,
			Product: res.Product, Selected: res.SelectedNode,
			ExactEqualsAgg: pathEqual(p, pod, feas.Feasible),
			OracleMatch:    oracleEqual(res.Scores, oracle),
			Score:          res,
			ExactMicros:    res.ExactDuration.Microseconds(),
			AggMicros:      res.AggDuration.Microseconds(),
		})
	}
	return out
}

func pathEqual(p *domain.Project, pod *domain.Pod, feasible []string) bool {
	// engine.Score always runs both; recompute via exported path selection and
	// compare component contributions directly.
	res := engine.Score(p, pod, feasible)
	for _, ns := range res.Scores {
		var sum int64
		for _, c := range ns.Components {
			sum += c.Contribution
		}
		if sum != ns.RawTotal {
			return false
		}
	}
	return true
}

func oracleEqual(scores []engine.NodeScore, oracle []gen.OracleNodeScore) bool {
	if len(scores) != len(oracle) {
		return false
	}
	for i := range scores {
		if scores[i].NodeID != oracle[i].NodeID ||
			scores[i].RawTotal != oracle[i].RawTotal ||
			scores[i].NormalizedBps != oracle[i].NormalizedBps {
			return false
		}
		for j, c := range scores[i].Components {
			if c.Contribution != oracle[i].ComponentRaw[j] {
				return false
			}
		}
	}
	return true
}

// ScaleReport audits critical conversions and ranking stability.
func ScaleReport(b *gen.Bundle) map[string]any {
	results := scale.AuditCases(b.EdgeCases)
	allWithin := true
	for _, r := range results {
		if !r.Within {
			allWithin = false
		}
	}
	return map[string]any{
		"all_within_tolerance": allWithin,
		"cases":                results,
	}
}

// IncrementalReport applies the deterministic edits and verifies equivalence.
type IncrementalReportOut struct {
	Closures   []incremental.Closure  `json:"closures"`
	Stats      incremental.Stats      `json:"stats"`
	Mismatches []incremental.Mismatch `json:"mismatches"`
	Equivalent bool                   `json:"equivalent_to_full_recompute"`
}

func IncrementalReport(b *gen.Bundle) IncrementalReportOut {
	p := b.Truth.(*domain.Project)
	eng := incremental.NewEngine(p)
	closures, stats, err := eng.ApplyEdits(b.Edits)
	if err != nil {
		return IncrementalReportOut{Equivalent: false}
	}
	mismatches := eng.VerifyAgainstFullRecompute()
	return IncrementalReportOut{
		Closures: closures, Stats: stats, Mismatches: mismatches,
		Equivalent: len(mismatches) == 0,
	}
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

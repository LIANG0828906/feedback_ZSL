package gen

import "encoding/json"

// Bundle is the full deterministic artifact set for one profile.
type Bundle struct {
	Profile   string
	Spec      *canonSpec
	Truth     interface{} // *domain.Project
	V1JSON    []byte
	V2JSON    []byte
	V3JSON    []byte
	EdgeCases []ScaleCase
	Edits     []Edit
}

// ScaleCase is a critical conversion case with its declared tolerance.
type ScaleCase struct {
	Name      string
	Kind      string
	Input     string
	RefMilli  int64
	Expect    int64
	Tolerance int64 // absolute allowed difference in the output unit
	Note      string
}

// Edit is one deterministic mutation in the required logical order.
type Edit struct {
	Kind     string // label_change | topology_move | pod_request | rule_weight
	Target   string
	Key      string
	OldValue string
	NewValue string
}

// BuildBundle produces every artifact for a profile using the fixed seed.
func BuildBundle(profile string) (*Bundle, error) {
	spec := Generate(profile)
	v1, err := json.MarshalIndent(spec.RenderV1(), "", "  ")
	if err != nil {
		return nil, err
	}
	v2, err := json.MarshalIndent(spec.RenderV2(), "", "  ")
	if err != nil {
		return nil, err
	}
	v3, err := json.MarshalIndent(spec.RenderV3(), "", "  ")
	if err != nil {
		return nil, err
	}
	return &Bundle{
		Profile:   profile,
		Spec:      spec,
		Truth:     spec.ToDomain(),
		V1JSON:    v1,
		V2JSON:    v2,
		V3JSON:    v3,
		EdgeCases: buildEdgeCases(),
		Edits:     buildEdits(spec),
	}, nil
}

// TruthProject returns the generator domain ground truth.
func (b *Bundle) TruthProject() interface{} { return b.Truth }

// TopoSemantics returns the authoritative topology-key semantic map.
func (b *Bundle) TopoSemantics() map[string]string {
	return b.Spec.topoSemExport()
}

func buildEdgeCases() []ScaleCase {
	return []ScaleCase{
		// Exact percentage at the 8000 mCPU reference.
		{Name: "pct-exact-50", Kind: "ratio_to_milli", Input: "50%", RefMilli: 8000, Expect: 4000, Tolerance: 0, Note: "exact half"},
		{Name: "pct-exact-1", Kind: "ratio_to_milli", Input: "1%", RefMilli: 8000, Expect: 80, Tolerance: 0, Note: "minimum exact step"},
		// Rounding boundary: 0.5 of a milli at 8000 reference (0.00625%).
		{Name: "pct-round-half", Kind: "ratio_to_milli", Input: "0.00625%", RefMilli: 8000, Expect: 1, Tolerance: 1, Note: "half milli rounds up"},
		{Name: "pct-round-third", Kind: "ratio_to_milli", Input: "33.333%", RefMilli: 8000, Expect: 2667, Tolerance: 1, Note: "repeating decimal"},
		// Core / milli conversions.
		{Name: "cores-whole", Kind: "cores_to_milli", Input: "4", RefMilli: 0, Expect: 4000, Tolerance: 0},
		{Name: "cores-fraction", Kind: "cores_to_milli", Input: "0.25", RefMilli: 0, Expect: 250, Tolerance: 0},
		{Name: "cores-round", Kind: "cores_to_milli", Input: "0.0005", RefMilli: 0, Expect: 1, Tolerance: 1, Note: "half milli rounds up"},
		// Weight scale conversions.
		{Name: "permille-350", Kind: "permille_to_bps", Input: "350", RefMilli: 0, Expect: 3500, Tolerance: 0},
		{Name: "bps-3500", Kind: "bps_to_permille", Input: "3500", RefMilli: 0, Expect: 350, Tolerance: 0},
		{Name: "bps-fractional", Kind: "bps_to_permille", Input: "3505", RefMilli: 0, Expect: -1, Tolerance: 0, Note: "non-integral permille rejected (Expect=-1 marks expected rejection)"},
		// Memory.
		{Name: "mem-gi", Kind: "memory_to_bytes", Input: "1Gi", RefMilli: 0, Expect: 1 << 30, Tolerance: 0},
		{Name: "mem-mi", Kind: "memory_to_bytes", Input: "512Mi", RefMilli: 0, Expect: 512 << 20, Tolerance: 0},
		// Tick conversion.
		{Name: "tick-15", Kind: "tick_to_ms", Input: "15", RefMilli: 0, Expect: 1500, Tolerance: 0},
	}
}

func buildEdits(spec *canonSpec) []Edit {
	if len(spec.nodes) == 0 || len(spec.pods) == 0 {
		return nil
	}
	n0 := spec.nodes[0]
	p0 := spec.pods[0]
	edits := []Edit{
		{Kind: "label_change", Target: n0.id, Key: labelApp, OldValue: n0.labels[labelApp], NewValue: "batch"},
		{Kind: "topology_move", Target: n0.id, Key: rackKey, OldValue: n0.topology[rackKey], NewValue: n0.topology[rackKey] + "-moved"},
	}
	if len(p0.preferRules) > 0 {
		r0 := p0.preferRules[0]
		edits = append(edits,
			Edit{Kind: "pod_request", Target: p0.id, Key: "cpu_milli", OldValue: itoa64(p0.cpuMilli), NewValue: itoa64(p0.cpuMilli + 160)},
			Edit{Kind: "rule_weight", Target: r0.id, Key: "weight_bps", OldValue: itoa64(r0.weightBps), NewValue: itoa64(min64(r0.weightBps+1000, 10000))},
		)
	}
	return edits
}

func itoa64(v int64) string {
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

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

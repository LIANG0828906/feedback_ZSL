package kernel

import "fmt"

type Bundle struct {
	Project  *Project        `json:"project"`
	V1       map[string]any  `json:"v1"`
	V2       map[string]any  `json:"v2"`
	V3       map[string]any  `json:"v3"`
	Truth    *RunReport      `json:"truth,omitempty"`
	Critical *CriticalReport `json:"critical,omitempty"`
}

type CriticalCase struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	TenantID        ID     `json:"tenant_id,omitempty"`
	NodeID          ID     `json:"node_id,omitempty"`
	Input           int64  `json:"input"`
	Expected        int64  `json:"expected"`
	Got             int64  `json:"got"`
	Saturated       bool   `json:"saturated,omitempty"`
	Code            string `json:"code,omitempty"`
	WithinTolerance bool   `json:"within_tolerance"`
	AvailableQuota  int64  `json:"available_quota_bytes,omitempty"`
	GuaranteeRemain int64  `json:"guarantee_remaining_bytes,omitempty"`
	BurstRemaining  int64  `json:"burst_remaining_bytes,omitempty"`
}

type CriticalReport struct {
	Seed   int64          `json:"seed"`
	Policy FixedPolicy    `json:"policy"`
	Cases  []CriticalCase `json:"cases"`
}

func mustKiB(kib int64) int64 {
	v, err := KiBIntegerToBytes(kib)
	if err != nil {
		panic(err)
	}
	return v
}

// SplitMix64 is used instead of math/rand so sample generation remains stable
// across Go releases and host architectures.
type splitMix64 struct{ state uint64 }

func (r *splitMix64) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func GenerateProject(seed int64) *Project {
	rng := splitMix64{state: uint64(seed)}
	_ = rng.next() // Seed is mixed into the stable stream; budgets are explicit below.

	p := &Project{
		SchemaVersion: SchemaCanonical,
		ProjectID:     fmt.Sprintf("seed-%d-multitenant-quota", seed),
		Seed:          seed,
		TickMS:        100,
		Policy:        DefaultFixedPolicy(),
		Nodes: []Node{
			{ID: "node-a", Rack: "rack-r1", CapacityBytes: mustKiB(4096), AllowedTenants: []ID{"t-alpha", "t-beta", "t-gamma", "t-delta"}},
			{ID: "node-b", Rack: "rack-r1", CapacityBytes: mustKiB(3072), AllowedTenants: []ID{"t-alpha", "t-beta", "t-delta"}},
			{ID: "node-c", Rack: "rack-r2", CapacityBytes: mustKiB(2832), AllowedTenants: []ID{"t-gamma", "t-delta"}},
		},
		Tenants: []Tenant{
			{ID: "t-alpha", Priority: 10, WeightPermyriad: 8000, QuotaBytes: mustKiB(3000), GuaranteeBytes: mustKiB(1500), BurstLimitBytes: mustKiB(750), GuaranteeNodeID: "node-a"},
			{ID: "t-beta", Priority: 8, WeightPermyriad: 6000, QuotaBytes: mustKiB(2000), GuaranteeBytes: mustKiB(1000), BurstLimitBytes: mustKiB(500), GuaranteeNodeID: "node-a"},
			{ID: "t-gamma", Priority: 5, WeightPermyriad: 4000, QuotaBytes: mustKiB(2500), GuaranteeBytes: mustKiB(500), BurstLimitBytes: mustKiB(625), GuaranteeNodeID: "node-a"},
			{ID: "t-delta", Priority: 5, WeightPermyriad: 2000, QuotaBytes: mustKiB(1500), GuaranteeBytes: mustKiB(500), BurstLimitBytes: mustKiB(375), GuaranteeNodeID: "node-a"},
		},
		Events: []Event{
			{ID: "e-alpha-open", Tick: 1, Type: EventTypeApply, TenantID: "t-alpha", NodeID: "node-a", AmountBytes: mustKiB(1024), BudgetMS: 4},
			{ID: "e-beta-open", Tick: 1, Type: EventTypeApply, TenantID: "t-beta", NodeID: "node-a", AmountBytes: mustKiB(768), BudgetMS: 4},
			{ID: "e-gamma-open", Tick: 1, Type: EventTypeApply, TenantID: "t-gamma", NodeID: "node-a", AmountBytes: mustKiB(256), BudgetMS: 2},

			{ID: "e-gamma-top-guarantee", Tick: 2, Type: EventTypeApply, TenantID: "t-gamma", NodeID: "node-a", AmountBytes: mustKiB(244), BudgetMS: 3, Refs: []string{"e-gamma-open"}},
			{ID: "e-delta-contend", Tick: 2, Type: EventTypeApply, TenantID: "t-delta", NodeID: "node-a", AmountBytes: mustKiB(1536), BudgetMS: 1},

			{ID: "e-alpha-expand", Tick: 3, Type: EventTypeApply, TenantID: "t-alpha", NodeID: "node-a", AmountBytes: mustKiB(1024), BudgetMS: 4, Refs: []string{"e-alpha-open"}},
			{ID: "e-beta-expand", Tick: 3, Type: EventTypeApply, TenantID: "t-beta", NodeID: "node-a", AmountBytes: mustKiB(768), BudgetMS: 4, Refs: []string{"e-beta-open"}},

			{ID: "e-beta-release-512", Tick: 4, Type: EventTypeRelease, TenantID: "t-beta", NodeID: "node-a", AmountBytes: mustKiB(512), BudgetMS: 2, Refs: []string{"e-beta-expand"}},
			{ID: "e-alpha-release-512", Tick: 5, Type: EventTypeRelease, TenantID: "t-alpha", NodeID: "node-a", AmountBytes: mustKiB(512), BudgetMS: 2, Refs: []string{"e-alpha-expand"}},
			{ID: "e-delta-weight-up", Tick: 5, Type: EventTypeWeight, TenantID: "t-delta", WeightPermyriad: 9000, BudgetMS: 1},

			{ID: "e-beta-release-60", Tick: 6, Type: EventTypeRelease, TenantID: "t-beta", NodeID: "node-a", AmountBytes: mustKiB(60), BudgetMS: 2},
			{ID: "e-alpha-release-36", Tick: 7, Type: EventTypeRelease, TenantID: "t-alpha", NodeID: "node-a", AmountBytes: mustKiB(36), BudgetMS: 2},

			{ID: "e-gamma-node-c", Tick: 8, Type: EventTypeApply, TenantID: "t-gamma", NodeID: "node-c", AmountBytes: mustKiB(256), BudgetMS: 3},
			{ID: "e-delta-node-c", Tick: 8, Type: EventTypeApply, TenantID: "t-delta", NodeID: "node-c", AmountBytes: mustKiB(256), BudgetMS: 1},
		},
	}
	return p
}

func GenerateBundle(seed int64) (*Bundle, error) {
	p := GenerateProject(seed)
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	v1, err := EncodeSchema(p, SchemaV1)
	if err != nil {
		return nil, err
	}
	v2, err := EncodeSchema(p, SchemaV2)
	if err != nil {
		return nil, err
	}
	v3, err := EncodeSchema(p, SchemaV3)
	if err != nil {
		return nil, err
	}
	truth, err := RunReference(p, DefaultBudgets())
	if err != nil {
		return nil, err
	}
	critical := GenerateCriticalReport(p)
	return &Bundle{Project: p, V1: v1, V2: v2, V3: v3, Truth: truth, Critical: critical}, nil
}

func GenerateCriticalReport(p *Project) *CriticalReport {
	r := &CriticalReport{Seed: p.Seed, Policy: p.Policy}

	add := func(c CriticalCase) {
		if c.Code == "" {
			c.WithinTolerance = WithinTolerance(c.Got, c.Expected, p.Policy)
		}
		r.Cases = append(r.Cases, c)
	}

	v, err := PermilleToPermyriad(1)
	add(CriticalCase{Name: "permille-one-to-bp", Kind: "scale", Input: 1, Expected: 10, Got: v, Code: codeIfErr(err)})
	v, err = PermilleToPermyriad(999)
	add(CriticalCase{Name: "permille-999-to-bp", Kind: "scale", Input: 999, Expected: 9990, Got: v, Code: codeIfErr(err)})
	_, err = PermilleToPermyriad(1 << 62)
	add(CriticalCase{Name: "permille-overflow", Kind: "scale-overflow", Input: 1 << 62, Expected: 0, Got: 0, Code: codeIfErr(err)})

	kibFixed, ferr := BytesToKiBFixed(1536, p.Policy.FractionBits)
	add(CriticalCase{Name: "bytes-1536-to-kib-q16", Kind: "unit", Input: 1536, Expected: int64(Fixed{Raw: 3 << (p.Policy.FractionBits - 1), FractionBits: p.Policy.FractionBits}.Raw), Got: kibFixed.Raw, Code: codeIfErr(ferr)})
	ms, terr := TicksToMilliseconds(7, p.TickMS)
	add(CriticalCase{Name: "tick-7-to-ms", Kind: "time", Input: 7, Expected: 700, Got: ms, Code: codeIfErr(terr)})
	tick, aligned, merr := MillisecondsToTick(750, p.TickMS)
	add(CriticalCase{Name: "ms-750-alignment", Kind: "time-alignment", Input: 750, Expected: 7, Got: tick, WithinTolerance: aligned && tick == 7, Code: codeIfErr(merr)})

	alpha, _ := p.Tenant("t-alpha")
	relativeLimit := alpha.QuotaBytes / 1_000_000 * p.Policy.TolerancePPM
	if rem := alpha.QuotaBytes % 1_000_000; rem != 0 {
		relativeLimit += rem * p.Policy.TolerancePPM / 1_000_000
	}
	limit := max64(p.Policy.ToleranceBytes, relativeLimit)
	driftGot := alpha.QuotaBytes + limit + 1
	driftWithin := WithinTolerance(driftGot, alpha.QuotaBytes, p.Policy)
	add(CriticalCase{
		Name: "critical-quota-drift-locator", Kind: "tolerance-drift",
		TenantID: alpha.ID, NodeID: alpha.GuaranteeNodeID,
		Input: alpha.QuotaBytes, Expected: alpha.QuotaBytes, Got: driftGot,
		Code: CodeTolerance, WithinTolerance: driftWithin,
		AvailableQuota: max64(0, driftGot), GuaranteeRemain: alpha.GuaranteeBytes,
		BurstRemaining: alpha.BurstLimitBytes,
	})

	for _, t := range p.Tenants {
		available := t.QuotaBytes
		guarantee := t.GuaranteeBytes
		burst := t.BurstLimitBytes
		add(CriticalCase{
			Name:            "declaration-" + string(t.ID),
			Kind:            "tenant-declaration",
			TenantID:        t.ID,
			NodeID:          t.GuaranteeNodeID,
			Input:           t.QuotaBytes,
			Expected:        t.QuotaBytes,
			Got:             t.QuotaBytes,
			WithinTolerance: true,
			AvailableQuota:  available,
			GuaranteeRemain: guarantee,
			BurstRemaining:  burst,
		})
	}
	return r
}

func codeIfErr(err error) string {
	if err == nil {
		return ""
	}
	if be, ok := AsBizError(err); ok {
		return be.Code
	}
	return CodeInvalidJSON
}

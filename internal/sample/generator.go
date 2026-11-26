// Package sample contains the deterministic fixed-seed diagnostic generator.
package sample

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"

	"quota-cluster/internal/engine"
	"quota-cluster/internal/fixed"
	"quota-cluster/internal/model"
	"quota-cluster/internal/projectio"
)

const MiB int64 = 1024 * 1024

const defaultBurstPermille = 500

// DeterministicRNG is SplitMix64 so generation does not depend on Go's global
// rand source implementation.
type DeterministicRNG struct{ state uint64 }

func NewRNG(seed int64) *DeterministicRNG {
	return &DeterministicRNG{state: uint64(seed) + 0x9e3779b97f4a7c15}
}

func (r *DeterministicRNG) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *DeterministicRNG) Int63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(r.next() % uint64(n))
}

// CriticalCase is one fixed-point boundary diagnostic.
type CriticalCase struct {
	Name                    string           `json:"name"`
	Input                   map[string]int64 `json:"input"`
	Conversion              fixed.Result     `json:"conversion"`
	ExpectedBytes           int64            `json:"expected_bytes,omitempty"`
	AvailableQuotaBytes     int64            `json:"available_quota_bytes,omitempty"`
	GuaranteeRemainingBytes int64            `json:"guarantee_remaining_bytes,omitempty"`
	BurstAmountBytes        int64            `json:"burst_amount_bytes,omitempty"`
	AbsoluteErrorBytes      int64            `json:"absolute_error_bytes,omitempty"`
	DeclaredToleranceBytes  int64            `json:"declared_tolerance_bytes"`
	ExceedsTolerance        bool             `json:"exceeds_tolerance"`
	Status                  string           `json:"status"`
}

// Bundle contains all generated artifacts.
type Bundle struct {
	Seed          int64
	Project       *model.Project
	V1            []byte
	V2            []byte
	V3            []byte
	Truth         *model.TruthFile
	TruthJSON     []byte
	CriticalCases []CriticalCase
	CriticalJSON  []byte
}

func mib(v int64) int64 { return v * MiB }

// Generate builds the same canonical content for every requested seed; seed
// changes only the deterministic background jitter and identifiers.
func Generate(seed int64) (*Bundle, error) {
	rng := NewRNG(seed)
	backgroundMiB := 80 + rng.Int63n(40)
	backgroundWeight := 110 + rng.Int63n(30)
	backgroundBytes := mib(backgroundMiB)

	p := &model.Project{
		ProjectID: fmt.Sprintf("quota-diagnostic-seed-%d", seed),
		Seed:      seed, LogicalTicks: 8,
		Policy: model.Policy{
			TickMillis: 10, RatioBasisBytes: mib(10000), ToleranceBytes: 1,
			DefaultBudgetMS: 5, FastCostMS: 1, PreciseCostMS: 2,
			WorkerBudget: 4, TaskQueueCapacity: 16, TenantQueueLimit: 2,
			RetryBackoffTicks: 2,
		},
		Nodes: []model.Node{
			{ID: "node-a", Rack: "rack-r1", CapacityBytes: mib(1024), AllowedTenants: []string{"tenant-alpha", "tenant-beta", "tenant-gamma", "tenant-delta", "tenant-epsilon"}},
			{ID: "node-b", Rack: "rack-r1", CapacityBytes: mib(768), AllowedTenants: []string{"tenant-zeta"}},
			{ID: "node-c", Rack: "rack-r2", CapacityBytes: mib(512), AllowedTenants: []string{"tenant-eta"}},
			{ID: "node-q", Rack: "rack-r3", CapacityBytes: mib(128), AllowedTenants: []string{"tenant-theta"}},
		},
		Tenants: []model.Tenant{
			{ID: "tenant-alpha", Priority: 3, InitialWeight: 100, QuotaBytes: mib(2500), GuaranteeBytes: mib(300), BurstLimitBytes: mib(1250), HomeNodeID: "node-a"},
			{ID: "tenant-beta", Priority: 4, InitialWeight: 100, QuotaBytes: mib(2000), GuaranteeBytes: mib(200), BurstLimitBytes: mib(1000), HomeNodeID: "node-a"},
			{ID: "tenant-gamma", Priority: 2, InitialWeight: 100, QuotaBytes: mib(1800), GuaranteeBytes: mib(150), BurstLimitBytes: mib(900), HomeNodeID: "node-a"},
			{ID: "tenant-delta", Priority: 1, InitialWeight: 100, QuotaBytes: mib(1500), GuaranteeBytes: mib(100), BurstLimitBytes: mib(750), HomeNodeID: "node-a"},
			{ID: "tenant-epsilon", Priority: 9, InitialWeight: 100, QuotaBytes: mib(3000), GuaranteeBytes: mib(100), BurstLimitBytes: mib(1500), HomeNodeID: "node-a"},
			{ID: "tenant-zeta", Priority: 2, InitialWeight: 100, QuotaBytes: mib(1000), GuaranteeBytes: mib(100), BurstLimitBytes: mib(500), HomeNodeID: "node-b"},
			{ID: "tenant-eta", Priority: 1, InitialWeight: 100, QuotaBytes: mib(800), GuaranteeBytes: mib(80), BurstLimitBytes: mib(400), HomeNodeID: "node-c"},
			{ID: "tenant-theta", Priority: 0, InitialWeight: 100, QuotaBytes: mib(1000), GuaranteeBytes: mib(100), BurstLimitBytes: mib(500), HomeNodeID: "node-q"},
		},
	}

	seq := 0
	add := func(tick int64, kind model.EventKind) model.Event {
		seq++
		return model.Event{Sequence: seq, Tick: tick, Kind: kind, LatencyBudgetMS: 5}
	}
	events := []model.Event{}

	e := add(1, model.EventApply)
	e.RequestID, e.NodeID, e.TenantID, e.AmountBytes = "req-epsilon-pre", "node-a", "tenant-epsilon", mib(200)
	events = append(events, e)
	e = add(1, model.EventApply)
	e.RequestID, e.NodeID, e.TenantID, e.AmountBytes = "req-zeta-background", "node-b", "tenant-zeta", backgroundBytes
	events = append(events, e)
	e = add(1, model.EventApply)
	e.RequestID, e.NodeID, e.TenantID, e.AmountBytes = "req-eta-background", "node-c", "tenant-eta", mib(100)
	events = append(events, e)

	e = add(2, model.EventWeight)
	e.TenantID, e.Weight = "tenant-alpha", 120
	events = append(events, e)
	e = add(2, model.EventWeight)
	e.TenantID, e.Weight = "tenant-zeta", backgroundWeight
	events = append(events, e)

	contention := []struct {
		req, tenant string
		amount      int64
	}{
		{"req-beta", "tenant-beta", 300},
		{"req-alpha", "tenant-alpha", 400},
		{"req-gamma", "tenant-gamma", 200},
		{"req-delta", "tenant-delta", 300},
		{"req-epsilon", "tenant-epsilon", 400},
	}
	for _, c := range contention {
		e = add(3, model.EventApply)
		e.RequestID, e.NodeID, e.TenantID, e.AmountBytes = c.req, "node-a", c.tenant, mib(c.amount)
		events = append(events, e)
	}
	for i, amount := range []int64{200, 200, 200} {
		e = add(3, model.EventApply)
		e.RequestID = fmt.Sprintf("req-theta-%d", i+1)
		e.NodeID, e.TenantID, e.AmountBytes = "node-q", "tenant-theta", mib(amount)
		events = append(events, e)
	}

	e = add(4, model.EventRelease)
	e.ReleaseRequestID, e.AmountBytes = "req-epsilon-pre", mib(200)
	events = append(events, e)
	e = add(4, model.EventRelease)
	e.ReleaseRequestID, e.AmountBytes = "req-zeta-background", backgroundBytes
	events = append(events, e)

	e = add(5, model.EventRelease)
	e.ReleaseRequestID, e.AmountBytes = "req-beta", mib(274)
	events = append(events, e)
	e = add(6, model.EventRelease)
	e.ReleaseRequestID, e.AmountBytes = "req-alpha", mib(300)
	events = append(events, e)
	e = add(7, model.EventRelease)
	e.ReleaseRequestID, e.AmountBytes = "req-gamma", mib(150)
	events = append(events, e)

	p.Events = events

	report, err := engine.Run(p, engine.Options{Workers: p.Policy.WorkerBudget, BatchSize: 2})
	if err != nil {
		return nil, fmt.Errorf("generate truth: %w", err)
	}
	fairnessOrder := make([]string, 0, len(report.Decisions))
	for _, d := range report.Decisions {
		fairnessOrder = append(fairnessOrder, d.RequestID)
	}
	truth := &model.TruthFile{
		SchemaVersion: "quota.truth.v1", Seed: seed, ProjectID: p.ProjectID,
		ToleranceBytes: p.Policy.ToleranceBytes, Ticks: report.Ticks,
		Decisions: report.Decisions, FairnessOrder: fairnessOrder,
	}

	v1, err := projectio.MarshalV1(p)
	if err != nil {
		return nil, err
	}
	v2, err := projectio.MarshalV2(p)
	if err != nil {
		return nil, err
	}
	v3, err := projectio.MarshalV3(p)
	if err != nil {
		return nil, err
	}
	truthJSON, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	cases := BuildCriticalCases(p)
	criticalJSON, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		return nil, err
	}

	return &Bundle{Seed: seed, Project: p, V1: v1, V2: v2, V3: v3, Truth: truth, TruthJSON: truthJSON, CriticalCases: cases, CriticalJSON: criticalJSON}, nil
}

// SeedBytes returns an 8-byte big endian representation useful in reports.
func SeedBytes(seed int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(seed))
	return b[:]
}

// BuildCriticalCases exercises exact boundaries, fractional rounding, and
// explicit saturation/overflow detection.
func BuildCriticalCases(p *model.Project) []CriticalCase {
	tol := p.Policy.ToleranceBytes
	alpha := p.Tenants[0]
	cases := []CriticalCase{
		{
			Name:          "v1_permille_to_v2_basis_points_alpha_quota",
			Input:         map[string]int64{"permille": 250},
			Conversion:    fixed.ScalePermilleToBasisPoints(250),
			ExpectedBytes: alpha.QuotaBytes, AvailableQuotaBytes: alpha.QuotaBytes,
			GuaranteeRemainingBytes: alpha.GuaranteeBytes, BurstAmountBytes: alpha.BurstLimitBytes,
			DeclaredToleranceBytes: tol, Status: "exact",
		},
		{
			Name:       "bytes_to_kib_non_divisible_boundary",
			Input:      map[string]int64{"bytes": 1025, "fixed_scale": fixed.FractionScale},
			Conversion: fixed.BytesToKiB(1025), ExpectedBytes: 1025,
			AbsoluteErrorBytes: 1, DeclaredToleranceBytes: tol,
			ExceedsTolerance: !fixed.WithinByteTolerance(1025, 1025, tol),
			Status:           "rounded_within_tolerance",
		},
		{
			Name:                   "tick_millis_fractional_alignment",
			Input:                  map[string]int64{"millis": 15, "tick_millis": 10, "fixed_scale": fixed.FractionScale},
			Conversion:             fixed.MillisToTicksFixed(15, 10),
			DeclaredToleranceBytes: 0, Status: "fractional_tick_diagnostic_only",
		},
		{
			Name:          "quota_ratio_last_basis_point",
			Input:         map[string]int64{"numerator": 9999, "scale": 10000, "basis_bytes": p.Policy.RatioBasisBytes},
			Conversion:    fixed.RatioToBytes(9999, 10000, p.Policy.RatioBasisBytes),
			ExpectedBytes: mib(9999), AvailableQuotaBytes: mib(9999),
			DeclaredToleranceBytes: tol, Status: "exact",
		},
		{
			Name:                   "permille_overflow_saturates",
			Input:                  map[string]int64{"permille": math.MaxInt64},
			Conversion:             fixed.ScalePermilleToBasisPoints(math.MaxInt64),
			DeclaredToleranceBytes: tol, ExceedsTolerance: true,
			Status: "overflow_saturated_rejected_by_loader",
		},
		{
			Name:                   "kib_to_bytes_overflow_saturates",
			Input:                  map[string]int64{"kib": math.MaxInt64},
			Conversion:             fixed.KiBToBytes(math.MaxInt64),
			DeclaredToleranceBytes: tol, ExceedsTolerance: true,
			Status: "overflow_saturated_rejected_by_loader",
		},
	}
	for i := range cases {
		c := &cases[i]
		if c.Name == "bytes_to_kib_non_divisible_boundary" {
			reconstructed := c.Conversion.Value * 1024 / fixed.FractionScale
			diff := abs64(reconstructed - c.ExpectedBytes)
			c.AbsoluteErrorBytes = diff
			c.ExceedsTolerance = !fixed.WithinByteTolerance(diff, 0, tol)
		}
		if c.Conversion.Overflow {
			c.ExceedsTolerance = true
		}
	}
	for _, tenant := range p.Tenants {
		quotaBP := tenant.QuotaBytes * 10000 / p.Policy.RatioBasisBytes
		guaranteeBP := tenant.GuaranteeBytes * 10000 / p.Policy.RatioBasisBytes
		quota := fixed.RatioToBytes(quotaBP, 10000, p.Policy.RatioBasisBytes)
		guarantee := fixed.RatioToBytes(guaranteeBP, 10000, p.Policy.RatioBasisBytes)
		burst := fixed.RatioToBytes(defaultBurstPermille, 1000, quota.Value)
		errBytes := maxAbs64(
			abs64(quota.Value-tenant.QuotaBytes),
			abs64(guarantee.Value-tenant.GuaranteeBytes),
			abs64(burst.Value-tenant.BurstLimitBytes),
		)
		cases = append(cases, CriticalCase{
			Name:                    "tenant_fixed_point_values_" + tenant.ID,
			Input:                   map[string]int64{"quota_basis_points": quotaBP, "guarantee_basis_points": guaranteeBP, "burst_permille_of_quota": defaultBurstPermille},
			Conversion:              quota,
			ExpectedBytes:           tenant.QuotaBytes,
			AvailableQuotaBytes:     quota.Value,
			GuaranteeRemainingBytes: guarantee.Value,
			BurstAmountBytes:        burst.Value,
			AbsoluteErrorBytes:      errBytes,
			DeclaredToleranceBytes:  tol,
			ExceedsTolerance:        !fixed.WithinByteTolerance(errBytes, 0, tol),
			Status:                  "tenant_values_within_declared_tolerance",
		})
	}
	return cases
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func maxAbs64(values ...int64) int64 {
	best := int64(0)
	for _, v := range values {
		if v > best {
			best = v
		}
	}
	return best
}

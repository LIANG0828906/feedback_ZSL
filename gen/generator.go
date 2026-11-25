package gen

import (
	"fmt"

	"quotaengine/quota"
)

type Generated struct {
	Project   *quota.Project
	Documents Documents
	Truth     Truth
}

type Truth struct {
	Seed           int64            `json:"seed"`
	ToleranceMilli int64            `json:"tolerance_millicores"`
	Steps          []quota.TimeStep `json:"steps"`
}

func (t Truth) Timeline() *quota.Timeline {
	final := quota.NewState(&quota.Project{})
	if len(t.Steps) > 0 {
		final = t.Steps[len(t.Steps)-1].State.Clone()
	}
	return &quota.Timeline{Steps: t.Steps, FinalState: final}
}

func (t Truth) Check(run *quota.Timeline) error {
	return quota.CompareTimelines(run, t.Timeline())
}

// Generate returns one semantic project encoded in three schemas plus truth
// produced by the independent reference state machine in reference.go.
func Generate(seed int64) (Generated, error) {
	p, err := buildProject(seed)
	if err != nil {
		return Generated{}, err
	}
	if err := p.Validate(); err != nil {
		return Generated{}, err
	}
	docs, err := renderDocuments(p)
	if err != nil {
		return Generated{}, err
	}
	truth, err := buildTruth(p)
	if err != nil {
		return Generated{}, err
	}
	return Generated{Project: p, Documents: docs, Truth: truth}, nil
}

func buildProject(seed int64) (*quota.Project, error) {
	// All ratios are multiples of 10 so the permille v1 encoding is exact.
	tenants := []quota.Tenant{
		{ID: "t-alpha", QuotaBP: 2500, GuaranteeBP: 1000, BurstBP: 800, Priority: 2},
		{ID: "t-beta", QuotaBP: 2000, GuaranteeBP: 1000, BurstBP: 600, Priority: 1},
		{ID: "t-gamma", QuotaBP: 1500, GuaranteeBP: 800, BurstBP: 0, Priority: 4},
		{ID: "t-delta", QuotaBP: 3000, GuaranteeBP: 1200, BurstBP: 700, Priority: 3},
	}
	all := []string{"t-alpha", "t-beta", "t-gamma", "t-delta"}
	nodes := []quota.Node{
		{ID: "n-core-1", CapacityMilli: 4000, RackID: "rack-a", AllowedTenants: append([]string(nil), all...)},
		{ID: "n-core-2", CapacityMilli: 3000, RackID: "rack-a", AllowedTenants: append([]string(nil), all...)},
		{ID: "n-edge-1", CapacityMilli: 2000, RackID: "rack-b", AllowedTenants: []string{"t-alpha", "t-beta", "t-delta"}},
	}
	events := []quota.Event{
		{Seq: 1, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-alpha", NodeID: "n-core-1", AmountMilli: 1000},
		{Seq: 2, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-beta", NodeID: "n-core-1", AmountMilli: 1000},
		{Seq: 3, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-gamma", NodeID: "n-core-1", AmountMilli: 1000},
		{Seq: 4, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-delta", NodeID: "n-core-1", AmountMilli: 1000},
		{Seq: 5, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-alpha", NodeID: "n-core-2", AmountMilli: 1000},
		{Seq: 6, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-gamma", NodeID: "n-core-2", AmountMilli: 1000},
		{Seq: 7, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-beta", NodeID: "n-core-2", AmountMilli: 1000},
		{Seq: 8, TimeMilli: 1000, Type: quota.EventRelease, TenantID: "t-delta", NodeID: "n-core-1", AmountMilli: 1000},
		{Seq: 9, TimeMilli: 1000, Type: quota.EventRelease, TenantID: "t-alpha", NodeID: "n-core-2", AmountMilli: 1000},
		{Seq: 11, TimeMilli: 2000, Type: quota.EventWeight, TenantID: "t-beta", WeightPermille: 2000},
		{Seq: 12, TimeMilli: 3000, Type: quota.EventWeight, TenantID: "t-gamma", WeightPermille: 2500},
		{Seq: 13, TimeMilli: 3000, Type: quota.EventRequest, TenantID: "t-alpha", NodeID: "n-core-1", AmountMilli: 1000},
		{Seq: 14, TimeMilli: 3000, Type: quota.EventRequest, TenantID: "t-delta", NodeID: "n-core-1", AmountMilli: 1000},
		{Seq: 15, TimeMilli: 3000, Type: quota.EventRequest, TenantID: "t-beta", NodeID: "n-core-2", AmountMilli: 1000},
		{Seq: 16, TimeMilli: 4000, Type: quota.EventRelease, TenantID: "t-beta", NodeID: "n-core-1", AmountMilli: 1000},
		{Seq: 17, TimeMilli: 4000, Type: quota.EventRelease, TenantID: "t-alpha", NodeID: "n-core-1", AmountMilli: 1000},
		{Seq: 18, TimeMilli: 4000, Type: quota.EventRequest, TenantID: "t-delta", NodeID: "n-core-1", AmountMilli: 1000},
		{Seq: 19, TimeMilli: 5000, Type: quota.EventWeight, TenantID: "t-delta", WeightPermille: 4000},
		{Seq: 20, TimeMilli: 6000, Type: quota.EventRelease, TenantID: "t-beta", NodeID: "n-core-2", AmountMilli: 1000},
		{Seq: 21, TimeMilli: 6000, Type: quota.EventRelease, TenantID: "t-delta", NodeID: "n-core-1", AmountMilli: 1000},
	}
	if seed != 42 {
		// The seed is part of the deterministic artifact identity. A harmless
		// demand variation makes different seeds observably different while
		// retaining exact v1 core-unit encodings.
		delta := (seed % 2) * 1000
		events[3].AmountMilli += delta
	}
	p := &quota.Project{SchemaVersion: "canonical", Seed: seed, Tenants: tenants, Nodes: nodes, Events: events}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated project is invalid: %w", err)
	}
	return p, nil
}

// CriticalConversions returns boundary fixed-point examples. Each truth is
// derived directly from the declared integer formula, not from the engine.
func CriticalConversions() []struct {
	CaseID         string
	CapacityMilli  int64
	RatioBP        int64
	WeightPermille int64
	Truth          int64
} {
	return []struct {
		CaseID         string
		CapacityMilli  int64
		RatioBP        int64
		WeightPermille int64
		Truth          int64
	}{
		{CaseID: "exact-quarter", CapacityMilli: 4000, RatioBP: 2500, WeightPermille: 1000, Truth: 1000},
		{CaseID: "round-down-floor", CapacityMilli: 4000, RatioBP: 2501, WeightPermille: 1000, Truth: 1000},
		{CaseID: "weighted-round-down", CapacityMilli: 4000, RatioBP: 1500, WeightPermille: 2500, Truth: 1500},
		{CaseID: "one-millicore", CapacityMilli: 1, RatioBP: 10000, WeightPermille: 1000, Truth: 1},
		{CaseID: "permille-overflow", CapacityMilli: 1<<62 - 1, RatioBP: 10000, WeightPermille: 1000, Truth: 1<<62 - 1},
	}
}

package solver

import (
	"testing"

	"maintenanceorchestrator/internal/domain"
)

func TestOversizedInstanceReportsHardCapacityReason(t *testing.T) {
	p := domain.Project{
		Nodes: []domain.Node{
			{ID: "source", CapacityMilli: 32000, WatermarkBasisPoints: 7500},
			{ID: "target", CapacityMilli: 8000, InitialUsedMilli: 5000, WatermarkBasisPoints: 7500},
		},
		Instances: []domain.Instance{
			{ID: "big", NodeID: "source", MilliCores: 12000, Priority: 1, DeclaredOrder: 1},
			{ID: "target-load", NodeID: "target", MilliCores: 5000},
		},
	}
	report := EvaluateTick(FeasibilityOptions{Project: p, Tick: 1, Pending: map[string]bool{"source": true}})
	if len(report.Nodes) != 1 {
		t.Fatalf("nodes=%d", len(report.Nodes))
	}
	d := report.Nodes[0].Decisions[0]
	if d.Movable || d.ReasonCode != ReasonTargetCapacity || d.ConstraintID != domain.EConstraintCapacity {
		t.Fatalf("decision=%+v", d)
	}
}

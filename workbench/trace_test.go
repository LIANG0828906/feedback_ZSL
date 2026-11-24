package workbench

import (
	"context"
	"testing"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/generator"
)

func TestThreePathsProduceIdenticalResults(t *testing.T) {
	project := generator.GenerateProjects(generator.DefaultSeed)[0]
	seg := project.Segments[0]
	ch, _, _ := seg.ChannelByID("velocity.main")
	point := ch.Layers[0].Points[1]
	trace := Trace{
		Name: "unit-trace",
		Operations: []domain.EditOp{
			{
				Kind: domain.EditMovePoint, SegmentID: seg.ID, ChannelID: ch.ID, LayerID: ch.Layers[0].ID,
				PointIndex: 1, Beat: point.Beat, Value: point.Value + 0.02, BatchID: "unit-batch-1",
			},
		},
	}
	report, err := RunThreePaths(context.Background(), project, domain.DefaultParameters(), trace)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Equal || len(report.Paths) != 3 {
		t.Fatalf("three paths differ: %+v", report)
	}
	for _, comparison := range report.Comparisons {
		if !comparison.Equal || comparison.Differences != 0 {
			t.Fatalf("comparison failed: %+v", comparison)
		}
	}
	resume := report.Paths[2]
	if resume.Name != "checkpoint-resume" || resume.Recovery == nil || resume.Recovery.LoadedBatches != 1 {
		t.Fatalf("resume path did not load checkpoint: %+v", resume)
	}
}

func TestThreePathsRejectUnbatchedTrace(t *testing.T) {
	project := generator.GenerateProjects(generator.DefaultSeed)[0]
	trace := Trace{Name: "bad", Operations: []domain.EditOp{{Kind: domain.EditSetPriority, SegmentID: project.Segments[0].ID}}}
	_, err := RunThreePaths(context.Background(), project, domain.DefaultParameters(), trace)
	if domain.CodeOf(err) != domain.CodeSchema {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
}

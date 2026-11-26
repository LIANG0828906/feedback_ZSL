package strokelab

import (
	"context"
	"testing"
)

func TestTopologyFailureAndStrokeAttribution(t *testing.T) {
	suite, err := GenerateSuite(7171)
	if err != nil {
		t.Fatal(err)
	}
	project := suite.Samples[2].Project
	weight := 1500
	edited, err := ApplyEdit(project, Edit{StrokeID: project.Strokes[0].ID, Weight: &weight})
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := NewEngine(edited)
	eval, err := engine.Run(context.Background(), RunOptions{Mode: RunFull})
	if ErrorCode(err) != CodeTopology {
		t.Fatalf("expected E_TOP, got %v", err)
	}
	if eval.Topology == nil || eval.Topology.Passed || len(eval.Topology.Issues) == 0 {
		t.Fatalf("topology failure report missing: %+v", eval.Topology)
	}
	if eval.Attribution == nil || len(eval.Attribution.Differences) == 0 {
		t.Fatalf("attribution missing")
	}
	foundStroke := false
	for _, c := range eval.Attribution.Culprits {
		if c.StrokeID == project.Strokes[0].ID {
			foundStroke = true
		}
	}
	if !foundStroke {
		t.Fatalf("edited stroke was not attributed: %+v", eval.Attribution.Culprits)
	}
	for _, d := range eval.Attribution.Differences {
		if d.ContributionShare < 0 || d.ContributionShare > WeightMax {
			t.Fatalf("invalid contribution share %d", d.ContributionShare)
		}
	}
}

func TestTopologyHoleAndRingIssuesIncludeMeasuredValues(t *testing.T) {
	suite, err := GenerateSuite(8181)
	if err != nil {
		t.Fatal(err)
	}
	project := suite.Samples[2].Project.Clone()
	project.Truth.Domains[0].Holes++
	project.Truth.Domains[0].RingWidthMilli += 5000
	engine, _ := NewEngine(project)
	eval, err := engine.Run(context.Background(), RunOptions{Mode: RunFull})
	if ErrorCode(err) != CodeTopology {
		t.Fatalf("expected E_TOP, got %v", err)
	}
	var holes, ring bool
	for _, issue := range eval.Topology.Issues {
		if issue.Metric == "holes" && issue.Actual > 0 {
			holes = true
		}
		if issue.Metric == "ring_width_milli" && issue.Actual > 0 {
			ring = true
		}
	}
	if !holes || !ring {
		t.Fatalf("missing measured hole/ring issue: %+v", eval.Topology.Issues)
	}
}

func TestSchedulingAndDeclarationOrderDoNotChangePixelsOrAttribution(t *testing.T) {
	suite, err := GenerateSuite(9191)
	if err != nil {
		t.Fatal(err)
	}
	project := suite.Samples[2].Project
	weight := 1700
	edited, _ := ApplyEdit(project, Edit{StrokeID: project.Strokes[0].ID, Weight: &weight})
	run := func(p *Project, cfg SchedulerConfig) *Evaluation {
		engine, _ := NewEngine(p)
		eval, err := engine.Run(context.Background(), RunOptions{Mode: RunFull, Scheduler: cfg})
		if ErrorCode(err) != CodeTopology {
			t.Fatalf("run: %v", err)
		}
		return eval
	}
	a := run(edited, SchedulerConfig{MaxParallelism: 1, MaxPixelsPerWorker: 4096, QueueBudget: 1, MaxRetries: 0})
	b := run(edited, SchedulerConfig{MaxParallelism: 4, MaxPixelsPerWorker: 4096, QueueBudget: 32, MaxRetries: 1})
	if !masksEqual(a.Composite, b.Composite) {
		t.Fatalf("scheduler changed composite")
	}
	if !attributionEqual(a.Attribution, b.Attribution) {
		t.Fatalf("scheduler changed attribution")
	}
	shuffled := edited.Clone()
	for i, j := 0, len(shuffled.Strokes)-1; i < j; i, j = i+1, j-1 {
		shuffled.Strokes[i], shuffled.Strokes[j] = shuffled.Strokes[j], shuffled.Strokes[i]
	}
	for i := range shuffled.Strokes {
		shuffled.Strokes[i].DeclOrder = i
	}
	c := run(shuffled, SchedulerConfig{MaxParallelism: 2, MaxPixelsPerWorker: 4096, QueueBudget: 8, MaxRetries: 0})
	if !masksEqual(a.Composite, c.Composite) {
		t.Fatalf("declaration order changed composite")
	}
	if !attributionEqual(a.Attribution, c.Attribution) {
		t.Fatalf("declaration order changed attribution")
	}
}

func TestOrderTieReturnsE_ORD(t *testing.T) {
	suite, err := GenerateSuite(1212)
	if err != nil {
		t.Fatal(err)
	}
	p := suite.Samples[2].Project.Clone()
	p.Strokes[1].Stage = p.Strokes[0].Stage
	p.Strokes[1].Seq = p.Strokes[0].Seq
	_, err = NewEngine(p)
	if ErrorCode(err) != CodeOrder {
		t.Fatalf("expected E_ORD, got %v", err)
	}
}

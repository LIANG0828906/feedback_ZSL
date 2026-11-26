package strokelab

import (
	"context"
	"testing"
)

func TestGenerateAndFullRunSmoke(t *testing.T) {
	suite, err := GenerateSuite(424242)
	if err != nil {
		t.Fatalf("generate suite: %v", err)
	}
	if len(suite.Samples) != 3 {
		t.Fatalf("got %d samples", len(suite.Samples))
	}
	for _, sample := range suite.Samples {
		engine, err := NewEngine(sample.Project)
		if err != nil {
			t.Fatalf("%s new engine: %v", sample.Name, err)
		}
		eval, err := engine.Run(context.Background(), RunOptions{Mode: RunFull})
		if err != nil {
			t.Fatalf("%s full run: %v", sample.Name, err)
		}
		if !masksEqual(eval.Composite, sample.CompositeMask) {
			t.Fatalf("%s composite differs from generated sample", sample.Name)
		}
		if eval.Topology == nil || !eval.Topology.Passed {
			t.Fatalf("%s baseline topology did not pass: %+v", sample.Name, eval.Topology)
		}
	}
}

func TestRunAcceptance(t *testing.T) {
	report, err := RunAcceptance(424242, t.TempDir())
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	if !report.AllPassed {
		t.Fatalf("acceptance report failed: %+v", report)
	}
}

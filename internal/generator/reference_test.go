package generator_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"quotaflow/internal/domain"
	"quotaflow/internal/engine"
	"quotaflow/internal/generator"
)

func TestReferenceTruthMatchesEngine(t *testing.T) {
	project := generator.Scenario(42)
	truth, err := generator.BuildTruth(project, 42, 64)
	if err != nil {
		t.Fatalf("build truth: %v", err)
	}
	eng, err := engine.New(project, engine.Config{ThreadBudget: 4, QueueLimit: 64})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if err := eng.RunAll(); err != nil {
		t.Fatalf("run engine: %v", err)
	}
	got := eng.Observations()
	if !reflect.DeepEqual(got, truth.Ticks) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(truth.Ticks, "", "  ")
		t.Fatalf("engine/truth mismatch\nengine:\n%s\ntruth:\n%s", gotJSON, wantJSON)
	}
}

func TestTruthContainsMigrationAndPause(t *testing.T) {
	project := generator.Scenario(42)
	truth, err := generator.BuildTruth(project, 42, 64)
	if err != nil {
		t.Fatalf("build truth: %v", err)
	}
	if len(truth.Migrations) == 0 || len(truth.PauseHistory) == 0 {
		t.Fatalf("scenario did not exercise migration pauses: migrations=%d pauses=%d", len(truth.Migrations), len(truth.PauseHistory))
	}
	var sawFailure domain.NodeObservation
	for _, tick := range truth.Ticks {
		for _, node := range tick.Nodes {
			if node.Failed {
				sawFailure = node
			}
		}
	}
	if sawFailure.NodeID == "" {
		t.Fatal("scenario did not exercise node failure")
	}
}

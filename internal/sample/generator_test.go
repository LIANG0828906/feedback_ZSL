package sample

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"tacticalworkbench/pkg/tactical"
)

func TestGeneratorIsDeterministic(t *testing.T) {
	first, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if !bytes.Equal(a, b) {
		t.Fatal("same seed produced different scenarios")
	}
	if _, err := tactical.ParseScenario(a); err != nil {
		t.Fatalf("generated scenario did not pass strict reload: %v", err)
	}
}

func TestGeneratedGroundTruthMatchesEngine(t *testing.T) {
	scenario, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotForScenario(scenario)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenario.GroundTruth.MinCosts) != len(scenario.Formation.Members) {
		t.Fatalf("unexpected truth coverage: %+v", scenario.GroundTruth)
	}
	for _, truth := range scenario.GroundTruth.MinCosts {
		member, _ := tactical.MemberByID(*scenario, truth.MemberID)
		path, err := tactical.SearchMember(*scenario, member, tactical.SearchOptions{Neighbors: 8, HeuristicWeightMilli: 1000})
		if err != nil {
			t.Fatal(err)
		}
		if path.Cost != truth.Cost || path.Cells[0] != truth.From || path.Cells[len(path.Cells)-1] != truth.To {
			t.Fatalf("truth mismatch for %s: truth=%+v path=%+v", truth.MemberID, truth, path)
		}
	}
	cornerIndex := 0
	for _, truth := range scenario.GroundTruth.PathLengths {
		path := snapshot.Paths[truth.MemberID]
		smooth := snapshot.Smooth[truth.MemberID]
		if truth.Cost != path.Cost || truth.Steps != len(path.Cells)-1 || truth.Corners != len(smooth.Corners) {
			t.Fatalf("path truth mismatch for %s: truth=%+v path=%+v corners=%d", truth.MemberID, truth, path, len(smooth.Corners))
		}
		for _, corner := range smooth.Corners {
			if cornerIndex >= len(scenario.GroundTruth.Corners) {
				t.Fatal("missing corner truth")
			}
			got := scenario.GroundTruth.Corners[cornerIndex]
			if got.MemberID != truth.MemberID || got.Grid != corner.Grid || got.World != corner.World {
				t.Fatalf("corner truth mismatch at %d: truth=%+v corner=%+v", cornerIndex, got, corner)
			}
			cornerIndex++
		}
	}
	if cornerIndex != len(scenario.GroundTruth.Corners) {
		t.Fatalf("unexpected extra corner truth rows: %d", len(scenario.GroundTruth.Corners)-cornerIndex)
	}
	for _, truth := range scenario.GroundTruth.FormationDeviations {
		trace := snapshot.Traces[truth.MemberID]
		if truth.Tick >= len(trace.States) {
			t.Fatalf("truth tick %d outside trace for %s", truth.Tick, truth.MemberID)
		}
		if got := trace.States[truth.Tick].FormationDeviationMilli; got != truth.DeviationMilli {
			t.Fatalf("deviation truth mismatch for %s tick %d: truth=%d got=%d", truth.MemberID, truth.Tick, truth.DeviationMilli, got)
		}
	}
}

func TestGeneratedScenarioRegionEditRemainsFormationValid(t *testing.T) {
	scenario, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := tactical.NewEngine(*scenario, tactical.EngineOptions{
		Search: tactical.SearchOptions{Neighbors: 8, HeuristicWeightMilli: 1000},
		Smoothing: tactical.SmoothingOptions{
			MaxSpacingMilli: scenario.Formation.MaxSpacingMilli, FormationSamples: 65,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.FullRebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := engine.ApplyRegionEdit(context.Background(), "bridge-lock", true)
	if err != nil {
		t.Fatalf("sample region edit failed: %v", err)
	}
	if !result.Consistency.Equal {
		t.Fatal("sample region edit was not consistent")
	}
}

package tactical_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"

	"tacticalworkbench/internal/sample"
	"tacticalworkbench/pkg/tactical"
)

func baseScenario(t *testing.T) *tactical.Scenario {
	t.Helper()
	scenario, err := sample.GenerateBase(sample.DefaultSeed)
	if err != nil {
		t.Fatalf("generate base scenario: %v", err)
	}
	return scenario
}

func cloneScenario(t *testing.T, scenario *tactical.Scenario) *tactical.Scenario {
	t.Helper()
	payload, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	var clone tactical.Scenario
	if err := json.Unmarshal(payload, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func requireCodeField(t *testing.T, err error, code tactical.Code, fieldFragment string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	typed, ok := tactical.AsError(err)
	if !ok {
		t.Fatalf("expected typed error %s, got %T: %v", code, err, err)
	}
	if typed.Code != code {
		t.Fatalf("expected code %s, got %s (%v)", code, typed.Code, err)
	}
	if fieldFragment != "" && !contains(typed.Field, fieldFragment) {
		t.Fatalf("expected field containing %q, got %q", fieldFragment, typed.Field)
	}
}

func contains(value, fragment string) bool {
	return len(fragment) == 0 || (len(value) >= len(fragment) && indexOf(value, fragment) >= 0)
}

func indexOf(value, fragment string) int {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return i
		}
	}
	return -1
}

func TestValidationRejectsMissingAndDuplicateFields(t *testing.T) {
	scenario := baseScenario(t)
	payload, _ := json.Marshal(scenario)
	var document map[string]any
	_ = json.Unmarshal(payload, &document)
	delete(document["map"].(map[string]any), "width")
	broken, _ := json.Marshal(document)
	_, err := tactical.ParseScenario(broken)
	requireCodeField(t, err, tactical.CodeSchema, "map.width")

	duplicate := cloneScenario(t, scenario)
	duplicate.Formation.Members[1].ID = duplicate.Formation.Members[0].ID
	err = tactical.ValidateScenario(duplicate)
	requireCodeField(t, err, tactical.CodeSchema, "formation.members")

	badOffset := cloneScenario(t, scenario)
	badOffset.Formation.Slots[1].DX = 40000
	err = tactical.ValidateScenario(badOffset)
	requireCodeField(t, err, tactical.CodeSchema, "formation.slots")

	nonFinite := cloneScenario(t, scenario)
	nonFinite.Formation.Slots[1].DY = math.Inf(1)
	err = tactical.ValidateScenario(nonFinite)
	requireCodeField(t, err, tactical.CodeSchema, "dy")
}

func TestValidationRejectsCellAndEndpointErrors(t *testing.T) {
	scenario := baseScenario(t)
	badCost := cloneScenario(t, scenario)
	idx := badCost.Map.Width*5 + 10
	badCost.Map.Cells[idx] = tactical.Cell{Cost: 0, Terrain: "road", Passable: true}
	err := tactical.ValidateScenario(badCost)
	requireCodeField(t, err, tactical.CodeSchema, "cost")

	badBlock := cloneScenario(t, scenario)
	badBlock.Map.Cells[idx] = tactical.Cell{Cost: 1, Terrain: "road", Passable: false}
	err = tactical.ValidateScenario(badBlock)
	requireCodeField(t, err, tactical.CodeSchema, "passable")

	badStart := cloneScenario(t, scenario)
	badStart.Formation.Members[0].Start.X = -1
	err = tactical.ValidateScenario(badStart)
	requireCodeField(t, err, tactical.CodeSchema, "start")
}

func TestSearchIsDeterministicAndOptimal(t *testing.T) {
	scenario := baseScenario(t)
	member, _ := tactical.MemberByID(*scenario, "A-LEAD")
	opts := tactical.SearchOptions{Neighbors: 8, HeuristicWeightMilli: 1000}
	first, err := tactical.SearchMember(*scenario, member, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tactical.SearchMember(*scenario, member, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Cells, second.Cells) || first.Cost != second.Cost {
		t.Fatalf("repeated searches differ: first=%v second=%v", first, second)
	}
	if first.Cost != 31 || len(first.Cells) != 32 {
		t.Fatalf("unexpected straight corridor route cost=%d cells=%d", first.Cost, len(first.Cells))
	}
	for i := 1; i < len(first.Cells); i++ {
		dx := first.Cells[i].X - first.Cells[i-1].X
		dy := first.Cells[i].Y - first.Cells[i-1].Y
		if abs(dx) > 1 || abs(dy) > 1 || (dx == 0 && dy == 0) {
			t.Fatalf("search produced a non-adjacent step at %d", i)
		}
	}
}

func TestHeuristicGuards(t *testing.T) {
	scenario := baseScenario(t)
	member, _ := tactical.MemberByID(*scenario, "A-LEAD")
	_, err := tactical.SearchMember(*scenario, member, tactical.SearchOptions{
		Neighbors: 8, HeuristicWeightMilli: 1001,
	})
	requireCodeField(t, err, tactical.CodeHeur, "")
	typed, _ := tactical.AsError(err)
	if typed.MemberID != member.ID {
		t.Fatalf("error did not identify member: %v", err)
	}

	_, err = tactical.SearchMember(*scenario, member, tactical.SearchOptions{
		Neighbors: 8, HeuristicWeightMilli: 1000,
		Heuristic: func(_, _ tactical.GridPoint, exact int64) int64 { return exact + 1 },
	})
	requireCodeField(t, err, tactical.CodeHeur, "")
	typed, _ = tactical.AsError(err)
	if typed.MemberID != member.ID {
		t.Fatalf("overestimate error did not identify member: %v", err)
	}

	_, err = tactical.SearchMember(*scenario, member, tactical.SearchOptions{
		Neighbors: 8, HeuristicWeightMilli: 1000, MaxCost: 1,
	})
	requireCodeField(t, err, tactical.CodeHeur, "")

	configured := cloneScenario(t, scenario)
	for i := range configured.Formation.Members {
		if configured.Formation.Members[i].ID == member.ID {
			configured.Formation.Members[i].HeuristicWeightMilli = 1001
		}
	}
	badEngine, err := tactical.NewEngine(*configured, tactical.EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = badEngine.FullRebuild(context.Background())
	requireCodeField(t, err, tactical.CodeHeur, "")
	typed, _ = tactical.AsError(err)
	if typed.MemberID != member.ID {
		t.Fatalf("configured overestimate error did not identify member: %v", err)
	}
}

func TestEngineHonorsContext(t *testing.T) {
	scenario := baseScenario(t)
	engine, err := tactical.NewEngine(*scenario, tactical.EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.FullRebuild(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestErrorFormatting(t *testing.T) {
	err := &tactical.Error{Code: tactical.CodeDiff, MemberID: "A", Tick: 3, Item: "trace.position", Message: fmt.Sprint("x")}
	if got := err.Error(); !contains(got, "E_DIFF") || !contains(got, "tick=3") {
		t.Fatalf("unexpected error string %q", got)
	}
}

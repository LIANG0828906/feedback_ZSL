package tactical_test

import (
	"context"
	"testing"

	"tacticalworkbench/pkg/tactical"
)

func TestSmoothingToleranceIsMonotonicAndPreservesCost(t *testing.T) {
	width, height := 14, 8
	cells := make([]tactical.Cell, width*height)
	for i := range cells {
		cells[i] = tactical.Cell{Cost: 1, Terrain: "road", Passable: true}
	}
	def := tactical.MapDef{ID: "smooth-fixture", Width: width, Height: height, Cells: cells}
	member := tactical.Member{ID: "S", Start: tactical.GridPoint{X: 1, Y: 1}, End: tactical.GridPoint{X: 12, Y: 5}}
	path := tactical.MemberPath{
		MemberID: "S", Cost: 99,
		Cells: []tactical.GridPoint{
			{X: 1, Y: 1}, {X: 2, Y: 2}, {X: 3, Y: 1}, {X: 4, Y: 2},
			{X: 5, Y: 1}, {X: 6, Y: 2}, {X: 7, Y: 3}, {X: 8, Y: 4},
			{X: 9, Y: 3}, {X: 10, Y: 4}, {X: 11, Y: 5}, {X: 12, Y: 5},
		},
	}
	lastCount := -1
	for _, tolerance := range []float64{0, 0.5, 1.5, 3.0} {
		slot := tactical.Slot{ID: "S", Anchor: "S", DX: 0, DY: 0, Tolerance: tolerance}
		smoothed, err := tactical.SmoothPath(def, member, slot, path, tactical.SmoothingOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if smoothed.SourceCost != path.Cost {
			t.Fatalf("smoothing changed source cost: %d -> %d", path.Cost, smoothed.SourceCost)
		}
		if lastCount >= 0 && len(smoothed.Corners) > lastCount {
			t.Fatalf("corner count increased when tolerance loosened: %d -> %d", lastCount, len(smoothed.Corners))
		}
		lastCount = len(smoothed.Corners)
		for _, corner := range smoothed.Corners {
			if corner.World.X != float64(corner.Fixed.X)/1000 || corner.World.Y != float64(corner.Fixed.Y)/1000 {
				t.Fatalf("corner world readout does not match fixed point: %+v", corner)
			}
		}
	}
	if lastCount < 2 {
		t.Fatalf("expected at least endpoints, got %d corners", lastCount)
	}
}

func TestFormationSpacingReturnsEForm(t *testing.T) {
	scenario := baseScenario(t)
	smooth := smoothFixture(t, scenario)
	err := tactical.CheckFormation(*scenario, smooth, tactical.SmoothingOptions{MaxSpacingMilli: 100})
	requireCodeField(t, err, tactical.CodeForm, "")
	typed, _ := tactical.AsError(err)
	if typed.Pair[0] == "" || typed.Pair[1] == "" {
		t.Fatalf("formation error did not include a member pair: %v", err)
	}
	if typed.DistanceMilli == 0 {
		t.Fatalf("formation error did not include the current spacing: %v", err)
	}
}

func smoothFixture(t *testing.T, scenario *tactical.Scenario) map[string]tactical.SmoothedPath {
	t.Helper()
	out := make(map[string]tactical.SmoothedPath)
	for _, member := range scenario.Formation.Members {
		path, err := tactical.SearchMember(*scenario, member, tactical.SearchOptions{Neighbors: 8, HeuristicWeightMilli: 1000})
		if err != nil {
			t.Fatal(err)
		}
		slot, _ := tactical.SlotByID(*scenario, member.SlotID)
		smoothed, err := tactical.SmoothPath(tactical.EffectiveMap(scenario.Map), member, slot, *path, tactical.SmoothingOptions{})
		if err != nil {
			t.Fatal(err)
		}
		out[member.ID] = *smoothed
	}
	return out
}

func TestSimulationRespectsTurnAndSegmentConstraints(t *testing.T) {
	scenario := baseScenario(t)
	engine := newEngine(t, scenario)
	snapshot, err := engine.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range scenario.Formation.Members {
		trace := snapshot.Traces[member.ID]
		if len(trace.States) < 2 {
			t.Fatalf("member %s produced an empty trace", member.ID)
		}
		if err := tactical.ValidateTrace(member, snapshot.Smooth[member.ID], trace, tactical.SimulationOptions{}); err != nil {
			t.Fatalf("member %s trace failed validation: %v", member.ID, err)
		}
		for i := 1; i < len(trace.States); i++ {
			delta := tactical.AngleDelta(trace.States[i-1].HeadingMilli, trace.States[i].HeadingMilli)
			if delta < 0 {
				delta = -delta
			}
			if delta > member.TurnRateMilli+1 {
				t.Fatalf("member %s turned %d in tick %d, limit %d", member.ID, delta, i, member.TurnRateMilli)
			}
			if trace.States[i].Segment < trace.States[i-1].Segment {
				t.Fatalf("member %s moved backwards at tick %d", member.ID, i)
			}
			if trace.States[i].Segment > trace.States[i-1].Segment+1 {
				t.Fatalf("member %s skipped a segment at tick %d", member.ID, i)
			}
		}
	}
}

func TestSimulationParameterAndTraceErrors(t *testing.T) {
	scenario := baseScenario(t)
	badStep := cloneScenario(t, scenario)
	badStep.Formation.Members[0].StepMilli = 0
	engine := newEngine(t, badStep)
	_, err := engine.FullRebuild(context.Background())
	requireCodeField(t, err, tactical.CodePara, "")

	badTurn := cloneScenario(t, scenario)
	badTurn.Formation.Members[0].TurnRateMilli = 9999
	engine = newEngine(t, badTurn)
	_, err = engine.FullRebuild(context.Background())
	requireCodeField(t, err, tactical.CodePara, "")

	engine = newEngine(t, scenario)
	snapshot, err := engine.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	member, _ := tactical.MemberByID(*scenario, "A-LEAD")
	trace := snapshot.Traces[member.ID]
	trace.States[1].Position.Y += 5000
	err = tactical.ValidateTrace(member, snapshot.Smooth[member.ID], trace, tactical.SimulationOptions{})
	requireCodeField(t, err, tactical.CodeTrace, "")
	typed, _ := tactical.AsError(err)
	if typed.Tick != 1 || typed.MemberID != member.ID {
		t.Fatalf("trace error lacks tick/member context: %v", err)
	}
}

func TestParallelAndSequentialSimulationAreIdentical(t *testing.T) {
	scenario := baseScenario(t)
	sequential, err := tactical.NewEngine(*scenario, tactical.EngineOptions{
		Search: tactical.SearchOptions{Neighbors: 8, HeuristicWeightMilli: 1000},
		Smoothing: tactical.SmoothingOptions{
			MaxSpacingMilli: scenario.Formation.MaxSpacingMilli, FormationSamples: 65,
		},
		MaxParallel: 1, StateBufferLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := tactical.NewEngine(*scenario, tactical.EngineOptions{
		Search: tactical.SearchOptions{Neighbors: 8, HeuristicWeightMilli: 1000},
		Smoothing: tactical.SmoothingOptions{
			MaxSpacingMilli: scenario.Formation.MaxSpacingMilli, FormationSamples: 65,
		},
		MaxParallel: 4, StateBufferLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	sequentialSnapshot, err := sequential.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parallelSnapshot, err := parallel.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tactical.CompareSnapshots(sequentialSnapshot, parallelSnapshot); err != nil {
		t.Fatalf("parallel simulation diverged from sequential: %v", err)
	}
	if parallelSnapshot.Stats.MaxParallel != 4 {
		t.Fatalf("parallel budget was not recorded: %+v", parallelSnapshot.Stats)
	}
}

func newEngine(t *testing.T, scenario *tactical.Scenario) *tactical.Engine {
	t.Helper()
	engine, err := tactical.NewEngine(*scenario, tactical.EngineOptions{
		Search: tactical.SearchOptions{Neighbors: 8, HeuristicWeightMilli: 1000},
		Smoothing: tactical.SmoothingOptions{
			MaxSpacingMilli: scenario.Formation.MaxSpacingMilli, FormationSamples: 65,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

package sample

import (
	"context"
	"fmt"
	"math/rand"
	"sort"

	"tacticalworkbench/pkg/tactical"
)

const (
	DefaultSeed   int64 = 20261124
	DefaultWidth        = 36
	DefaultHeight       = 22
)

// Generate builds a deterministic local scenario with analytic ground truth.
func Generate(seed int64) (*tactical.Scenario, error) {
	scenario, err := GenerateBase(seed)
	if err != nil {
		return nil, err
	}
	snapshot, err := SnapshotForScenario(scenario)
	if err != nil {
		return nil, err
	}
	if err := PopulateTruth(scenario, snapshot); err != nil {
		return nil, err
	}
	if err := tactical.ValidateScenario(scenario); err != nil {
		return nil, err
	}
	return scenario, nil
}

// GenerateBase builds the deterministic map and formation without requiring
// an already valid formation-coupled trace.
func GenerateBase(seed int64) (*tactical.Scenario, error) {
	rng := rand.New(rand.NewSource(seed))
	width, height := DefaultWidth, DefaultHeight
	cells := make([]tactical.Cell, width*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			inA := y >= 2 && y <= 8
			inB := y >= 13 && y <= 19
			cell := tactical.Cell{Terrain: "marsh", Cost: int64(5 + rng.Intn(7)), Passable: true}
			switch {
			case inA || inB:
				cell.Terrain = "road"
				cell.Cost = 1
			case y >= 9 && y <= 12:
				cell.Terrain = "urban"
				cell.Cost = int64(2 + rng.Intn(5))
			default:
				if rng.Intn(3) == 0 {
					cell.Terrain = "forest"
				}
			}
			cells[y*width+x] = cell
		}
	}
	for _, point := range []tactical.GridPoint{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 0, Y: 1}, {X: 1, Y: 1},
		{X: 34, Y: 20}, {X: 35, Y: 20}, {X: 34, Y: 21}, {X: 35, Y: 21},
	} {
		idx := point.Y*width + point.X
		cells[idx].Passable = false
		cells[idx].BlockedBy = "base-barrier"
		cells[idx].Terrain = "wall"
		cells[idx].Cost = 0
	}
	def := tactical.MapDef{
		ID: "training-grid-local", Width: width, Height: height, Cells: cells,
		BlockedRegions: []tactical.BlockedRegion{
			{ID: "urban-central-lock", X: 14, Y: 9, Width: 4, Height: 4, Active: true},
			{ID: "bridge-lock", X: 18, Y: 2, Width: 2, Height: 3, Active: false},
			{ID: "south-yard-lock", X: 26, Y: 10, Width: 3, Height: 2, Active: true},
		},
	}

	members := []tactical.Member{
		{
			ID: "A-LEAD", Callsign: "Falcon", Group: "ALPHA", SlotID: "A-LEAD",
			Start: tactical.GridPoint{X: 2, Y: 5}, End: tactical.GridPoint{X: 33, Y: 5},
			StepMilli: 700, TurnRateMilli: 1100, HeuristicWeightMilli: 1000,
			Neighbors: 8, DependsOn: []string{},
		},
		{
			ID: "A-RIGHT", Callsign: "Viper", Group: "ALPHA", SlotID: "A-RIGHT",
			Start: tactical.GridPoint{X: 2, Y: 7}, End: tactical.GridPoint{X: 33, Y: 7},
			StepMilli: 650, TurnRateMilli: 950, HeuristicWeightMilli: 1000,
			Neighbors: 8, DependsOn: []string{"A-LEAD"},
		},
		{
			ID: "A-REAR", Callsign: "Kestrel", Group: "ALPHA", SlotID: "A-REAR",
			Start: tactical.GridPoint{X: 1, Y: 4}, End: tactical.GridPoint{X: 32, Y: 4},
			StepMilli: 600, TurnRateMilli: 900, HeuristicWeightMilli: 1000,
			Neighbors: 8, DependsOn: []string{"A-LEAD"},
		},
		{
			ID: "B-LEAD", Callsign: "Atlas", Group: "BRAVO", SlotID: "B-LEAD",
			Start: tactical.GridPoint{X: 2, Y: 16}, End: tactical.GridPoint{X: 33, Y: 16},
			StepMilli: 750, TurnRateMilli: 1200, HeuristicWeightMilli: 1000,
			Neighbors: 8, DependsOn: []string{},
		},
		{
			ID: "B-LEFT", Callsign: "Jackal", Group: "BRAVO", SlotID: "B-LEFT",
			Start: tactical.GridPoint{X: 2, Y: 14}, End: tactical.GridPoint{X: 33, Y: 14},
			StepMilli: 680, TurnRateMilli: 1000, HeuristicWeightMilli: 1000,
			Neighbors: 8, DependsOn: []string{"B-LEAD"},
		},
		{
			ID: "B-REAR", Callsign: "Osprey", Group: "BRAVO", SlotID: "B-REAR",
			Start: tactical.GridPoint{X: 3, Y: 17}, End: tactical.GridPoint{X: 34, Y: 17},
			StepMilli: 620, TurnRateMilli: 980, HeuristicWeightMilli: 1000,
			Neighbors: 8, DependsOn: []string{"B-LEAD"},
		},
	}
	formation := tactical.FormationDef{
		ID: "two-echelon", SlotVersion: "slots-v1",
		Slots: []tactical.Slot{
			{ID: "A-LEAD", Anchor: "A-LEAD", DX: 0, DY: 0, Tolerance: 2.5, FacingMilli: 0},
			{ID: "A-RIGHT", Anchor: "A-LEAD", DX: 0, DY: 2, Tolerance: 2.5, FacingMilli: 0},
			{ID: "A-REAR", Anchor: "A-LEAD", DX: -1, DY: -1, Tolerance: 2.5, FacingMilli: 0},
			{ID: "B-LEAD", Anchor: "B-LEAD", DX: 0, DY: 0, Tolerance: 2.5, FacingMilli: 0},
			{ID: "B-LEFT", Anchor: "B-LEAD", DX: 0, DY: -2, Tolerance: 2.5, FacingMilli: 0},
			{ID: "B-REAR", Anchor: "B-LEAD", DX: 1, DY: 1, Tolerance: 2.5, FacingMilli: 0},
		},
		Members: members, MaxSpacingMilli: 6000,
		DefaultEpsilon: 2500, PairToleranceMult: 2,
	}
	instructions := []tactical.Instruction{
		{Tick: 0, MemberID: "A-LEAD", Kind: "advance", Value: 1},
		{Tick: 0, MemberID: "B-LEAD", Kind: "advance", Value: 1},
		{Tick: 12, MemberID: "A-LEAD", Kind: "lane-hold", Value: 1},
		{Tick: 18, MemberID: "B-LEAD", Kind: "lane-hold", Value: 1},
		{Tick: 32, MemberID: "A-LEAD", Kind: "advance", Value: 2},
		{Tick: 36, MemberID: "B-LEAD", Kind: "advance", Value: 2},
	}
	scenario := &tactical.Scenario{
		SchemaVersion: 1, Seed: seed, Map: def, Formation: formation,
		Instructions: instructions,
		GroundTruth: tactical.GroundTruth{
			MinCosts: []tactical.CostTruth{}, PathLengths: []tactical.PathTruth{},
			Corners: []tactical.CornerTruth{}, FormationDeviations: []tactical.DeviationTruth{},
		},
	}
	if err := tactical.ValidateScenario(scenario); err != nil {
		return nil, err
	}
	return scenario, nil
}

func SnapshotForScenario(scenario *tactical.Scenario) (*tactical.Snapshot, error) {
	engine, err := tactical.NewEngine(*scenario, tactical.EngineOptions{
		Search: tactical.SearchOptions{Neighbors: 8, HeuristicWeightMilli: 1000},
		Smoothing: tactical.SmoothingOptions{
			MaxSpacingMilli:  scenario.Formation.MaxSpacingMilli,
			FormationSamples: 65,
		},
		Simulation: tactical.SimulationOptions{},
	})
	if err != nil {
		return nil, err
	}
	snapshot, err := engine.FullRebuild(context.Background())
	if err != nil {
		return nil, fmt.Errorf("sample full rebuild: %w", err)
	}
	return snapshot, nil
}

func PopulateTruth(scenario *tactical.Scenario, snapshot *tactical.Snapshot) error {
	truth := tactical.GroundTruth{}
	for _, member := range scenario.Formation.Members {
		path := snapshot.Paths[member.ID]
		smooth := snapshot.Smooth[member.ID]
		trace := snapshot.Traces[member.ID]
		truth.MinCosts = append(truth.MinCosts, tactical.CostTruth{
			MemberID: member.ID, From: member.Start, To: member.End, Cost: path.Cost,
		})
		length := int64(0)
		for i := 0; i < len(smooth.Corners)-1; i++ {
			length += tactical.DistanceMilli(smooth.Corners[i].Fixed, smooth.Corners[i+1].Fixed)
		}
		truth.PathLengths = append(truth.PathLengths, tactical.PathTruth{
			MemberID: member.ID, Cost: path.Cost, Steps: len(path.Cells) - 1,
			Corners: len(smooth.Corners), LengthMilli: length,
		})
		for _, corner := range smooth.Corners {
			truth.Corners = append(truth.Corners, tactical.CornerTruth{
				MemberID: member.ID, Grid: corner.Grid, World: corner.World,
			})
		}
		if len(trace.States) > 0 {
			ticks := []int{0, len(trace.States) / 2, len(trace.States) - 1}
			sort.Ints(ticks)
			last := -1
			for _, tick := range ticks {
				if tick == last {
					continue
				}
				last = tick
				truth.FormationDeviations = append(truth.FormationDeviations, tactical.DeviationTruth{
					Tick: tick, MemberID: member.ID,
					DeviationMilli: trace.States[tick].FormationDeviationMilli,
				})
			}
		}
	}
	scenario.GroundTruth = truth
	return nil
}

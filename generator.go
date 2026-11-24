package syncgame

import (
	"fmt"
	"sort"
)

type GeneratorOptions struct {
	Seed      uint64
	Players   int
	MaxTicks  int
	TickRate  int
	RenderFPS int
}

func DefaultGeneratorOptions() GeneratorOptions {
	return GeneratorOptions{
		Seed:      0x5eedc0de,
		Players:   4,
		MaxTicks:  180,
		TickRate:  30,
		RenderFPS: 30,
	}
}

// Generate creates the complete deterministic fixture. Truth is produced by
// running the same engine and client code used for validation; there is no
// hand-authored expected state.
func Generate(opts GeneratorOptions) (*Sample, error) {
	if opts.Players < 2 || opts.Players > 8 {
		return nil, tickErr(0, "*", "players", "generator supports 2..8 players")
	}
	if opts.MaxTicks <= 0 {
		return nil, tickErr(0, "*", "max_ticks", "max ticks must be positive")
	}
	if opts.TickRate <= 0 || opts.RenderFPS <= 0 {
		return nil, tickErr(0, "*", "rate", "tick rate and render FPS must be positive")
	}

	sample := &Sample{
		SchemaVersion:    SchemaVersion,
		AlgorithmVersion: AlgorithmVersion,
		Seed:             opts.Seed,
		TickRate:         opts.TickRate,
		RenderFPS:        opts.RenderFPS,
		MaxTicks:         opts.MaxTicks,
		Arena:            Arena{Width: 240, Height: 240},
		ResourceGoal:     120,
		LatencyTiers: []LatencyTier{
			{ID: "direct", ObservationDelayFrames: 0, MaxInputLatencyFrames: 1, MaxCorrectionFrames: 2, MaxConsecutiveRollbacks: 8, MaxRollbackDepth: 12},
			{ID: "low", ObservationDelayFrames: 2, MaxInputLatencyFrames: 3, MaxCorrectionFrames: 3, MaxConsecutiveRollbacks: 8, MaxRollbackDepth: 12},
			{ID: "medium", ObservationDelayFrames: 5, MaxInputLatencyFrames: 6, MaxCorrectionFrames: 4, MaxConsecutiveRollbacks: 8, MaxRollbackDepth: 16},
			{ID: "high", ObservationDelayFrames: 9, MaxInputLatencyFrames: 10, MaxCorrectionFrames: 5, MaxConsecutiveRollbacks: 8, MaxRollbackDepth: 20},
		},
	}

	names := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf", "Hotel"}
	corners := [][2]int{{20, 20}, {220, 20}, {20, 220}, {220, 220}, {120, 20}, {120, 220}, {20, 120}, {220, 120}}
	for i := 0; i < opts.Players; i++ {
		playerID := fmt.Sprintf("p%d", i+1)
		spawn := corners[i]
		// A tiny seed-dependent offset avoids identical fixtures for different
		// seeds while remaining fixed-point.
		spawn[0] += int(deriveUint64(opts.Seed, 0, playerID, "spawn-x") % 7)
		spawn[1] += int(deriveUint64(opts.Seed, 0, playerID, "spawn-y") % 7)
		sample.Units = append(sample.Units, UnitSeed{
			PlayerID:     playerID,
			DisplayName:  names[i],
			SpawnX:       spawn[0],
			SpawnY:       spawn[1],
			Direction:    0,
			Health:       100,
			MaxHealth:    100,
			Resource:     10 + i,
			MoveSpeed:    4,
			RespawnTicks: 20,
			Skills: []SkillDef{
				{ID: "pulse", Cost: 8, CooldownTicks: 6, Range: 300, Damage: 35},
				{ID: "mend", Cost: 6, CooldownTicks: 10, Range: 0, Heal: 24},
			},
		})
		sample.Clients = append(sample.Clients, ClientSeed{
			ID:          "c" + playerID[1:],
			PlayerID:    playerID,
			LatencyTier: LatencyTierEnum[(i%3)+1],
		})
	}

	sample.Intents = generateIntents(sample, opts.Seed)
	if err := ValidateSample(sample); err != nil {
		return nil, err
	}
	if err := FinalizeTruth(sample); err != nil {
		return nil, err
	}
	return sample, nil
}

func generateIntents(sample *Sample, seed uint64) []TickIntent {
	intents := make([]TickIntent, 0, sample.MaxTicks)
	centerX, centerY := sample.Arena.Width/2, sample.Arena.Height/2
	for tick := 0; tick < sample.MaxTicks; tick++ {
		ti := TickIntent{Tick: tick, Inputs: make([]Intent, 0, len(sample.Units))}
		for _, unit := range sample.Units {
			in := Intent{PlayerID: unit.PlayerID}
			phase := tick % 12
			switch {
			case tick < 10:
				in.Kind = IntentMove
				in.DX, in.DY = stepToward(centerX, centerY, unit.SpawnX, unit.SpawnY, tick)
			case tick%12 == 0 || tick%12 == 4 || tick%12 == 8:
				in.Kind = IntentGather
			case tick%12 == 2 && tick >= 14:
				in.Kind = IntentSkill
				in.Skill = "pulse"
				in.TargetID = chooseTarget(sample, unit.PlayerID, tick, seed)
			case tick%12 == 7 && tick >= 30:
				in.Kind = IntentItem
				if deriveUint64(seed, tick, unit.PlayerID, "item")%2 == 0 {
					in.Item = "medkit"
				} else {
					in.Item = "energy_cell"
				}
			case tick%12 == 10 && tick >= 50:
				in.Kind = IntentSkill
				in.Skill = "mend"
			default:
				in.Kind = IntentMove
				angle := int(deriveUint64(seed, tick, unit.PlayerID, "move-angle") % 8)
				in.DX, in.DY = directionVector(angle)
				// Pull wayward units gently toward the arena center.
				if tick%5 == 0 {
					in.DX = sign(centerX - int(deriveUint64(seed, tick, unit.PlayerID, "x")%uint64(sample.Arena.Width)))
					in.DY = sign(centerY - int(deriveUint64(seed, tick, unit.PlayerID, "y")%uint64(sample.Arena.Height)))
				}
			}
			// Keep an intent train for the first ten ticks even if a phase
			// transition would otherwise leave the unit idle.
			if phase == 0 && tick == 0 {
				in.Kind = IntentMove
			}
			ti.Inputs = append(ti.Inputs, in)
		}
		sort.Slice(ti.Inputs, func(i, j int) bool { return ti.Inputs[i].PlayerID < ti.Inputs[j].PlayerID })
		intents = append(intents, ti)
	}
	return intents
}

func stepToward(tx, ty, sx, sy, tick int) (int, int) {
	// The exact current coordinate is simulated by the engine, so use a fixed
	// scripted approach that reaches the central third of the arena.
	dx := sign(tx - (sx + tick*3))
	dy := sign(ty - (sy + tick*3))
	return dx, dy
}

func chooseTarget(sample *Sample, playerID string, tick int, seed uint64) string {
	candidates := make([]string, 0, len(sample.Units)-1)
	for _, u := range sample.Units {
		if u.PlayerID != playerID {
			candidates = append(candidates, u.PlayerID)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return candidates[int(deriveUint64(seed, tick, playerID, "target")%uint64(len(candidates)))]
}

func directionVector(i int) (int, int) {
	switch i % 8 {
	case 0:
		return 1, 0
	case 1:
		return 1, -1
	case 2:
		return 0, -1
	case 3:
		return -1, -1
	case 4:
		return -1, 0
	case 5:
		return -1, 1
	case 6:
		return 0, 1
	default:
		return 1, 1
	}
}

func sign(v int) int {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}
